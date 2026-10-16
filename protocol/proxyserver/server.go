package proxyserver

import (
	"context"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	proxypb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/protocol/genproto/messageloop/shared/v2"
)

// Server implements proxypb.ProxyServiceServer on top of the optional
// capability handlers in Options. Capabilities left nil answer with gRPC
// Unimplemented, so a backend that only serves RPC needs nothing else — the
// MessageLoop server surfaces the other calls as unsupported proxy features.
//
// Panic recovery is built in: a panicking handler with an in-band error
// field yields that error (INTERNAL_ERROR); lifecycle handlers yield a gRPC
// Internal status, since the lifecycle responses carry no error field.
type Server struct {
	proxypb.UnimplementedProxyServiceServer

	rpc       RPCHandler
	auth      AuthHandler
	acl       ACLHandler
	lifecycle LifecycleHandler
	apiKey    APIKeyHandler
}

// Options configures a Server. Every field is optional; nil capabilities
// answer Unimplemented.
type Options struct {
	RPC       RPCHandler
	Auth      AuthHandler
	ACL       ACLHandler
	Lifecycle LifecycleHandler
	APIKey    APIKeyHandler
}

// New creates a Server.
func New(opts Options) *Server {
	return &Server{
		rpc:       opts.RPC,
		auth:      opts.Auth,
		acl:       opts.ACL,
		lifecycle: opts.Lifecycle,
		apiKey:    opts.APIKey,
	}
}

// RegisterGRPC registers the server on a gRPC server under the
// messageloop.proxy.v2.ProxyService name the MessageLoop server dials.
func (s *Server) RegisterGRPC(g grpc.ServiceRegistrar) {
	proxypb.RegisterProxyServiceServer(g, s)
}

// recoverInBand converts a panicking handler into an in-band error response.
// reqID echoes the RPC request id when the response carries one.
func recoverInBand(method, reqID string, resp any) {
	if r := recover(); r != nil {
		log.Printf("proxyserver: %s handler panic: %v", method, r)
		switch out := resp.(type) {
		case **proxypb.RPCResponse:
			*out = &proxypb.RPCResponse{Id: reqID, Error: internalError(r)}
		case **proxypb.AuthenticateResponse:
			*out = &proxypb.AuthenticateResponse{Error: internalError(r)}
		case **proxypb.SubscribeAclResponse:
			*out = &proxypb.SubscribeAclResponse{Error: internalError(r)}
		case **proxypb.PublishAclResponse:
			*out = &proxypb.PublishAclResponse{Error: internalError(r)}
		case **proxypb.AuthenticateAPIKeyResponse:
			*out = &proxypb.AuthenticateAPIKeyResponse{Error: internalError(r)}
		}
	}
}

// recoverOutOfBand converts a panicking lifecycle handler into a gRPC
// Internal status error (the lifecycle responses have no in-band field).
func recoverOutOfBand(method string, errp *error) {
	if r := recover(); r != nil {
		log.Printf("proxyserver: %s handler panic: %v", method, r)
		*errp = status.Error(codes.Internal, fmt.Sprintf("internal error: %v", r))
	}
}

// internalError builds the in-band error for a panicking handler.
func internalError(r any) *sharedv2.Error {
	return &sharedv2.Error{
		Code:    "INTERNAL_ERROR",
		Type:    "server_error",
		Message: fmt.Sprintf("internal error: %v", r),
	}
}

// RPC implements ProxyServiceServer.RPC.
func (s *Server) RPC(ctx context.Context, req *proxypb.RPCRequest) (resp *proxypb.RPCResponse, _ error) {
	if s.rpc == nil {
		return nil, status.Error(codes.Unimplemented, "rpc handler not configured")
	}
	defer recoverInBand("RPC", req.GetId(), &resp)

	resp, err := s.rpc.HandleRPC(ctx, req)
	if err != nil {
		code, typ := "INTERNAL_ERROR", "server_error"
		if err == ErrNoRoute {
			code, typ = "NO_ROUTE", "request_error"
		}
		return &proxypb.RPCResponse{
			Id:    req.GetId(),
			Error: inBandError(err, code, typ),
		}, nil
	}
	if resp == nil {
		return nil, status.Error(codes.Internal, "rpc handler returned a nil response")
	}
	if resp.Id == "" {
		resp.Id = req.GetId()
	}
	return resp, nil
}

// Authenticate implements ProxyServiceServer.Authenticate.
func (s *Server) Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (resp *proxypb.AuthenticateResponse, _ error) {
	if s.auth == nil {
		return nil, status.Error(codes.Unimplemented, "auth handler not configured")
	}
	defer recoverInBand("Authenticate", "", &resp)

	user, err := s.auth.Authenticate(ctx, req)
	if err != nil {
		return &proxypb.AuthenticateResponse{
			Error: inBandError(err, "AUTH_ERROR", "auth_error"),
		}, nil
	}
	return &proxypb.AuthenticateResponse{UserInfo: user}, nil
}

// SubscribeAcl implements ProxyServiceServer.SubscribeAcl.
func (s *Server) SubscribeAcl(ctx context.Context, req *proxypb.SubscribeAclRequest) (resp *proxypb.SubscribeAclResponse, _ error) {
	if s.acl == nil {
		return nil, status.Error(codes.Unimplemented, "acl handler not configured")
	}
	defer recoverInBand("SubscribeAcl", "", &resp)

	if err := s.acl.CheckSubscribeACL(ctx, req); err != nil {
		return &proxypb.SubscribeAclResponse{
			Error: inBandError(err, "PERMISSION_DENIED", "acl_error"),
		}, nil
	}
	return &proxypb.SubscribeAclResponse{}, nil
}

// PublishAcl implements ProxyServiceServer.PublishAcl.
func (s *Server) PublishAcl(ctx context.Context, req *proxypb.PublishAclRequest) (resp *proxypb.PublishAclResponse, _ error) {
	if s.acl == nil {
		return nil, status.Error(codes.Unimplemented, "acl handler not configured")
	}
	defer recoverInBand("PublishAcl", "", &resp)

	if err := s.acl.CheckPublishACL(ctx, req); err != nil {
		return &proxypb.PublishAclResponse{
			Error: inBandError(err, "PERMISSION_DENIED", "acl_error"),
		}, nil
	}
	return &proxypb.PublishAclResponse{}, nil
}

// OnConnected implements ProxyServiceServer.OnConnected.
func (s *Server) OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) (resp *proxypb.OnConnectedResponse, err error) {
	if s.lifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "lifecycle handler not configured")
	}
	defer recoverOutOfBand("OnConnected", &err)

	if err := s.lifecycle.OnConnected(ctx, req); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &proxypb.OnConnectedResponse{}, nil
}

// OnSubscribed implements ProxyServiceServer.OnSubscribed.
func (s *Server) OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) (resp *proxypb.OnSubscribedResponse, err error) {
	if s.lifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "lifecycle handler not configured")
	}
	defer recoverOutOfBand("OnSubscribed", &err)

	if err := s.lifecycle.OnSubscribed(ctx, req); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &proxypb.OnSubscribedResponse{}, nil
}

// OnUnsubscribed implements ProxyServiceServer.OnUnsubscribed.
func (s *Server) OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) (resp *proxypb.OnUnsubscribedResponse, err error) {
	if s.lifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "lifecycle handler not configured")
	}
	defer recoverOutOfBand("OnUnsubscribed", &err)

	if err := s.lifecycle.OnUnsubscribed(ctx, req); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &proxypb.OnUnsubscribedResponse{}, nil
}

// OnDisconnected implements ProxyServiceServer.OnDisconnected.
func (s *Server) OnDisconnected(ctx context.Context, req *proxypb.OnDisconnectedRequest) (resp *proxypb.OnDisconnectedResponse, err error) {
	if s.lifecycle == nil {
		return nil, status.Error(codes.Unimplemented, "lifecycle handler not configured")
	}
	defer recoverOutOfBand("OnDisconnected", &err)

	if err := s.lifecycle.OnDisconnected(ctx, req); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &proxypb.OnDisconnectedResponse{}, nil
}

// AuthenticateAPIKey implements ProxyServiceServer.AuthenticateAPIKey.
func (s *Server) AuthenticateAPIKey(ctx context.Context, req *proxypb.AuthenticateAPIKeyRequest) (resp *proxypb.AuthenticateAPIKeyResponse, _ error) {
	if s.apiKey == nil {
		return nil, status.Error(codes.Unimplemented, "api key handler not configured")
	}
	defer recoverInBand("AuthenticateAPIKey", "", &resp)

	identity, err := s.apiKey.AuthenticateAPIKey(ctx, req)
	if err != nil {
		return &proxypb.AuthenticateAPIKeyResponse{
			Error: inBandError(err, "AUTH_ERROR", "auth_error"),
		}, nil
	}
	return &proxypb.AuthenticateAPIKeyResponse{Identity: identity}, nil
}
