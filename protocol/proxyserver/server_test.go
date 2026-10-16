package proxyserver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	proxypb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/protocol/genproto/messageloop/shared/v2"
)

func TestServerRPCEchoAndErrorMapping(t *testing.T) {
	assert := assert.New(t)

	ok := RPCHandlerFunc(func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		return &proxypb.RPCResponse{Payload: req.GetPayload(), Error: &sharedv2.Error{Code: "BUSINESS", Type: "biz", Message: "custom"}}, nil
	})
	failing := RPCHandlerFunc(func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		return nil, errors.New("boom")
	})
	carried := RPCHandlerFunc(func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		return nil, NewError(&sharedv2.Error{Code: "MY_CODE", Type: "my_type", Message: "verbatim"})
	})
	panicking := RPCHandlerFunc(func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		panic("handler bug")
	})
	nilResp := RPCHandlerFunc(func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		return nil, nil
	})

	s := New(Options{RPC: ok})
	req := &proxypb.RPCRequest{Id: "req-1", Channel: "chat.room1", Method: "send", Payload: NewTextPayload("hi")}

	resp, err := s.RPC(context.Background(), req)
	require.NoError(t, err)
	assert.Equal("req-1", resp.Id, "reply id echoes the request")
	assert.Equal("hi", PayloadText(resp.Payload), "payload passes through in-band")
	assert.Equal("BUSINESS", resp.Error.Code, "handler-set reply errors pass through verbatim")

	resp, err = New(Options{RPC: failing}).RPC(context.Background(), req)
	require.NoError(t, err, "handler errors are in-band, not gRPC errors")
	assert.Equal("req-1", resp.Id)
	assert.Equal("INTERNAL_ERROR", resp.Error.Code)
	assert.Equal("server_error", resp.Error.Type)
	assert.Contains(resp.Error.Message, "boom")

	resp, err = New(Options{RPC: carried}).RPC(context.Background(), req)
	require.NoError(t, err)
	assert.Equal("MY_CODE", resp.Error.Code, "*Error payloads are used verbatim")
	assert.Equal("my_type", resp.Error.Type)

	resp, err = New(Options{RPC: panicking}).RPC(context.Background(), req)
	require.NoError(t, err)
	assert.Equal("req-1", resp.Id)
	assert.Equal("INTERNAL_ERROR", resp.Error.Code)

	_, err = New(Options{RPC: nilResp}).RPC(context.Background(), req)
	assert.Equal(codes.Internal, status.Code(err), "nil handler response is a gRPC error, not a silent drop")
}

func TestServerRPCNoRoute(t *testing.T) {
	assert := assert.New(t)
	mux := NewRPCMux()
	s := New(Options{RPC: mux})

	resp, err := s.RPC(context.Background(), &proxypb.RPCRequest{Id: "r", Channel: "chat.room1", Method: "missing"})
	require.NoError(t, err)
	assert.Equal("NO_ROUTE", resp.Error.Code)
	assert.Equal("request_error", resp.Error.Type)

	require.NoError(t, mux.Handle("chat.*", "send", echo("send")))
	resp, err = s.RPC(context.Background(), &proxypb.RPCRequest{Id: "r", Channel: "chat.room1", Method: "send"})
	require.NoError(t, err)
	assert.Nil(resp.Error)
	assert.Equal("send", resp.Metadata.Entries["method"])
}

func TestServerUnimplementedCapabilities(t *testing.T) {
	assert := assert.New(t)
	s := New(Options{})

	ctx := context.Background()
	_, err := s.RPC(ctx, &proxypb.RPCRequest{})
	assert.Equal(codes.Unimplemented, status.Code(err))
	_, err = s.Authenticate(ctx, &proxypb.AuthenticateRequest{})
	assert.Equal(codes.Unimplemented, status.Code(err))
	_, err = s.SubscribeAcl(ctx, &proxypb.SubscribeAclRequest{})
	assert.Equal(codes.Unimplemented, status.Code(err))
	_, err = s.PublishAcl(ctx, &proxypb.PublishAclRequest{})
	assert.Equal(codes.Unimplemented, status.Code(err))
	_, err = s.OnConnected(ctx, &proxypb.OnConnectedRequest{})
	assert.Equal(codes.Unimplemented, status.Code(err))
	_, err = s.AuthenticateAPIKey(ctx, &proxypb.AuthenticateAPIKeyRequest{})
	assert.Equal(codes.Unimplemented, status.Code(err))
}

func TestServerAuthAndAPIKey(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	auth := AuthHandlerFunc(func(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.UserInfo, error) {
		if req.GetToken() == "" {
			return nil, errors.New("empty token")
		}
		return &proxypb.UserInfo{Id: "u1", Namespace: "acme"}, nil
	})
	s := New(Options{Auth: auth})

	resp, err := s.Authenticate(ctx, &proxypb.AuthenticateRequest{Token: "t"})
	require.NoError(t, err)
	assert.Nil(resp.Error)
	assert.Equal("u1", resp.UserInfo.Id)
	assert.Equal("acme", resp.UserInfo.Namespace)

	resp, err = s.Authenticate(ctx, &proxypb.AuthenticateRequest{ClientId: "c"})
	require.NoError(t, err, "auth denial is in-band")
	assert.Equal("AUTH_ERROR", resp.Error.Code)
	assert.Nil(resp.UserInfo)

	apikey := APIKeyHandlerFunc(func(ctx context.Context, req *proxypb.AuthenticateAPIKeyRequest) (*proxypb.APIKeyInfo, error) {
		return &proxypb.APIKeyInfo{KeyId: "k1", Namespaces: []string{"acme"}}, nil
	})
	resp2, err := New(Options{APIKey: apikey}).AuthenticateAPIKey(ctx, &proxypb.AuthenticateAPIKeyRequest{ApiKey: "key"})
	require.NoError(t, err)
	assert.Nil(resp2.Error)
	assert.Equal("k1", resp2.Identity.KeyId)

	resp3, err := New(Options{APIKey: APIKeyHandlerFunc(func(ctx context.Context, req *proxypb.AuthenticateAPIKeyRequest) (*proxypb.APIKeyInfo, error) {
		return nil, errors.New("unknown key")
	})}).AuthenticateAPIKey(ctx, &proxypb.AuthenticateAPIKeyRequest{ApiKey: "key"})
	require.NoError(t, err, "key denial is in-band")
	assert.Equal("AUTH_ERROR", resp3.Error.Code)
	assert.Nil(resp3.Identity)
}

func TestServerACL(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	acl := &aclFuncs{
		subscribe: func(ctx context.Context, req *proxypb.SubscribeAclRequest) error {
			if req.GetChannel() == "private.secret" {
				return NewError(&sharedv2.Error{Code: "PRIVATE", Type: "acl_error", Message: "members only"})
			}
			return nil
		},
		publish: func(ctx context.Context, req *proxypb.PublishAclRequest) error {
			return errors.New("no publishing")
		},
	}
	s := New(Options{ACL: acl})

	resp, err := s.SubscribeAcl(ctx, &proxypb.SubscribeAclRequest{Channel: "public.news"})
	require.NoError(t, err)
	assert.Nil(resp.Error, "allow yields an empty in-band response")

	resp, err = s.SubscribeAcl(ctx, &proxypb.SubscribeAclRequest{Channel: "private.secret"})
	require.NoError(t, err, "denial is in-band")
	assert.Equal("PRIVATE", resp.Error.Code, "*Error carries a custom denial code")

	resp2, err := s.PublishAcl(ctx, &proxypb.PublishAclRequest{Channel: "public.news"})
	require.NoError(t, err)
	assert.Equal("PERMISSION_DENIED", resp2.Error.Code, "plain errors map to the default denial code")
}

func TestServerLifecycle(t *testing.T) {
	assert := assert.New(t)
	ctx := context.Background()

	calls := 0
	lc := &lifecycleFuncs{
		onConnected: func(ctx context.Context, req *proxypb.OnConnectedRequest) error {
			calls++
			return errors.New("hook store down")
		},
	}
	s := New(Options{Lifecycle: lc})

	_, err := s.OnConnected(ctx, &proxypb.OnConnectedRequest{SessionId: "s1"})
	assert.Equal(codes.Internal, status.Code(err), "lifecycle failures have no in-band channel")
	assert.Equal(1, calls)

	resp, err := New(Options{Lifecycle: &lifecycleFuncs{}}).OnSubscribed(ctx, &proxypb.OnSubscribedRequest{})
	require.NoError(t, err)
	assert.NotNil(resp)
}

// aclFuncs adapts per-method functions to ACLHandler.
type aclFuncs struct {
	subscribe func(ctx context.Context, req *proxypb.SubscribeAclRequest) error
	publish   func(ctx context.Context, req *proxypb.PublishAclRequest) error
}

func (a *aclFuncs) CheckSubscribeACL(ctx context.Context, req *proxypb.SubscribeAclRequest) error {
	return a.subscribe(ctx, req)
}

func (a *aclFuncs) CheckPublishACL(ctx context.Context, req *proxypb.PublishAclRequest) error {
	return a.publish(ctx, req)
}

// lifecycleFuncs adapts individual hooks to LifecycleHandler; unset hooks
// succeed.
type lifecycleFuncs struct {
	onConnected    func(ctx context.Context, req *proxypb.OnConnectedRequest) error
	onSubscribed   func(ctx context.Context, req *proxypb.OnSubscribedRequest) error
	onUnsubscribed func(ctx context.Context, req *proxypb.OnUnsubscribedRequest) error
	onDisconnected func(ctx context.Context, req *proxypb.OnDisconnectedRequest) error
}

func (l *lifecycleFuncs) OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) error {
	if l.onConnected == nil {
		return nil
	}
	return l.onConnected(ctx, req)
}

func (l *lifecycleFuncs) OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) error {
	if l.onSubscribed == nil {
		return nil
	}
	return l.onSubscribed(ctx, req)
}

func (l *lifecycleFuncs) OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) error {
	if l.onUnsubscribed == nil {
		return nil
	}
	return l.onUnsubscribed(ctx, req)
}

func (l *lifecycleFuncs) OnDisconnected(ctx context.Context, req *proxypb.OnDisconnectedRequest) error {
	if l.onDisconnected == nil {
		return nil
	}
	return l.onDisconnected(ctx, req)
}
