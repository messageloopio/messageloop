package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
)

func newTestHTTPProxy(t *testing.T, server *httptest.Server) *HTTPProxy {
	t.Helper()
	p, err := NewHTTPProxy(&ProxyConfig{
		Name:     "test-http",
		Endpoint: server.URL,
		HTTP:     &HTTPProxyConfig{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestHTTPProxy_NotificationMethods_PassThroughBackendError(t *testing.T) {
	// The backend answers every notification endpoint with a non-empty Error field.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"NOTIFY_REJECTED","message":"backend rejected the notification"}}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	assertBackendError := func(t *testing.T, err *sharedpb.Error) {
		t.Helper()
		require.NotNil(t, err, "backend Error field must not be swallowed")
		assert.Equal(t, "NOTIFY_REJECTED", err.Code)
		assert.Equal(t, "backend rejected the notification", err.Message)
	}

	t.Run("OnConnected", func(t *testing.T) {
		resp, err := p.OnConnected(ctx, &OnConnectedProxyRequest{SessionID: "s-1", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})

	t.Run("OnSubscribed", func(t *testing.T) {
		resp, err := p.OnSubscribed(ctx, &OnSubscribedProxyRequest{SessionID: "s-1", Channel: "chat.a", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})

	t.Run("OnUnsubscribed", func(t *testing.T) {
		resp, err := p.OnUnsubscribed(ctx, &OnUnsubscribedProxyRequest{SessionID: "s-1", Channel: "chat.a", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})

	t.Run("OnDisconnected", func(t *testing.T) {
		resp, err := p.OnDisconnected(ctx, &OnDisconnectedProxyRequest{SessionID: "s-1", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})
}

func TestHTTPProxy_Authenticate_RequestCarriesSessionAndRemoteAddr(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	_, err := p.Authenticate(ctx, &AuthenticateProxyRequest{
		ClientID:   "client-1",
		Token:      "token-1",
		ClientType: "web",
		SessionID:  "session-1",
		RemoteAddr: "10.0.0.1:4321",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "client-1", body["client_id"])
	assert.Equal(t, "token-1", body["token"])
	assert.Equal(t, "web", body["client_type"])
	assert.Equal(t, "session-1", body["session_id"])
	assert.Equal(t, "10.0.0.1:4321", body["remote_addr"])
}

func TestHTTPProxy_SubscribeAcl_RequestCarriesUserAndSession(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	_, err := p.SubscribeAcl(ctx, &SubscribeAclProxyRequest{
		Channel:   "chat.private",
		Token:     "token-1",
		UserID:    "user-1",
		SessionID: "session-1",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "chat.private", body["channel"])
	assert.Equal(t, "token-1", body["token"])
	assert.Equal(t, "user-1", body["user_id"])
	assert.Equal(t, "session-1", body["session_id"])
}

func TestHTTPProxy_PublishAcl_RequestCarriesUserAndSession(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	_, err := p.PublishAcl(ctx, &PublishAclProxyRequest{
		Channel:   "chat.private",
		Token:     "token-1",
		UserID:    "user-1",
		SessionID: "session-1",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "chat.private", body["channel"])
	assert.Equal(t, "token-1", body["token"])
	assert.Equal(t, "user-1", body["user_id"])
	assert.Equal(t, "session-1", body["session_id"])
}
