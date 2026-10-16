package registrant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeServer is a configurable stand-in for the MessageLoop registry API.
type fakeServer struct {
	mu         sync.Mutex
	srv        *httptest.Server
	posts      int
	beats      int
	deletes    int
	lastPost   map[string]any
	authHeader string
	postStatus int // zero means 204
	beatStatus int // zero means 204
	failBeats  int // fail the first N heartbeats with 500
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /registry/v1/services/{service}/instances", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.posts++
		if f.postStatus == 0 {
			f.postStatus = http.StatusNoContent
		}
		status := f.postStatus
		body := make(map[string]any)
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastPost = body
		f.authHeader = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.WriteHeader(status)
	})
	mux.HandleFunc("PUT /registry/v1/services/{service}/instances/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.beats++
		beat := f.beats
		if f.beatStatus == 0 {
			f.beatStatus = http.StatusNoContent
		}
		status := f.beatStatus
		fail := beat <= f.failBeats
		f.authHeader = r.Header.Get("Authorization")
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(status)
	})
	mux.HandleFunc("DELETE /registry/v1/services/{service}/instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletes++
		f.authHeader = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) counts() (posts, beats, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.beats, f.deletes
}

func testOptions(url string) Options {
	return Options{
		ServerURL:         url,
		Service:           "acme-battle",
		ID:                "battle-1",
		Address:           "10.0.0.7:9001",
		Version:           "v1",
		Tags:              []string{"canary"},
		Meta:              map[string]string{"rack": "r1"},
		HeartbeatInterval: 10 * time.Millisecond,
	}
}

func TestRegisterHeartbeatCloseLifecycle(t *testing.T) {
	f := newFakeServer(t)
	reg, err := Register(context.Background(), testOptions(f.srv.URL))
	require.NoError(t, err)

	// The register body carries the instance attributes verbatim.
	f.mu.Lock()
	body := f.lastPost
	f.mu.Unlock()
	assert.Equal(t, "battle-1", body["id"])
	assert.Equal(t, "10.0.0.7:9001", body["address"])
	assert.Equal(t, "v1", body["version"])
	assert.Equal(t, []any{"canary"}, body["tags"])
	assert.Equal(t, map[string]any{"rack": "r1"}, body["meta"])

	// Heartbeats renew the entry while the registration lives.
	require.Eventually(t, func() bool {
		_, beats, _ := f.counts()
		return beats >= 3
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, reg.Close())
	require.NoError(t, reg.Close()) // idempotent

	posts, beats, deletes := f.counts()
	assert.Equal(t, 1, posts, "exactly one register")
	assert.Equal(t, 1, deletes, "exactly one deregister")

	// The stopped loop issues no further heartbeats.
	time.Sleep(3 * testOptions(f.srv.URL).HeartbeatInterval)
	_, beatsAfter, _ := f.counts()
	assert.Equal(t, beats, beatsAfter)

	assert.ErrorIs(t, reg.Healthy(), ErrClosed)
}

func TestRegisterFailFast(t *testing.T) {
	f := newFakeServer(t)
	f.mu.Lock()
	f.postStatus = http.StatusInternalServerError
	f.mu.Unlock()

	reg, err := Register(context.Background(), testOptions(f.srv.URL))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
	assert.Nil(t, reg)

	// No hidden retry: the caller owns the process fate.
	posts, beats, deletes := f.counts()
	assert.Equal(t, 1, posts)
	assert.Zero(t, beats)
	assert.Zero(t, deletes)
}

func TestHeartbeatNotFoundReregisters(t *testing.T) {
	f := newFakeServer(t)
	f.mu.Lock()
	f.beatStatus = http.StatusNotFound
	f.mu.Unlock()

	reg, err := Register(context.Background(), testOptions(f.srv.URL))
	require.NoError(t, err)

	// Each expired beat triggers a re-register under the same ID, so the
	// registration keeps reporting healthy.
	require.Eventually(t, func() bool {
		posts, _, _ := f.counts()
		return posts >= 3
	}, 2*time.Second, 5*time.Millisecond)
	assert.NoError(t, reg.Healthy())

	require.NoError(t, reg.Close())
}

func TestHeartbeatNotFoundWithoutReregister(t *testing.T) {
	f := newFakeServer(t)
	f.mu.Lock()
	f.beatStatus = http.StatusNotFound
	f.mu.Unlock()

	opts := testOptions(f.srv.URL)
	opts.DisableReregister = true
	reg, err := Register(context.Background(), opts)
	require.NoError(t, err)

	// No re-register: posts stay at the initial one and the health surface
	// turns not-found after heartbeatFailLimit beats.
	require.Eventually(t, func() bool {
		posts, _, _ := f.counts()
		return posts == 1
	}, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return errors.Is(reg.Healthy(), ErrNotFound)
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, reg.Close())
}

func TestHealthyRecoversAfterLimit(t *testing.T) {
	f := newFakeServer(t)
	f.mu.Lock()
	f.failBeats = 5 // first five beats fail with 500, then recover
	f.mu.Unlock()

	reg, err := Register(context.Background(), testOptions(f.srv.URL))
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return reg.Healthy() != nil
	}, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		return reg.Healthy() == nil
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, reg.Close())
}

func TestTokenHeader(t *testing.T) {
	f := newFakeServer(t)
	opts := testOptions(f.srv.URL)
	opts.Token = "sekrit"
	reg, err := Register(context.Background(), opts)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, beats, _ := f.counts()
		return beats >= 1
	}, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, reg.Close())

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, "Bearer sekrit", f.authHeader)
}

func TestOptionsValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"missing server url", func(o *Options) { o.ServerURL = "" }, "ServerURL is required"},
		{"bad scheme", func(o *Options) { o.ServerURL = "acme-admin:8080" }, "must be an http(s) URL"},
		{"missing service", func(o *Options) { o.Service = "" }, "Service is required"},
		{"missing id", func(o *Options) { o.ID = "" }, "ID is required"},
		{"missing address", func(o *Options) { o.Address = "" }, "Address is required"},
		{"bare port", func(o *Options) { o.Address = ":9001" }, "must include a host"},
		{"not host:port", func(o *Options) { o.Address = "example.com" }, "must be host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{ServerURL: "http://ml:8080", Service: "s", ID: "i", Address: "h:1"}
			tc.mutate(&opts)
			reg, err := Register(context.Background(), opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Nil(t, reg)
		})
	}
}

func TestDefaultHeartbeatInterval(t *testing.T) {
	assert.Equal(t, 10*time.Second, DefaultHeartbeatInterval)
}
