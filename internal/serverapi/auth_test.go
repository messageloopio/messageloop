package serverapi

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/proxy"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// fakeAPIAuthProxy is a controllable proxy.Proxy for resolver tests: it
// counts AuthenticateAPIKey calls and answers from the configured fields.
type fakeAPIAuthProxy struct {
	mu         sync.Mutex
	calls      int
	lastAPIKey string
	identity   *proxy.APIKeyInfo
	respErr    *sharedv2.Error
	err        error
	block      chan struct{} // non-nil: AuthenticateAPIKey blocks until closed
}

func (f *fakeAPIAuthProxy) AuthenticateAPIKey(ctx context.Context, req *proxy.AuthenticateAPIKeyProxyRequest) (*proxy.AuthenticateAPIKeyProxyResponse, error) {
	f.mu.Lock()
	f.calls++
	f.lastAPIKey = req.APIKey
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &proxy.AuthenticateAPIKeyProxyResponse{Error: f.respErr, Identity: f.identity}, nil
}

func (f *fakeAPIAuthProxy) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAPIAuthProxy) RPC(context.Context, *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	return nil, nil
}
func (f *fakeAPIAuthProxy) Authenticate(context.Context, *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	return &proxy.AuthenticateProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) SubscribeAcl(context.Context, *proxy.SubscribeAclProxyRequest) (*proxy.SubscribeAclProxyResponse, error) {
	return &proxy.SubscribeAclProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) PublishAcl(context.Context, *proxy.PublishAclProxyRequest) (*proxy.PublishAclProxyResponse, error) {
	return &proxy.PublishAclProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) OnConnected(context.Context, *proxy.OnConnectedProxyRequest) (*proxy.OnConnectedProxyResponse, error) {
	return &proxy.OnConnectedProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) OnSubscribed(context.Context, *proxy.OnSubscribedProxyRequest) (*proxy.OnSubscribedProxyResponse, error) {
	return &proxy.OnSubscribedProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) OnUnsubscribed(context.Context, *proxy.OnUnsubscribedProxyRequest) (*proxy.OnUnsubscribedProxyResponse, error) {
	return &proxy.OnUnsubscribedProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) OnDisconnected(context.Context, *proxy.OnDisconnectedProxyRequest) (*proxy.OnDisconnectedProxyResponse, error) {
	return &proxy.OnDisconnectedProxyResponse{}, nil
}
func (f *fakeAPIAuthProxy) Name() string { return "fake-admin-auth" }
func (f *fakeAPIAuthProxy) Close() error { return nil }

// newTestResolver builds a resolver around the fake proxy with sane
// defaults; individual tests override via the returned fake.
func newTestResolver(t *testing.T, mutate func(*apiAuthOptions, *fakeAPIAuthProxy)) (*apiAuthResolver, *fakeAPIAuthProxy) {
	t.Helper()
	fake := &fakeAPIAuthProxy{}
	opts := apiAuthOptions{
		FindProxy: func() proxy.Proxy { return fake },
		Ceiling:   authz.CapHistoryRead | authz.CapSessionAct | authz.CapChannelsList,
		CacheTTL:  30 * time.Second,
	}
	if mutate != nil {
		mutate(&opts, fake)
	}
	return newAPIAuthResolver(opts), fake
}

func adminIdentityKeyID(presented string) string { return cacheKey(presented) }

// fullIdentity is the happy-path backend answer used by most tests.
func fullIdentity() *proxy.APIKeyInfo {
	return &proxy.APIKeyInfo{
		KeyID:        "key-42",
		Namespaces:   []string{"acme", "beta"},
		Capabilities: []string{"history.read", "session.act"},
	}
}

func TestAdminAuthResolver_VerifyCacheHit(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.identity = fullIdentity()
	})

	first, err := r.Verify(context.Background(), "sk-tenant-key-material-0001")
	require.NoError(t, err)
	second, err := r.Verify(context.Background(), "sk-tenant-key-material-0001")
	require.NoError(t, err)

	assert.Equal(t, 1, fake.callCount(), "the second verify must come from the cache")
	assert.Equal(t, first, second)
	assert.Equal(t, "key-42", second.KeyID)

	// A different key is a different cache entry and does hit the proxy.
	_, err = r.Verify(context.Background(), "sk-tenant-key-material-0002")
	require.NoError(t, err)
	assert.Equal(t, 2, fake.callCount())
}

func TestAdminAuthResolver_VerifyExpiry(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.identity = fullIdentity()
	})
	r.opts.CacheTTL = 30 * time.Millisecond

	key := "sk-expiring-key-material-0001"
	_, err := r.Verify(context.Background(), key)
	require.NoError(t, err)

	// Simulate the TTL elapsing instead of sleeping through it.
	entry := r.entries[adminIdentityKeyID(key)]
	entry.expireAt = time.Now().Add(-time.Millisecond)
	r.mu.Lock()
	r.entries[adminIdentityKeyID(key)] = entry
	r.mu.Unlock()

	_, err = r.Verify(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, 2, fake.callCount(), "an expired entry must trigger a fresh proxy call")
}

// TestAdminAuthResolver_VerifyMaxAgeClamp pins TTL = min(configured, max_age)
// with the 1s floor (design §2.3 / G8 relative max_age).
func TestAdminAuthResolver_VerifyMaxAgeClamp(t *testing.T) {
	cases := []struct {
		name       string
		cfgTTL     time.Duration
		maxAgeSec  int64
		wantMinTTL time.Duration // inclusive window around the expected TTL
		wantMaxTTL time.Duration
	}{
		{"max_age wins over 30s config", 30 * time.Second, 2, 2 * time.Second, 3 * time.Second},
		{"max_age 0 uses configured TTL", 2 * time.Second, 0, 2 * time.Second, 3 * time.Second},
		{"max_age 1s lands on the floor", 30 * time.Second, 1, 1 * time.Second, 1500 * time.Millisecond},
		{"sub-second config TTL floored at 1s", 100 * time.Millisecond, 0, 1 * time.Second, 1500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
				opts.CacheTTL = tc.cfgTTL
				f.identity = fullIdentity()
				f.identity.MaxAgeSeconds = tc.maxAgeSec
			})

			before := time.Now()
			_, err := r.Verify(context.Background(), "sk-max-age-key-material-001")
			require.NoError(t, err)

			entry := r.entries[adminIdentityKeyID("sk-max-age-key-material-001")]
			age := entry.expireAt.Sub(before)
			assert.GreaterOrEqual(t, age, tc.wantMinTTL, "effective TTL below the expected window")
			assert.LessOrEqual(t, age, tc.wantMaxTTL, "effective TTL above the expected window")
			assert.Equal(t, 1, fake.callCount())
		})
	}
}

func TestAdminAuthResolver_VerifyNegativeCache(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.respErr = &sharedv2.Error{Code: "INVALID_API_KEY", Type: "auth_error"}
	})

	key := "sk-rejected-key-material-0001"
	_, err := r.Verify(context.Background(), key)
	require.ErrorIs(t, err, errAPIKeyRejected)

	// The rejection is cached for negativeAuthCacheTTL (5s).
	entry := r.entries[adminIdentityKeyID(key)]
	require.False(t, entry.allowed)
	assert.InDelta(t, negativeAuthCacheTTL, time.Until(entry.expireAt), float64(time.Second))

	// The cached rejection answers again without a proxy round-trip.
	_, err = r.Verify(context.Background(), key)
	require.ErrorIs(t, err, errAPIKeyRejected)
	assert.Equal(t, 1, fake.callCount())

	// The negative error text never carries the presented credential.
	assert.NotContains(t, err.Error(), key)
}

// TestAdminAuthResolver_VerifyConcurrentDedup: concurrent verifies of the
// same key attach to one in-flight call — the proxy sees exactly one round
// trip (G9).
func TestAdminAuthResolver_VerifyConcurrentDedup(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.identity = fullIdentity()
		f.block = make(chan struct{})
	})

	const workers = 10
	var wg sync.WaitGroup
	results := make([]authz.APIIdentity, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = r.Verify(context.Background(), "sk-concurrent-key-material-1")
		}(i)
	}
	// Give the workers time to pile onto the in-flight call, then release.
	time.Sleep(50 * time.Millisecond)
	close(fake.block)
	wg.Wait()

	assert.Equal(t, 1, fake.callCount(), "concurrent verifies of one key must dedup to a single proxy call")
	for i := range results {
		require.NoError(t, errs[i])
		assert.Equal(t, "key-42", results[i].KeyID)
	}
}

// TestAdminAuthResolver_VerifyCapsClamp pins D17: granted capabilities are
// mapped onto the closed set (unknown names dropped with a WARN, all-unknown
// → zero) and AND-ed with the node ceiling.
func TestAdminAuthResolver_VerifyCapsClamp(t *testing.T) {
	cases := []struct {
		name     string
		caps     []string
		ceiling  authz.Capability
		wantCaps authz.Capability
	}{
		{"grant above ceiling intersects", []string{"history.read", "session.act", "channels.list"}, authz.CapHistoryRead, authz.CapHistoryRead},
		{"unknown names dropped", []string{"history.read", "brand.new.cap"}, authz.DefaultCapabilityCeiling, authz.CapHistoryRead},
		{"all unknown means zero", []string{"nope", "nada"}, authz.DefaultCapabilityCeiling, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
				opts.Ceiling = tc.ceiling
				f.identity = fullIdentity()
				f.identity.Capabilities = tc.caps
			})

			id, err := r.Verify(context.Background(), "sk-caps-clamp-key-material-1")
			require.NoError(t, err)
			assert.Equal(t, tc.wantCaps, id.Caps)
			assert.Equal(t, tc.wantCaps, id.Principal().Caps, "caps pass through Principal")
		})
	}
}

// TestAdminAuthResolver_VerifyNamespaceSanitize pins the four namespace
// cleaning cases: legal list, invalid entries dropped, "*" singleton, and
// "*" mixed with exact entries collapsing to zero scope.
func TestAdminAuthResolver_VerifyNamespaceSanitize(t *testing.T) {
	cases := []struct {
		name       string
		namespaces []string
		want       []string
	}{
		{"legal exact list kept", []string{"acme", "beta"}, []string{"acme", "beta"}},
		{"invalid entries dropped", []string{"Bad_NS", "acme", "-nope"}, []string{"acme"}},
		{"wildcard singleton", []string{"*"}, []string{"*"}},
		{"wildcard mixed with exact collapses to zero scope", []string{"*", "acme"}, []string{}},
		{"all invalid collapses to zero scope", []string{"UPPER", "under_score"}, []string{}},
		{"empty grant is zero scope (D5)", nil, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
				f.identity = fullIdentity()
				f.identity.Namespaces = tc.namespaces
			})

			id, err := r.Verify(context.Background(), "sk-namespace-key-material-1")
			require.NoError(t, err)
			assert.Equal(t, tc.want, id.Namespaces)

			// Zero-scope identities reject everything (fail-closed).
			if len(id.Namespaces) == 0 {
				assert.False(t, id.AllowsNamespace("acme"))
				assert.False(t, id.AllowsChannel("acme:chat.room1"))
			}
		})
	}
}

// TestAdminAuthResolver_ProxyErrorNegativeCache: a transport error enters
// the short 2s negative cache and answers repeats without a proxy call.
func TestAdminAuthResolver_ProxyErrorNegativeCache(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.err = fmt.Errorf("connection refused")
	})

	key := "sk-proxy-error-key-material-1"
	_, err := r.Verify(context.Background(), key)
	require.ErrorIs(t, err, errProxyUnavailable)

	entry := r.entries[adminIdentityKeyID(key)]
	require.False(t, entry.allowed)
	assert.InDelta(t, proxyErrorCacheTTL, time.Until(entry.expireAt), float64(time.Second))

	_, err = r.Verify(context.Background(), key)
	require.ErrorIs(t, err, errProxyUnavailable)
	assert.Equal(t, 1, fake.callCount(), "the error negative cache must absorb repeats")
	assert.NotContains(t, err.Error(), key)
}

// TestAdminAuthResolver_Breaker pins D27: five consecutive proxy errors
// open the breaker; while open, verifications fail fast without touching
// the proxy; once the cooldown elapses a fresh (successful) call closes it
// and clears the streak.
func TestAdminAuthResolver_Breaker(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.err = fmt.Errorf("connection refused")
	})

	key := "sk-breaker-key-material-0001"
	// Call 1 hits the proxy; calls 2-5 hit the 2s error negative cache (each
	// still counting toward the streak, so a request flood trips fast).
	for i := 0; i < breakerThreshold; i++ {
		_, err := r.Verify(context.Background(), key)
		require.ErrorIs(t, err, errProxyUnavailable)
	}
	assert.Equal(t, 1, fake.callCount(), "the negative cache must shield the proxy during the streak")
	require.False(t, r.breakerOpenAt.IsZero(), "the breaker must be open after %d consecutive errors", breakerThreshold)

	// Expire the error entry: the open breaker now fast-fails without a call.
	r.mu.Lock()
	entry := r.entries[adminIdentityKeyID(key)]
	entry.expireAt = time.Now().Add(-time.Millisecond)
	r.entries[adminIdentityKeyID(key)] = entry
	r.mu.Unlock()

	_, err := r.Verify(context.Background(), key)
	require.ErrorIs(t, err, errProxyUnavailable)
	assert.Equal(t, 1, fake.callCount(), "an open breaker must not touch the proxy")

	// Cooldown elapsed, proxy recovers: the next call goes through, the
	// breaker closes and the streak is cleared.
	fake.mu.Lock()
	fake.err = nil
	fake.identity = fullIdentity()
	fake.mu.Unlock()
	r.mu.Lock()
	r.breakerOpenAt = time.Now().Add(-breakerCooldown - time.Second)
	r.mu.Unlock()

	id, err := r.Verify(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, "key-42", id.KeyID)
	assert.Equal(t, 2, fake.callCount())
	assert.True(t, r.breakerOpenAt.IsZero(), "the breaker must be closed after recovery")
	assert.Equal(t, 0, r.consecutiveErrors)
}

func TestAdminAuthResolver_CapacityEviction(t *testing.T) {
	r, _ := newTestResolver(t, nil)

	// Fill the cache to the cap, then insert one more: the size stays at the
	// cap (a random entry was evicted) and the new entry is present.
	r.mu.Lock()
	for i := 0; i < maxAuthCacheEntries; i++ {
		r.entries[fmt.Sprintf("prekey-%04d", i)] = cacheEntry{allowed: false, expireAt: time.Now().Add(time.Hour)}
	}
	r.storeLocked("the-new-key", cacheEntry{allowed: true, expireAt: time.Now().Add(time.Hour)})
	r.mu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Len(t, r.entries, maxAuthCacheEntries)
	_, ok := r.entries["the-new-key"]
	assert.True(t, ok, "the most recent entry must survive the eviction")
}

// TestAdminInterceptor is the integration pass over the authentication
// interceptor: credential extraction, the static table, the length gate,
// the insecure path, the proxy path, and metric attribution.
func TestAdminInterceptor(t *testing.T) {
	const staticToken = "static-admin-token-0123456789"
	const proxyKey = "sk-tenant-proxy-key-material-001"
	const shortKey = "short-key"

	handler := func(ctx context.Context, req any) (any, error) {
		id, ok := APIIdentityFromContext(ctx)
		require.True(t, ok, "the handler context must carry the verified identity")
		return id, nil
	}
	// D28 order probe: a 3-char static token configured directly on the
	// resolver (bypassing config.Validate, which enforces ≥20) must still
	// authenticate — the static comparison runs before the length gate.
	const deliberatelyShortStaticToken = "tok"

	call := func(r *apiAuthResolver, values ...string) (authz.APIIdentity, error) {
		ctx := context.Background()
		if len(values) > 0 {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(values...))
		}
		resp, err := r.Interceptor()(ctx, nil, &googlegrpc.UnaryServerInfo{}, handler)
		if err != nil {
			return authz.APIIdentity{}, err
		}
		id, ok := resp.(authz.APIIdentity)
		require.True(t, ok)
		return id, nil
	}

	newMetrics := func() *prometheus.CounterVec {
		vec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_api_auth_requests_total"}, []string{"verifier", "key_id", "result"})
		// A throwaway registry per call: subtests each get their own
		// collector without colliding on the global registry.
		prometheus.NewRegistry().MustRegister(vec)
		return vec
	}

	t.Run("static token hit injects superadmin identity", func(t *testing.T) {
		counter := newMetrics()
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.AuthTokens = []string{staticToken}
			opts.FindProxy = func() proxy.Proxy { return f }
			opts.AuthRequests = counter
		})

		id, err := call(r, "authorization", "Bearer "+staticToken)
		require.NoError(t, err)
		assert.Equal(t, "static-token", id.KeyID)
		assert.Equal(t, []string{"*"}, id.Namespaces)
		assert.Equal(t, r.opts.Ceiling, id.Caps)
		assert.Equal(t, 0, fake.callCount(), "the static path must not touch the proxy")
		assert.Equal(t, float64(1), testutil.ToFloat64(counter.WithLabelValues("static", "static-token", "allow")))
	})

	t.Run("short static token not killed by the length gate (D28)", func(t *testing.T) {
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.AuthTokens = []string{deliberatelyShortStaticToken}
			opts.FindProxy = func() proxy.Proxy { return f }
		})

		id, err := call(r, "authorization", "Bearer "+deliberatelyShortStaticToken)
		require.NoError(t, err)
		assert.Equal(t, "static-token", id.KeyID)
		assert.Equal(t, 0, fake.callCount())
	})

	t.Run("static token miss falls through to the proxy", func(t *testing.T) {
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.AuthTokens = []string{staticToken}
			opts.FindProxy = func() proxy.Proxy { return f }
			f.identity = fullIdentity()
		})

		id, err := call(r, "authorization", "Bearer "+proxyKey)
		require.NoError(t, err)
		assert.Equal(t, "key-42", id.KeyID)
		assert.Equal(t, 1, fake.callCount())
	})

	t.Run("x-api-key fallback", func(t *testing.T) {
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.FindProxy = func() proxy.Proxy { return f }
			f.identity = fullIdentity()
		})

		id, err := call(r, "x-api-key", proxyKey)
		require.NoError(t, err)
		assert.Equal(t, "key-42", id.KeyID)
		assert.Equal(t, proxyKey, fake.lastAPIKey, "the presented key must reach the proxy backend")
	})

	t.Run("bearer preferred over x-api-key", func(t *testing.T) {
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.AuthTokens = []string{staticToken}
			opts.FindProxy = func() proxy.Proxy { return f }
		})

		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"authorization", "Bearer "+staticToken,
			"x-api-key", proxyKey,
		))
		resp, err := r.Interceptor()(ctx, nil, &googlegrpc.UnaryServerInfo{}, handler)
		require.NoError(t, err)
		id := resp.(authz.APIIdentity)
		assert.Equal(t, "static-token", id.KeyID, "authorization Bearer must win over x-api-key")
		assert.Equal(t, 0, fake.callCount())
	})

	t.Run("non-bearer authorization falls back to x-api-key", func(t *testing.T) {
		r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.FindProxy = func() proxy.Proxy { return f }
			f.identity = fullIdentity()
		})

		id, err := call(r, "authorization", "Token "+proxyKey, "x-api-key", proxyKey)
		require.NoError(t, err)
		assert.Equal(t, "key-42", id.KeyID)
	})

	t.Run("length gate rejects short keys without touching the proxy (G9)", func(t *testing.T) {
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.FindProxy = func() proxy.Proxy { return f }
		})

		_, err := call(r, "authorization", "Bearer "+shortKey)
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
		assert.Equal(t, 0, fake.callCount())

		// The rejection is negatively cached.
		r.mu.Lock()
		entry := r.entries[adminIdentityKeyID(shortKey)]
		r.mu.Unlock()
		require.False(t, entry.allowed)
		assert.NotContains(t, status.Convert(err).Message(), shortKey, "error messages must not carry the presented key")
	})

	t.Run("no credential with allow_insecure injects the insecure identity", func(t *testing.T) {
		counter := newMetrics()
		r, fake := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.AllowInsecure = true
			opts.FindProxy = func() proxy.Proxy { return f }
			opts.AuthRequests = counter
		})

		id, err := call(r)
		require.NoError(t, err)
		assert.Equal(t, "insecure", id.KeyID)
		assert.Equal(t, []string{"*"}, id.Namespaces)
		assert.Equal(t, r.opts.Ceiling, id.Caps)
		assert.Equal(t, 0, fake.callCount())
		assert.Equal(t, float64(1), testutil.ToFloat64(counter.WithLabelValues("insecure", "insecure", "allow")))
	})

	t.Run("no credential with tokens configured is rejected", func(t *testing.T) {
		r, _ := newTestResolver(t, func(opts *apiAuthOptions, _ *fakeAPIAuthProxy) {
			opts.AuthTokens = []string{staticToken}
		})

		_, err := call(r)
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("no credential and nothing configured is rejected (fail-closed)", func(t *testing.T) {
		r, _ := newTestResolver(t, nil)

		_, err := call(r)
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("credential with no proxy assignment is rejected", func(t *testing.T) {
		r, _ := newTestResolver(t, func(opts *apiAuthOptions, _ *fakeAPIAuthProxy) {
			opts.FindProxy = nil
		})

		_, err := call(r, "x-api-key", proxyKey)
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("proxy outage surfaces a distinct unavailable message", func(t *testing.T) {
		r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.FindProxy = func() proxy.Proxy { return f }
			f.err = fmt.Errorf("dial timeout")
		})

		_, err := call(r, "x-api-key", proxyKey)
		require.Error(t, err)
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "unavailable")
		assert.NotContains(t, status.Convert(err).Message(), proxyKey)
	})

	t.Run("deny metrics carry the proxy verifier and unknown key", func(t *testing.T) {
		counter := newMetrics()
		r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.FindProxy = func() proxy.Proxy { return f }
			opts.AuthRequests = counter
			f.respErr = &sharedv2.Error{Code: "INVALID_API_KEY", Type: "auth_error"}
		})

		_, err := call(r, "x-api-key", proxyKey)
		require.Error(t, err)
		assert.Equal(t, float64(1), testutil.ToFloat64(counter.WithLabelValues("proxy", "unknown", "deny")))
	})

	t.Run("rpc counter attributes method, key, and handler outcome (G7)", func(t *testing.T) {
		const method = "/messageloop.server.v2.APIService/GetPresence"
		rpcVec := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_admin_rpc_total"}, []string{"method", "key_id", "result"})
		prometheus.NewRegistry().MustRegister(rpcVec)

		// Authenticated call whose handler succeeds → ok under the key's ID.
		r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
			opts.AuthTokens = []string{staticToken}
			opts.FindProxy = func() proxy.Proxy { return f }
			opts.RPCs = rpcVec
		})
		authedCtx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Bearer "+staticToken))

		// Authenticated call whose handler succeeds → ok under the key's ID.
		_, err := r.Interceptor()(authedCtx, nil,
			&googlegrpc.UnaryServerInfo{FullMethod: method}, handler)
		require.NoError(t, err)
		assert.Equal(t, float64(1), testutil.ToFloat64(rpcVec.WithLabelValues(method, "static-token", "ok")))

		// Authenticated call whose handler fails → error, still attributed.
		failing := func(ctx context.Context, req any) (any, error) {
			return nil, status.Error(codes.InvalidArgument, "boom")
		}
		_, err = r.Interceptor()(authedCtx, nil,
			&googlegrpc.UnaryServerInfo{FullMethod: method}, failing)
		require.Error(t, err)
		assert.Equal(t, float64(1), testutil.ToFloat64(rpcVec.WithLabelValues(method, "static-token", "error")))

		// Authentication rejection → denied, never reaching the handler.
		_, err = r.Interceptor()(context.Background(), nil,
			&googlegrpc.UnaryServerInfo{FullMethod: method}, handler)
		require.Error(t, err)
		assert.Equal(t, float64(1), testutil.ToFloat64(rpcVec.WithLabelValues(method, "unknown", "denied")))
	})
}

// TestAdminIdentityContext pins the exported context helpers used by later
// handler-side phases.
func TestAdminIdentityContext(t *testing.T) {
	id := authz.APIIdentity{KeyID: "key-7", Namespaces: []string{"acme"}, Caps: authz.CapHistoryRead}

	ctx := WithAPIIdentity(context.Background(), id)
	got, ok := APIIdentityFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, id, got)

	_, ok = APIIdentityFromContext(context.Background())
	assert.False(t, ok)
}

// TestCredentialExtractionNeverLogsPlaintext walks every rejection path and
// asserts the returned error strings never embed the presented credential.
func TestCredentialExtractionNeverLogsPlaintext(t *testing.T) {
	secrets := []string{"sk-secret-credential-material-1", "another-secret-value"}
	r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
		opts.FindProxy = func() proxy.Proxy { return f }
		opts.AuthTokens = []string{"static-admin-token-0123456789"}
		f.err = fmt.Errorf("backend exploded")
	})

	var probes []error
	// Length gate.
	_, err := r.Interceptor()(metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", secrets[0])),
		nil, &googlegrpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { return nil, nil })
	probes = append(probes, err)
	// Proxy transport error.
	r.mu.Lock()
	delete(r.entries, adminIdentityKeyID(secrets[1]))
	r.mu.Unlock()
	_, err = r.Interceptor()(metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", secrets[1])),
		nil, &googlegrpc.UnaryServerInfo{}, func(context.Context, any) (any, error) { return nil, nil })
	probes = append(probes, err)
	// Verify-level rejections.
	for _, secret := range secrets {
		r.mu.Lock()
		delete(r.entries, adminIdentityKeyID(secret))
		r.mu.Unlock()
		_, verr := r.Verify(context.Background(), secret)
		probes = append(probes, verr)
	}

	for _, err := range probes {
		require.Error(t, err)
		for _, secret := range secrets {
			assert.False(t, strings.Contains(err.Error(), secret))
		}
	}
}

// TestResolverMetricsNilSafe: a resolver without a counter must not panic.
func TestResolverMetricsNilSafe(t *testing.T) {
	r, _ := newTestResolver(t, func(opts *apiAuthOptions, f *fakeAPIAuthProxy) {
		opts.FindProxy = func() proxy.Proxy { return f }
		f.identity = fullIdentity()
	})
	require.Nil(t, r.opts.AuthRequests)

	_, err := r.Verify(context.Background(), "sk-nil-metrics-key-material-1")
	require.NoError(t, err)
}

// TestEvictionAndDedupUnderRapidReuse is a light concurrency smoke over the
// shared mutable state (run with -race in CI).
func TestEvictionAndDedupUnderRapidReuse(t *testing.T) {
	r, fake := newTestResolver(t, func(_ *apiAuthOptions, f *fakeAPIAuthProxy) {
		f.identity = fullIdentity()
	})

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("sk-race-key-material-%04d", i%4)
			_, _ = r.Verify(context.Background(), key)
		}(i)
	}
	wg.Wait()
	t.Logf("proxy calls: %d (4 distinct keys, dedup may merge further)", fake.callCount())
}
