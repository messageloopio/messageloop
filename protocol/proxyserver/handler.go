// Package proxyserver provides the server-side framework for implementing
// MessageLoop proxy backends (the proxypb.ProxyService gRPC surface that the
// MessageLoop server calls out to for RPC forwarding, authentication, ACL
// checks and lifecycle hooks).
//
// Backends implement only the capability interfaces they need and hand them
// to New; the returned Server implements proxypb.ProxyServiceServer and takes
// care of payload conversion, error mapping and unimplemented-capability
// responses. RPC requests are dispatched through RPCMux, which routes on
// (channel, method) glob patterns with the same semantics as the server-side
// proxy router (first registered match wins).
package proxyserver

import (
	"context"

	proxypb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
)

// RPCHandler handles client RPC requests forwarded by the MessageLoop server.
// Implementations are usually an RPCMux; a custom type also works.
type RPCHandler interface {
	HandleRPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error)
}

// RPCHandlerFunc adapts a function to RPCHandler.
type RPCHandlerFunc func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error)

// HandleRPC implements RPCHandler.
func (f RPCHandlerFunc) HandleRPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	return f(ctx, req)
}

// AuthHandler authenticates client tokens. On success the returned UserInfo
// is passed to the server verbatim (its namespace field decides multi-tenant
// scoping); a returned error is mapped to the in-band AUTH_ERROR response.
type AuthHandler interface {
	Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.UserInfo, error)
}

// ACLHandler authorizes subscribe/publish attempts. A returned error denies
// the operation and is mapped to the in-band response error (the server
// denies on any non-nil error); nil allows it.
type ACLHandler interface {
	CheckSubscribeACL(ctx context.Context, req *proxypb.SubscribeAclRequest) error
	CheckPublishACL(ctx context.Context, req *proxypb.PublishAclRequest) error
}

// LifecycleHandler receives client connection events. A returned error is
// mapped to a gRPC Internal status: the lifecycle responses carry no in-band
// error field, so out-of-band is the only failure channel.
type LifecycleHandler interface {
	OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) error
	OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) error
	OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) error
	OnDisconnected(ctx context.Context, req *proxypb.OnDisconnectedRequest) error
}

// APIKeyHandler verifies Server API keys. On success the returned APIKeyInfo
// is passed through verbatim (empty namespaces/capabilities fail closed on
// the caller); a returned error is mapped to the in-band AUTH_ERROR response.
type APIKeyHandler interface {
	AuthenticateAPIKey(ctx context.Context, req *proxypb.AuthenticateAPIKeyRequest) (*proxypb.APIKeyInfo, error)
}

// AuthHandlerFunc adapts a function to AuthHandler.
type AuthHandlerFunc func(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.UserInfo, error)

// Authenticate implements AuthHandler.
func (f AuthHandlerFunc) Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.UserInfo, error) {
	return f(ctx, req)
}

// APIKeyHandlerFunc adapts a function to APIKeyHandler.
type APIKeyHandlerFunc func(ctx context.Context, req *proxypb.AuthenticateAPIKeyRequest) (*proxypb.APIKeyInfo, error)

// AuthenticateAPIKey implements APIKeyHandler.
func (f APIKeyHandlerFunc) AuthenticateAPIKey(ctx context.Context, req *proxypb.AuthenticateAPIKeyRequest) (*proxypb.APIKeyInfo, error) {
	return f(ctx, req)
}
