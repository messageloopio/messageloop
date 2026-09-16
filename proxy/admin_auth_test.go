package proxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// Compile-time interface completeness: both transports must keep implementing
// the full Proxy surface, including AuthenticateAdmin.
var (
	_ Proxy = (*GRPCProxy)(nil)
	_ Proxy = (*HTTPProxy)(nil)
)

// AuthenticateAdmin implements Proxy.AuthenticateAdmin for the router test
// mock (mockRPCProxy, declared in router_test.go). Go allows a method on a
// package type to live in any file of the package, so the interface extension
// lands here and the existing test files stay untouched.
func (m *mockRPCProxy) AuthenticateAdmin(ctx context.Context, req *AuthenticateAdminProxyRequest) (*AuthenticateAdminProxyResponse, error) {
	return &AuthenticateAdminProxyResponse{}, nil
}

// --- gRPC contract ---

// fakeAdminAuthServer is a ProxyService fake for the AuthenticateAdmin
// contract: it records the request it received and answers from a handler.
type fakeAdminAuthServer struct {
	proxypb.UnimplementedProxyServiceServer
	gotReq  chan *proxypb.AuthenticateAdminRequest
	handler func(req *proxypb.AuthenticateAdminRequest) *proxypb.AuthenticateAdminResponse
}

func (s *fakeAdminAuthServer) AuthenticateAdmin(ctx context.Context, req *proxypb.AuthenticateAdminRequest) (*proxypb.AuthenticateAdminResponse, error) {
	s.gotReq <- req
	return s.handler(req), nil
}

// newGRPCAdminAuthProxy starts an in-process gRPC server serving srv and
// returns a GRPCProxy pointed at it (transport_test.go listener pattern).
func newGRPCAdminAuthProxy(t *testing.T, srv *fakeAdminAuthServer) *GRPCProxy {
	t.Helper()

	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	s := grpc.NewServer()
	proxypb.RegisterProxyServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)

	p, err := NewGRPCProxy(&ProxyConfig{
		Name:     "test-grpc-admin",
		Endpoint: lis.Addr().String(),
		GRPC:     &GRPCProxyConfig{Insecure: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestGRPCProxy_AuthenticateAdmin locks the gRPC round-trip contract: the
// request fields must reach the backend untouched and the response error and
// identity (all four fields) must map into the proxy response types, covering
// the full identity, nil identity + error, and empty-lists branches.
func TestGRPCProxy_AuthenticateAdmin(t *testing.T) {
	cases := []struct {
		name    string
		backend *proxypb.AuthenticateAdminResponse
		verify  func(t *testing.T, resp *AuthenticateAdminProxyResponse)
	}{
		{
			name: "identity fully mapped",
			backend: &proxypb.AuthenticateAdminResponse{
				Identity: &proxypb.AdminIdentityInfo{
					KeyId:         "key-42",
					Namespaces:    []string{"acme", "beta"},
					Capabilities:  []string{"session.act", "history.read"},
					MaxAgeSeconds: 30,
				},
			},
			verify: func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
				require.Nil(t, resp.Error)
				require.NotNil(t, resp.Identity)
				assert.Equal(t, "key-42", resp.Identity.KeyID)
				assert.Equal(t, []string{"acme", "beta"}, resp.Identity.Namespaces)
				assert.Equal(t, []string{"session.act", "history.read"}, resp.Identity.Capabilities)
				assert.Equal(t, int64(30), resp.Identity.MaxAgeSeconds)
			},
		},
		{
			name: "nil identity with backend error",
			backend: &proxypb.AuthenticateAdminResponse{
				Error: &sharedv2.Error{Code: "INVALID_API_KEY", Type: "auth_error", Message: "key rejected"},
			},
			verify: func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
				require.Nil(t, resp.Identity, "an identity the backend never decided stays nil")
				require.NotNil(t, resp.Error)
				assert.Equal(t, "INVALID_API_KEY", resp.Error.Code)
				assert.Equal(t, "auth_error", resp.Error.Type)
				assert.Equal(t, "key rejected", resp.Error.Message)
			},
		},
		{
			name: "empty lists deny everything with zero capabilities",
			backend: &proxypb.AuthenticateAdminResponse{
				Identity: &proxypb.AdminIdentityInfo{KeyId: "key-43"},
			},
			verify: func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
				require.Nil(t, resp.Error)
				require.NotNil(t, resp.Identity)
				assert.Equal(t, "key-43", resp.Identity.KeyID)
				assert.Empty(t, resp.Identity.Namespaces)
				assert.Empty(t, resp.Identity.Capabilities)
				assert.Equal(t, int64(0), resp.Identity.MaxAgeSeconds)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &fakeAdminAuthServer{
				gotReq:  make(chan *proxypb.AuthenticateAdminRequest, 1),
				handler: func(req *proxypb.AuthenticateAdminRequest) *proxypb.AuthenticateAdminResponse { return tc.backend },
			}
			p := newGRPCAdminAuthProxy(t, srv)

			resp, err := p.AuthenticateAdmin(context.Background(), &AuthenticateAdminProxyRequest{
				APIKey:     "sk-test-admin-key-material",
				RemoteAddr: "10.0.0.9:5555",
			})
			require.NoError(t, err)

			got := <-srv.gotReq
			assert.Equal(t, "sk-test-admin-key-material", got.ApiKey, "api key must pass through to the backend")
			assert.Equal(t, "10.0.0.9:5555", got.RemoteAddr, "remote addr must pass through to the backend")

			tc.verify(t, resp)
		})
	}
}

// --- HTTP contract ---

// TestHTTPProxy_AuthenticateAdmin_RequestCarriesKeyAndRemoteAddr verifies the
// request body the backend receives: marshalProxyJSON emits proto field names
// (snake_case), so api_key and remote_addr must arrive under those members.
func TestHTTPProxy_AuthenticateAdmin_RequestCarriesKeyAndRemoteAddr(t *testing.T) {
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

	_, err := p.AuthenticateAdmin(context.Background(), &AuthenticateAdminProxyRequest{
		APIKey:     "sk-test-admin-key-material",
		RemoteAddr: "10.0.0.9:5555",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "sk-test-admin-key-material", body["api_key"])
	assert.Equal(t, "10.0.0.9:5555", body["remote_addr"])
}

// TestHTTPProxy_AuthenticateAdmin_ResponseBothJSONFieldNames locks the
// response parse contract: the protojson decoder must accept both the proto3
// JSON contract (camelCase such as keyId/maxAgeSeconds) and the original proto
// field names (key_id/max_age_seconds), and the identity and error members
// must map to the proxy response types including the nil-identity and
// empty-lists branches.
func TestHTTPProxy_AuthenticateAdmin_ResponseBothJSONFieldNames(t *testing.T) {
	cases := []struct {
		name     string
		respBody string
		verify   func(t *testing.T, resp *AuthenticateAdminProxyResponse)
	}{
		{
			name:     "camelCase JSON contract",
			respBody: `{"identity":{"keyId":"key-9","namespaces":["acme","beta"],"capabilities":["history.read"],"maxAgeSeconds":15}}`,
			verify:   verifyFullIdentity(t),
		},
		{
			name:     "original proto field names",
			respBody: `{"identity":{"key_id":"key-9","namespaces":["acme","beta"],"capabilities":["history.read"],"max_age_seconds":15}}`,
			verify:   verifyFullIdentity(t),
		},
		{
			name:     "error only leaves identity nil",
			respBody: `{"error":{"code":"INVALID_API_KEY","type":"auth_error","message":"key rejected"}}`,
			verify: func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
				require.Nil(t, resp.Identity, "an identity the backend never decided stays nil")
				require.NotNil(t, resp.Error)
				assert.Equal(t, "INVALID_API_KEY", resp.Error.Code)
				assert.Equal(t, "auth_error", resp.Error.Type)
				assert.Equal(t, "key rejected", resp.Error.Message)
			},
		},
		{
			name:     "empty identity lists deny everything",
			respBody: `{"identity":{"keyId":"key-10"}}`,
			verify: func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
				require.Nil(t, resp.Error)
				require.NotNil(t, resp.Identity)
				assert.Equal(t, "key-10", resp.Identity.KeyID)
				assert.Empty(t, resp.Identity.Namespaces)
				assert.Empty(t, resp.Identity.Capabilities)
				assert.Equal(t, int64(0), resp.Identity.MaxAgeSeconds)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.respBody))
			}))
			defer server.Close()

			p := newTestHTTPProxy(t, server)

			resp, err := p.AuthenticateAdmin(context.Background(), &AuthenticateAdminProxyRequest{
				APIKey:     "sk-test-admin-key-material",
				RemoteAddr: "10.0.0.9:5555",
			})
			require.NoError(t, err)
			tc.verify(t, resp)
		})
	}
}

// verifyFullIdentity asserts the fully populated identity shared by the two
// JSON-shape subtests of TestHTTPProxy_AuthenticateAdmin_ResponseBothJSONFieldNames.
func verifyFullIdentity(t *testing.T) func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
	t.Helper()
	return func(t *testing.T, resp *AuthenticateAdminProxyResponse) {
		require.Nil(t, resp.Error)
		require.NotNil(t, resp.Identity)
		assert.Equal(t, "key-9", resp.Identity.KeyID)
		assert.Equal(t, []string{"acme", "beta"}, resp.Identity.Namespaces)
		assert.Equal(t, []string{"history.read"}, resp.Identity.Capabilities)
		assert.Equal(t, int64(15), resp.Identity.MaxAgeSeconds)
	}
}
