package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/prometheus/client_golang/prometheus"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/messageloopio/messageloop/proxy"
)

// The admin API key authentication chain (design §2.3): a unary interceptor
// backed by adminAuthResolver. Credentials come from `authorization: Bearer`
// (preferred) or `x-api-key`; static auth_tokens are compared first (D28:
// before the length gate), and anything else is verified through the
// admin_auth-assigned proxy with a bounded cache, in-flight dedup, and a
// consecutive-error breaker (D27).
const (
	// staticTokenKeyID is the AdminIdentity.KeyID of a static auth_tokens hit.
	staticTokenKeyID = "static-token"
	// insecureKeyID is the AdminIdentity.KeyID of the allow_insecure path.
	insecureKeyID = "insecure"
	// unknownKeyID labels metrics/logs for credentials that could not be
	// attributed to an identity.
	unknownKeyID = "unknown"

	// defaultAdminAuthCacheTTL mirrors config.DefaultAdminAuthCacheTTL
	// (server.grpc_admin.admin_auth_cache_ttl, design D15).
	defaultAdminAuthCacheTTL = 30 * time.Second
	// negativeAuthCacheTTL is the fixed rejection cache TTL (not configurable).
	negativeAuthCacheTTL = 5 * time.Second
	// proxyErrorCacheTTL is the short negative cache for proxy transport
	// errors (D27 fail-fast: the outage must not turn fail-closed into a
	// per-request rpcTimeout hang).
	proxyErrorCacheTTL = 2 * time.Second
	// breakerThreshold is the consecutive proxy-error count that opens the
	// breaker; breakerCooldown is how long it stays open (D27).
	breakerThreshold = 5
	breakerCooldown  = 30 * time.Second
	// maxAuthCacheEntries bounds the combined positive/negative cache;
	// insertions past the cap evict a random entry (G9 key-spray limit).
	maxAuthCacheEntries = 1024
	// minProxyKeyChars is the proxy-path credential length gate (G9). Static
	// tokens are compared BEFORE this gate (D28) so a short configured static
	// token is never killed by it.
	minProxyKeyChars = 20
	// minCacheTTL floors the effective positive TTL (design §2.3: TTL =
	// min(config, max_age), never below 1s).
	minCacheTTL = 1 * time.Second
)

// Sentinel verification errors. Their text never includes the presented
// credential, and they let the interceptor distinguish "the key is bad"
// from "the verifier is unreachable".
var (
	errAdminKeyRejected = errors.New("admin api key rejected by verifier")
	errProxyUnavailable = errors.New("admin api key verifier unavailable")
)

// adminAuthOptions configures the admin auth chain, assembled from
// server.grpc_admin / proxy.admin_auth at PrepareAdminServer time.
type adminAuthOptions struct {
	// AuthTokens is the static superadmin token list (any match wins).
	AuthTokens []string
	// AllowInsecure serves the admin API unauthenticated (no credential →
	// insecure superadmin identity). G5 keeps it loopback-only via Validate.
	AllowInsecure bool
	// FindProxy returns the admin_auth-assigned proxy (nil when unassigned).
	FindProxy func() proxy.Proxy
	// Ceiling is the node capability upper bound every granted identity is
	// clamped against (D17).
	Ceiling authz.Capability
	// CacheTTL is the configured positive cache TTL (0 → default).
	CacheTTL time.Duration
	// AuthRequests is the admin_auth_requests_total counter (nil disables).
	AuthRequests *prometheus.CounterVec
}

// cacheEntry is one positive or negative cache record. The cache key is
// sha256(presented); the presented credential itself is never stored.
type cacheEntry struct {
	identity authz.AdminIdentity
	allowed  bool
	err      error // the rejection error for negative entries
	expireAt time.Time
}

// verifyCall is one in-flight proxy verification. Concurrent verifications
// of the same credential attach to the first call's done channel instead of
// dialing the proxy themselves (~25-line singleflight, no dependency).
type verifyCall struct {
	done     chan struct{}
	identity authz.AdminIdentity
	err      error
	allowed  bool
	expireAt time.Time
}

// adminAuthResolver verifies admin credentials against the static token
// list and the admin_auth-assigned proxy, caching every verdict (positive
// and negative) under the sha256 of the presented credential.
type adminAuthResolver struct {
	opts adminAuthOptions
	ttl  time.Duration

	mu                sync.Mutex
	entries           map[string]cacheEntry
	inFlight          map[string]*verifyCall
	consecutiveErrors int
	breakerOpenAt     time.Time // zero = closed
}

// newAdminAuthResolver builds the resolver. ttl <= 0 resolves to the 30s
// default (config server.grpc_admin.admin_auth_cache_ttl).
func newAdminAuthResolver(opts adminAuthOptions) *adminAuthResolver {
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = defaultAdminAuthCacheTTL
	}
	return &adminAuthResolver{
		opts:     opts,
		ttl:      ttl,
		entries:  make(map[string]cacheEntry),
		inFlight: make(map[string]*verifyCall),
	}
}

// cacheKey derives the cache key from the presented credential: the hex
// sha256 digest. The plaintext never enters the cache, logs, or errors.
func cacheKey(presented string) string {
	sum := sha256.Sum256([]byte(presented))
	return hex.EncodeToString(sum[:])
}

// Verify resolves one presented credential to a clamped AdminIdentity.
// Verdicts are cached (positive for ttl, rejections for 5s, proxy errors
// for 2s), same-credential concurrency is deduplicated, and consecutive
// proxy errors open a 30s breaker that fails fast without touching the
// proxy (D27). A returned error is one of errAdminKeyRejected /
// errProxyUnavailable (or context cancellation) and never carries the
// presented credential.
func (r *adminAuthResolver) Verify(ctx context.Context, presented string) (authz.AdminIdentity, error) {
	key := cacheKey(presented)
	now := time.Now()

	r.mu.Lock()
	// Cache hit (positive or negative) — including the 2s proxy-error
	// negative cache. A cached error still counts toward the breaker streak
	// so a request flood during an outage trips it fast (D27).
	if e, ok := r.entries[key]; ok {
		if now.Before(e.expireAt) {
			if e.allowed {
				r.mu.Unlock()
				return e.identity, nil
			}
			// A cached proxy error still feeds the breaker streak so a
			// request flood during an outage trips it fast (D27).
			if errors.Is(e.err, errProxyUnavailable) {
				r.recordProxyErrorLocked()
			}
			err := e.err
			r.mu.Unlock()
			return authz.AdminIdentity{}, err
		}
		delete(r.entries, key)
	}

	// Breaker open → fail fast without touching the proxy.
	if !r.breakerOpenAt.IsZero() {
		if now.Sub(r.breakerOpenAt) < breakerCooldown {
			r.mu.Unlock()
			return authz.AdminIdentity{}, errProxyUnavailable
		}
		// Cooldown elapsed: close the breaker and start a fresh streak.
		r.breakerOpenAt = time.Time{}
		r.consecutiveErrors = 0
	}

	// Same-credential in-flight dedup: attach to the leader's call.
	if call, ok := r.inFlight[key]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return authz.AdminIdentity{}, ctx.Err()
		}
		if call.allowed {
			return call.identity, nil
		}
		return authz.AdminIdentity{}, call.err
	}

	call := &verifyCall{done: make(chan struct{})}
	r.inFlight[key] = call
	r.mu.Unlock()

	identity, entryTTL, verdict, verdictErr := r.verifyViaProxy(ctx, presented)

	r.mu.Lock()
	call.identity = identity
	call.err = verdictErr
	call.allowed = verdict == verdictAllow
	call.expireAt = now.Add(verdict.resolveTTL(entryTTL))
	close(call.done)
	delete(r.inFlight, key)
	switch verdict {
	case verdictAllow:
		// The proxy answered definitively: the error streak and an open
		// breaker (whose cooldown has elapsed) reset.
		r.consecutiveErrors = 0
		r.breakerOpenAt = time.Time{}
		r.storeLocked(key, cacheEntry{allowed: true, identity: identity, expireAt: call.expireAt})
	case verdictDeny:
		r.consecutiveErrors = 0
		r.storeLocked(key, cacheEntry{allowed: false, err: errAdminKeyRejected, expireAt: now.Add(negativeAuthCacheTTL)})
	case verdictError:
		r.recordProxyErrorLocked()
		r.storeLocked(key, cacheEntry{allowed: false, err: errProxyUnavailable, expireAt: now.Add(proxyErrorCacheTTL)})
	}
	r.mu.Unlock()

	if verdict != verdictAllow {
		return authz.AdminIdentity{}, verdictErr
	}
	return identity, nil
}

// verdict classifies one proxy round-trip.
type verdict int

const (
	verdictAllow verdict = iota
	verdictDeny
	verdictError
)

// resolveTTL maps the verdict to its cache duration: allows use the
// effective positive TTL handed back by the clamp (config TTL clamped by
// max_age, floored at 1s), rejections the fixed negative TTL, proxy errors
// the short error TTL.
func (v verdict) resolveTTL(positive time.Duration) time.Duration {
	switch v {
	case verdictDeny:
		return negativeAuthCacheTTL
	case verdictError:
		return proxyErrorCacheTTL
	default:
		return positive
	}
}

// recordProxyErrorLocked bumps the consecutive-error streak and opens the
// breaker at the threshold. Callers must hold r.mu.
func (r *adminAuthResolver) recordProxyErrorLocked() {
	r.consecutiveErrors++
	if r.consecutiveErrors >= breakerThreshold {
		r.breakerOpenAt = time.Now()
		r.consecutiveErrors = 0
	}
}

// storeLocked inserts an entry, evicting a random one at the capacity cap
// (Go map iteration order is randomized, which is the "random eviction").
// Callers must hold r.mu.
func (r *adminAuthResolver) storeLocked(key string, e cacheEntry) {
	if len(r.entries) >= maxAuthCacheEntries {
		for k := range r.entries {
			delete(r.entries, k)
			break
		}
	}
	r.entries[key] = e
}

// rejectWithoutProxy inserts a negative cache entry without contacting the
// proxy (G9: the length gate stops credential spraying at the door).
func (r *adminAuthResolver) rejectWithoutProxy(presented string) {
	r.mu.Lock()
	r.storeLocked(cacheKey(presented), cacheEntry{
		allowed:  false,
		err:      errAdminKeyRejected,
		expireAt: time.Now().Add(negativeAuthCacheTTL),
	})
	r.mu.Unlock()
}

// verifyViaProxy performs one proxy round-trip and clamps the result. The
// returned TTL is the effective positive cache duration for this entry;
// verdictErr never embeds the presented credential.
func (r *adminAuthResolver) verifyViaProxy(ctx context.Context, presented string) (authz.AdminIdentity, time.Duration, verdict, error) {
	p := r.opts.FindProxy()
	if p == nil {
		// The interceptor checks the assignment before calling Verify; this
		// is the defensive path for a revoked assignment mid-flight.
		return authz.AdminIdentity{}, 0, verdictError, errProxyUnavailable
	}

	resp, err := p.AuthenticateAdmin(ctx, &proxy.AuthenticateAdminProxyRequest{
		APIKey:     presented,
		RemoteAddr: remoteAddrFromContext(ctx),
	})
	if err != nil {
		log.WarnContext(ctx, "admin api key verification failed: proxy unreachable",
			"proxy", p.Name(), "error", err.Error())
		return authz.AdminIdentity{}, 0, verdictError, errProxyUnavailable
	}
	// A backend decision (accept or reject) proves the proxy is healthy.
	if resp.Error != nil || resp.Identity == nil {
		errorCode := ""
		if resp.Error != nil {
			errorCode = resp.Error.Code
		}
		log.WarnContext(ctx, "admin api key rejected by verifier",
			"proxy", p.Name(), "key_id", unknownKeyID,
			"error_code", errorCode)
		return authz.AdminIdentity{}, 0, verdictDeny, errAdminKeyRejected
	}

	identity, positiveTTL := r.clampIdentity(ctx, p.Name(), resp.Identity)
	if identity.KeyID == "" {
		log.WarnContext(ctx, "proxy returned an admin identity without key_id; attributing as unknown",
			"proxy", p.Name())
		identity.KeyID = unknownKeyID
	}
	return identity, positiveTTL, verdictAllow, nil
}

// clampIdentity applies the server-side trust clamps (design §2.3, D17):
// capabilities map onto the closed set and are AND-ed with the node
// ceiling, namespaces pass the namespace grammar ("*" only as a singleton,
// unknown/invalid entries dropped with a WARN, empty result → zero-scope),
// and the positive TTL resolves to min(configured TTL, max_age) floored at
// 1s. It returns the clamped identity and the effective positive TTL.
func (r *adminAuthResolver) clampIdentity(ctx context.Context, proxyName string, info *proxy.AdminIdentityInfo) (authz.AdminIdentity, time.Duration) {
	caps := authz.Capability(0)
	known := 0
	for _, name := range info.Capabilities {
		bit, ok := authz.ClosedCapabilityNames[name]
		if !ok {
			// Version skew tolerance: unknown closed-set names are dropped
			// toward safety, never error.
			log.WarnContext(ctx, "dropping unknown admin capability name from proxy",
				"proxy", proxyName, "capability", name)
			continue
		}
		caps |= bit
		known++
	}
	if len(info.Capabilities) > 0 && known == 0 {
		log.WarnContext(ctx, "proxy returned no known admin capability names; identity gets zero capabilities",
			"proxy", proxyName)
	}
	caps &= r.opts.Ceiling

	namespaces := make([]string, 0, len(info.Namespaces))
	sawWildcard := false
	for _, ns := range info.Namespaces {
		if ns == "*" {
			sawWildcard = true
			continue
		}
		if err := topics.ValidateNamespace(ns); err != nil {
			log.WarnContext(ctx, "dropping invalid admin namespace from proxy",
				"proxy", proxyName, "error", err.Error())
			continue
		}
		namespaces = append(namespaces, ns)
	}
	switch {
	case len(info.Namespaces) == 0:
		// Fail-closed (D5): an empty grant is a zero-scope identity.
		namespaces = []string{}
		log.WarnContext(ctx, "proxy returned an admin identity with no namespaces; zero-scope identity",
			"proxy", proxyName, "key_id", info.KeyID)
	case sawWildcard && len(namespaces) == 0:
		namespaces = []string{"*"}
	case sawWildcard:
		// "*" mixed with exact entries is a backend misconfiguration: the
		// whole grant collapses to zero scope (fail-closed) rather than
		// silently widening to "*" or shrinking to the exact remainder.
		namespaces = []string{}
		log.WarnContext(ctx, "proxy returned \"*\" mixed with exact admin namespaces; collapsing to zero-scope identity",
			"proxy", proxyName, "key_id", info.KeyID)
	case len(namespaces) == 0:
		log.WarnContext(ctx, "all admin namespaces from proxy were invalid; zero-scope identity",
			"proxy", proxyName, "key_id", info.KeyID)
	}

	ttl := r.ttl
	if info.MaxAgeSeconds > 0 {
		if d := time.Duration(info.MaxAgeSeconds) * time.Second; d < ttl {
			ttl = d
		}
	}
	if ttl < minCacheTTL {
		ttl = minCacheTTL
	}

	return authz.AdminIdentity{
		KeyID:      info.KeyID,
		Namespaces: namespaces,
		Caps:       caps,
	}, ttl
}

// Interceptor returns the admin authentication unary interceptor. Order per
// design §2.3: extract credential (Bearer preferred, x-api-key fallback) →
// no-credential path (tokens configured → reject; allow_insecure → insecure
// identity; else reject) → static token comparison (constant-time, BEFORE
// the length gate, D28) → proxy path (assignment check → length gate →
// Verify).
func (r *adminAuthResolver) Interceptor() googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (any, error) {
		presented, found := credentialFromMetadata(ctx)
		if !found {
			if len(r.opts.AuthTokens) > 0 {
				r.observe("static", unknownKeyID, "deny")
				log.WarnContext(ctx, "admin api request rejected: missing credential",
					"verifier", "static", "key_id", unknownKeyID)
				return nil, status.Error(codes.Unauthenticated, "missing admin credential")
			}
			if r.opts.AllowInsecure {
				identity := authz.AdminIdentity{KeyID: insecureKeyID, Namespaces: []string{"*"}, Caps: r.opts.Ceiling}
				r.observe("insecure", insecureKeyID, "allow")
				log.WarnContext(ctx, "admin api request allowed WITHOUT authentication",
					"verifier", "insecure", "key_id", insecureKeyID)
				return handler(WithAdminIdentity(ctx, identity), req)
			}
			r.observe("static", unknownKeyID, "deny")
			log.WarnContext(ctx, "admin api request rejected: no credential and no authentication configured",
				"verifier", "static", "key_id", unknownKeyID)
			return nil, status.Error(codes.Unauthenticated, "missing admin credential")
		}

		// Static token list first — deliberately before the proxy length
		// gate (D28: a short static token must not be killed by that gate).
		for _, token := range r.opts.AuthTokens {
			// Constant-time so token timing cannot leak the match;
			// ConstantTimeCompare is length-safe (mismatched lengths → 0).
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1 {
				identity := authz.AdminIdentity{KeyID: staticTokenKeyID, Namespaces: []string{"*"}, Caps: r.opts.Ceiling}
				r.observe("static", staticTokenKeyID, "allow")
				return handler(WithAdminIdentity(ctx, identity), req)
			}
		}

		if r.opts.FindProxy == nil || r.opts.FindProxy() == nil {
			r.observe("proxy", unknownKeyID, "deny")
			log.WarnContext(ctx, "admin api credential rejected: no admin_auth proxy assigned",
				"key_id", unknownKeyID)
			return nil, status.Error(codes.Unauthenticated, "invalid admin credential")
		}

		// G9 length gate: short credentials never reach the proxy.
		if len(presented) < minProxyKeyChars {
			r.rejectWithoutProxy(presented)
			r.observe("proxy", unknownKeyID, "deny")
			log.WarnContext(ctx, "admin api credential rejected: below minimum length",
				"key_id", unknownKeyID)
			return nil, status.Error(codes.Unauthenticated, "invalid admin credential")
		}

		identity, err := r.Verify(ctx, presented)
		if err != nil {
			r.observe("proxy", unknownKeyID, "deny")
			log.WarnContext(ctx, "admin api credential rejected",
				"key_id", unknownKeyID, "reason", err.Error())
			if errors.Is(err, errProxyUnavailable) {
				return nil, status.Error(codes.Unauthenticated, "admin credential verification unavailable (verifier proxy unreachable)")
			}
			return nil, status.Error(codes.Unauthenticated, "invalid admin credential")
		}

		r.observe("proxy", identity.KeyID, "allow")
		log.DebugContext(ctx, "admin api request authenticated via proxy",
			"proxy", r.proxyName(), "key_id", identity.KeyID)
		return handler(WithAdminIdentity(ctx, identity), req)
	}
}

// proxyName returns the assigned proxy's name for logs ("unknown" when
// unassigned or unnamed).
func (r *adminAuthResolver) proxyName() string {
	if r.opts.FindProxy == nil {
		return unknownKeyID
	}
	p := r.opts.FindProxy()
	if p == nil || p.Name() == "" {
		return unknownKeyID
	}
	return p.Name()
}

// observe increments the admin_auth_requests_total counter (nil-safe).
func (r *adminAuthResolver) observe(verifier, keyID, result string) {
	if r.opts.AuthRequests == nil {
		return
	}
	r.opts.AuthRequests.WithLabelValues(verifier, keyID, result).Inc()
}

// credentialFromMetadata extracts the presented credential: the Bearer
// scheme of `authorization` wins; `x-api-key` is the fallback. An
// authorization header without the Bearer prefix is ignored in favor of
// x-api-key.
func credentialFromMetadata(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	const bearerPrefix = "Bearer "
	if values := md.Get("authorization"); len(values) > 0 {
		if len(values[0]) > len(bearerPrefix) && strings.HasPrefix(values[0], bearerPrefix) {
			return values[0][len(bearerPrefix):], true
		}
	}
	if values := md.Get("x-api-key"); len(values) > 0 && values[0] != "" {
		return values[0], true
	}
	return "", false
}

// remoteAddrFromContext returns the gRPC peer address for proxy audit logs
// (empty when unavailable, e.g. in tests).
func remoteAddrFromContext(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p != nil && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// adminIdentityContextKey is the unexported context key for the verified
// admin identity.
type adminIdentityContextKey struct{}

// WithAdminIdentity attaches the verified admin identity to the context for
// downstream handlers (the S3+ scope layer reads it from here).
func WithAdminIdentity(ctx context.Context, id authz.AdminIdentity) context.Context {
	return context.WithValue(ctx, adminIdentityContextKey{}, id)
}

// AdminIdentityFromContext returns the verified admin identity previously
// attached by WithAdminIdentity, and whether one is present.
func AdminIdentityFromContext(ctx context.Context) (authz.AdminIdentity, bool) {
	id, ok := ctx.Value(adminIdentityContextKey{}).(authz.AdminIdentity)
	return id, ok
}
