package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRPCProxy is a mock implementation of RPCProxy for testing.
type mockRPCProxy struct {
	name string
}

func (m *mockRPCProxy) ProxyRPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error) {
	return &RPCProxyResponse{
		Event: req.Event,
	}, nil
}

func (m *mockRPCProxy) Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error) {
	return &AuthenticateProxyResponse{}, nil
}

func (m *mockRPCProxy) SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error) {
	return &SubscribeAclProxyResponse{}, nil
}

func (m *mockRPCProxy) OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error) {
	return &OnConnectedProxyResponse{}, nil
}

func (m *mockRPCProxy) OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error) {
	return &OnSubscribedProxyResponse{}, nil
}

func (m *mockRPCProxy) OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error) {
	return &OnUnsubscribedProxyResponse{}, nil
}

func (m *mockRPCProxy) OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error) {
	return &OnDisconnectedProxyResponse{}, nil
}

func (m *mockRPCProxy) Name() string {
	return m.name
}

func (m *mockRPCProxy) Close() error {
	return nil
}

func TestRouter_Add(t *testing.T) {
	r := NewRouter()
	p1 := &mockRPCProxy{name: "proxy1"}

	err := r.Add(p1, "chat.*", "*")
	require.NoError(t, err)

	// Test invalid pattern
	p2 := &mockRPCProxy{name: "proxy2"}
	err = r.Add(p2, "[invalid", "*")
	assert.Error(t, err)
}

func TestRouter_Match(t *testing.T) {
	r := NewRouter()
	p1 := &mockRPCProxy{name: "user-service"}
	p2 := &mockRPCProxy{name: "chat-service"}
	p3 := &mockRPCProxy{name: "default-service"}

	// Add routes
	require.NoError(t, r.Add(p1, "user.*", "*"))
	require.NoError(t, r.Add(p2, "chat.*", "send*"))
	require.NoError(t, r.Add(p3, "*", "*"))

	tests := []struct {
		name           string
		channel        string
		method         string
		expectedProxy  string
		shouldMatch    bool
	}{
		{
			name:          "user service get",
			channel:       "user.profile",
			method:        "get",
			expectedProxy: "user-service",
			shouldMatch:   true,
		},
		{
			name:          "user service set",
			channel:       "user.settings",
			method:        "set",
			expectedProxy: "user-service",
			shouldMatch:   true,
		},
		{
			name:          "chat service send message",
			channel:       "chat.messages",
			method:        "sendMessage",
			expectedProxy: "chat-service",
			shouldMatch:   true,
		},
		{
			name:          "chat service other method",
			channel:       "chat.messages",
			method:        "getHistory",
			expectedProxy: "default-service",
			shouldMatch:   true,
		},
		{
			name:          "default service fallback",
			channel:       "other.channel",
			method:        "anyMethod",
			expectedProxy: "default-service",
			shouldMatch:   true,
		},
		{
			name:        "no match when router empty",
			channel:     "any.channel",
			method:      "anyMethod",
			shouldMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.shouldMatch {
				p := r.Match(tt.channel, tt.method)
				require.NotNil(t, p)
				assert.Equal(t, tt.expectedProxy, p.Name())
			} else {
				// Create empty router for this test
				emptyRouter := NewRouter()
				p := emptyRouter.Match(tt.channel, tt.method)
				assert.Nil(t, p)
			}
		})
	}
}

func TestRouter_MatchOrder(t *testing.T) {
	r := NewRouter()
	p1 := &mockRPCProxy{name: "first"}
	p2 := &mockRPCProxy{name: "second"}

	// Add routes - first matching route should win
	require.NoError(t, r.Add(p1, "chat.*", "*"))
	require.NoError(t, r.Add(p2, "*", "*"))

	// Should match first route (more specific)
	p := r.Match("chat.messages", "anyMethod")
	assert.Equal(t, "first", p.Name())

	// Should match second route
	p = r.Match("other.channel", "anyMethod")
	assert.Equal(t, "second", p.Name())
}

func TestRouter_WildcardPatterns(t *testing.T) {
	p := &mockRPCProxy{name: "wildcard-proxy"}

	tests := []struct {
		name        string
		pattern     string
		channel     string
		shouldMatch bool
	}{
		{
			name:        "single star matches all",
			pattern:     "*",
			channel:     "anything",
			shouldMatch: true,
		},
		{
			name:        "dot star matches nested",
			pattern:     "chat.*",
			channel:     "chat.messages",
			shouldMatch: true,
		},
		{
			name:        "dot star matches multiple levels",
			pattern:     "chat.*",
			channel:     "chat.presence.online",
			shouldMatch: true,
		},
		{
			name:        "specific prefix",
			pattern:     "user.profile",
			channel:     "user.profile",
			shouldMatch: true,
		},
		{
			name:        "specific prefix no match",
			pattern:     "user.profile",
			channel:     "user.settings",
			shouldMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testRouter := NewRouter()
			require.NoError(t, testRouter.Add(p, tt.pattern, "*"))

			result := testRouter.Match(tt.channel, "anyMethod")
			if tt.shouldMatch {
				assert.NotNil(t, result)
			} else {
				assert.Nil(t, result)
			}
		})
	}
}

func TestRouter_AddFromConfig(t *testing.T) {
	r := NewRouter()
	p := &mockRPCProxy{name: "config-proxy"}

	cfg := &ProxyConfig{
		Name:     "test-proxy",
		Endpoint: "http://localhost:8001",
		Routes: []RouteConfig{
			{Channel: "user.*", Method: "*"},
			{Channel: "chat.*", Method: "send*"},
		},
	}

	err := r.AddFromConfig(p, cfg)
	require.NoError(t, err)

	// Test matches
	assert.Equal(t, "config-proxy", r.Match("user.profile", "get").Name())
	assert.Equal(t, "config-proxy", r.Match("chat.messages", "sendMessage").Name())
	assert.Nil(t, r.Match("other.channel", "anyMethod"))
}

func TestRouter_Close(t *testing.T) {
	r := NewRouter()
	p1 := &mockRPCProxy{name: "proxy1"}
	p2 := &mockRPCProxy{name: "proxy2"}

	require.NoError(t, r.Add(p1, "channel1", "*"))
	require.NoError(t, r.Add(p2, "channel2", "*"))

	err := r.Close()
	assert.NoError(t, err)

	// After close, routes should be cleared
	assert.Nil(t, r.Match("channel1", "method1"))
}
