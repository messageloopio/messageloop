package registryhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDirectory is an in-memory Registry+Discovery for handler tests.
type fakeDirectory struct {
	mu      sync.Mutex
	entries map[string]map[string]registry.Instance // service -> id -> instance

	regErr        error
	heartbeatErr  error
	deregisterErr error
	hbNotFoundIDs map[string]bool // "service/id" -> answer ErrNotRegistered
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{entries: map[string]map[string]registry.Instance{}}
}

func (f *fakeDirectory) Register(_ context.Context, inst registry.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.regErr != nil {
		return f.regErr
	}
	if inst.Name == "" || inst.ID == "" {
		return registry.ErrBadName
	}
	if f.entries[inst.Name] == nil {
		f.entries[inst.Name] = map[string]registry.Instance{}
	}
	f.entries[inst.Name][inst.ID] = inst
	return nil
}

func (f *fakeDirectory) Deregister(_ context.Context, serviceName, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deregisterErr != nil {
		return f.deregisterErr
	}
	delete(f.entries[serviceName], instanceID)
	return nil
}

func (f *fakeDirectory) Heartbeat(_ context.Context, serviceName, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heartbeatErr != nil {
		return f.heartbeatErr
	}
	if f.hbNotFoundIDs[serviceName+"/"+instanceID] || f.entries[serviceName][instanceID].ID == "" {
		return registry.ErrNotRegistered
	}
	return nil
}

func (f *fakeDirectory) GetService(_ context.Context, name string, filter registry.Filter) ([]registry.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []registry.Instance
	for _, inst := range f.entries[name] {
		if registry.MatchFilter(filter, inst) {
			out = append(out, inst)
		}
	}
	return out, nil
}

func (f *fakeDirectory) Watch(_ context.Context, _ string, _ registry.Filter) (registry.Watcher, error) {
	return nil, errors.New("watch not implemented in fake")
}

func (f *fakeDirectory) Close() error { return nil }

func newTestServer(t *testing.T) (*httptest.Server, *fakeDirectory) {
	t.Helper()
	f := newFakeDirectory()
	srv := httptest.NewServer(New(f, f))
	t.Cleanup(srv.Close)
	return srv, f
}

func do(t *testing.T, method, url string, body string) (*http.Response, string) {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, strings.TrimSpace(string(bodyBytes))
}

func TestRegisterAndList(t *testing.T) {
	srv, f := newTestServer(t)

	resp, body := do(t, http.MethodPost, srv.URL+"/registry/v1/services/acme-battle/instances",
		`{"id":"i-1","address":"10.0.0.7:9001","version":"v1.2.0","tags":["canary"],"meta":{"rack":"r1"}}`)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, body)

	resp, body = do(t, http.MethodGet, srv.URL+"/registry/v1/services/acme-battle", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		Instances []instanceView `json:"instances"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Instances, 1)
	inst := got.Instances[0]
	assert.Equal(t, "i-1", inst.ID)
	assert.Equal(t, "10.0.0.7:9001", inst.Address)
	assert.Equal(t, "v1.2.0", inst.Version)
	assert.Equal(t, []string{"canary"}, inst.Tags)
	assert.Equal(t, map[string]string{"rack": "r1"}, inst.Meta)

	// Stored shape: the directory entry the Resolver will consume.
	f.mu.Lock()
	stored := f.entries["acme-battle"]["i-1"]
	f.mu.Unlock()
	assert.Equal(t, registry.StatusPassing, stored.Status)
	require.Len(t, stored.Endpoints, 1)
	assert.Equal(t, registry.ProtocolGRPC, stored.Endpoints[0].Protocol)
	assert.Equal(t, "10.0.0.7:9001", stored.Endpoints[0].Address)
}

func TestRegisterValidation(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL + "/registry/v1/services/acme/instances"

	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad json", `{"id":`, "invalid JSON body"},
		{"missing id", `{"address":"h:1"}`, "id is required"},
		{"missing address", `{"id":"i-1"}`, "address is required"},
		{"bare port", `{"id":"i-1","address":":9001"}`, "must include a host"},
		{"not host port", `{"id":"i-1","address":"example.com"}`, "must be host:port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, http.MethodPost, base, tc.body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Contains(t, body, tc.want)
		})
	}
}

func TestRegisterBackendError(t *testing.T) {
	srv, f := newTestServer(t)
	f.mu.Lock()
	f.regErr = errors.New("redis down")
	f.mu.Unlock()

	resp, body := do(t, http.MethodPost, srv.URL+"/registry/v1/services/acme/instances", `{"id":"i-1","address":"h:1"}`)
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, body, "redis down")
}

func TestHeartbeat(t *testing.T) {
	srv, f := newTestServer(t)
	_, _ = do(t, http.MethodPost, srv.URL+"/registry/v1/services/acme/instances", `{"id":"i-1","address":"h:1"}`)

	resp, _ := do(t, http.MethodPut, srv.URL+"/registry/v1/services/acme/instances/i-1/heartbeat", "")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Unknown instance → 404, the re-register signal for registrants.
	resp, body := do(t, http.MethodPut, srv.URL+"/registry/v1/services/acme/instances/ghost/heartbeat", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, body, "not registered")

	// A directory failure is a 500, not a not-found.
	f.mu.Lock()
	f.heartbeatErr = errors.New("redis down")
	f.mu.Unlock()
	resp, body = do(t, http.MethodPut, srv.URL+"/registry/v1/services/acme/instances/i-1/heartbeat", "")
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, body, "redis down")
}

func TestDeregisterIdempotent(t *testing.T) {
	srv, _ := newTestServer(t)
	instance := srv.URL + "/registry/v1/services/acme/instances/i-1"

	_, _ = do(t, http.MethodPost, srv.URL+"/registry/v1/services/acme/instances", `{"id":"i-1","address":"h:1"}`)
	resp, _ := do(t, http.MethodDelete, instance, "")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Deleting a gone instance is still a 204.
	resp, _ = do(t, http.MethodDelete, instance, "")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)

	resp, body := do(t, http.MethodGet, srv.URL+"/registry/v1/services/acme", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"instances":[]}`, body)
}

func TestListEmptyService(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, body := do(t, http.MethodGet, srv.URL+"/registry/v1/services/nobody", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"instances":[]}`, body)
}
