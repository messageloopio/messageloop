package messageloopgo

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// RPCHandler defines the interface for handling RPC requests from the MessageLoop server.
// Backend services implement this interface to handle actual RPC logic.
type RPCHandler interface {
	// HandleRPC processes an RPC request and returns the response.
	HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error)
}

// RPCRequest represents an incoming RPC request from MessageLoop.
type RPCRequest struct {
	ID      string
	Channel string
	Method  string
	Payload *Message
}

// RPCResponse represents the response to an RPC request.
type RPCResponse struct {
	Payload *Message
	Error   *sharedv2.Error
}

// AuthHandler defines the interface for handling authentication requests.
type AuthHandler interface {
	// Authenticate authenticates a client and returns user info.
	Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error)
}

// AuthenticateRequest represents an authentication request.
type AuthenticateRequest struct {
	ClientID   string
	Token      string
	ClientType string
}

// AuthenticateResponse represents the response to an authentication request.
type AuthenticateResponse struct {
	UserInfo *UserInfo
	Error    *sharedv2.Error
}

// UserInfo contains authenticated user information.
type UserInfo struct {
	ID         string
	Username   string
	Token      string
	ClientType string
	ClientID   string
}

// ToProto converts UserInfo to protobuf format.
func (u *UserInfo) ToProto() *proxypb.UserInfo {
	if u == nil {
		return nil
	}
	return &proxypb.UserInfo{
		Id:         u.ID,
		Username:   u.Username,
		Token:      u.Token,
		ClientType: u.ClientType,
		ClientId:   u.ClientID,
	}
}

// payloadV1toV2 converts a proxy-wire shared.v1 payload into the SDK's
// shared.v2 payload type. The shapes are identical except for the package
// path: the proxy protocol (protocol/proxy/v1) still speaks shared.v1 while
// the SDK's data types are v2.
func payloadV1toV2(p *sharedpb.Payload) *sharedv2.Payload {
	if p == nil {
		return nil
	}
	out := &sharedv2.Payload{ContentType: p.GetContentType()}
	switch d := p.Data.(type) {
	case *sharedpb.Payload_Json:
		out.Data = &sharedv2.Payload_Json{Json: d.Json}
	case *sharedpb.Payload_Binary:
		out.Data = &sharedv2.Payload_Binary{Binary: d.Binary}
	case *sharedpb.Payload_Text:
		out.Data = &sharedv2.Payload_Text{Text: d.Text}
	}
	return out
}

// payloadV2toV1 converts an SDK shared.v2 payload back into the shared.v1
// payload the proxy wire carries.
func payloadV2toV1(p *sharedv2.Payload) *sharedpb.Payload {
	if p == nil {
		return nil
	}
	out := &sharedpb.Payload{ContentType: p.GetContentType()}
	switch d := p.Data.(type) {
	case *sharedv2.Payload_Json:
		out.Data = &sharedpb.Payload_Json{Json: d.Json}
	case *sharedv2.Payload_Binary:
		out.Data = &sharedpb.Payload_Binary{Binary: d.Binary}
	case *sharedv2.Payload_Text:
		out.Data = &sharedpb.Payload_Text{Text: d.Text}
	}
	return out
}

// errorV2toV1 converts an SDK shared.v2 error into the shared.v1 error the
// proxy wire carries.
func errorV2toV1(e *sharedv2.Error) *sharedpb.Error {
	if e == nil {
		return nil
	}
	return &sharedpb.Error{
		Code:    e.GetCode(),
		Type:    e.GetType(),
		Message: e.GetMessage(),
	}
}

// ACLHandler defines the interface for handling subscription ACL checks.
type ACLHandler interface {
	// CheckSubscribeACL checks if a client is allowed to subscribe to a channel.
	CheckSubscribeACL(ctx context.Context, channel, token string) error
	// CheckPublishACL checks if a client is allowed to publish to a channel.
	CheckPublishACL(ctx context.Context, channel, token string) error
}

// LifecycleHandler defines the interface for handling client lifecycle events.
type LifecycleHandler interface {
	// OnConnected is called when a client connects.
	OnConnected(ctx context.Context, sessionID, username string) error
	// OnDisconnected is called when a client disconnects.
	OnDisconnected(ctx context.Context, sessionID, username string) error
	// OnSubscribed is called when a client subscribes to a channel.
	OnSubscribed(ctx context.Context, sessionID, channel, username string) error
	// OnUnsubscribed is called when a client unsubscribes from a channel.
	OnUnsubscribed(ctx context.Context, sessionID, channel, username string) error
}

// RPCHandlerImpl implements the RPCHandler interface.
type RPCHandlerImpl struct{}

func (h *RPCHandlerImpl) HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
	return nil, status.Error(codes.Unimplemented, "RPC handler not implemented")
}

// AuthHandlerImpl implements the AuthHandler interface.
type AuthHandlerImpl struct{}

func (h *AuthHandlerImpl) Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error) {
	return &AuthenticateResponse{
		Error: &sharedv2.Error{
			Code:    "AUTH_NOT_IMPLEMENTED",
			Type:    "auth_error",
			Message: "Authentication handler not implemented",
		},
	}, nil
}

// ACLHandlerImpl implements the ACLHandler interface.
type ACLHandlerImpl struct{}

func (h *ACLHandlerImpl) CheckSubscribeACL(ctx context.Context, channel, token string) error {
	return nil // Default: allow all
}

func (h *ACLHandlerImpl) CheckPublishACL(ctx context.Context, channel, token string) error {
	return nil // Default: allow all
}

// LifecycleHandlerImpl implements the LifecycleHandler interface.
type LifecycleHandlerImpl struct{}

func (h *LifecycleHandlerImpl) OnConnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *LifecycleHandlerImpl) OnDisconnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *LifecycleHandlerImpl) OnSubscribed(ctx context.Context, sessionID, channel, username string) error {
	return nil
}

func (h *LifecycleHandlerImpl) OnUnsubscribed(ctx context.Context, sessionID, channel, username string) error {
	return nil
}

// HandlerImpl is a default implementation of all handlers.
// Services can embed this type and override only the methods they need.
//
// Custom handlers are injected through the RPCHandler, AuthHandler,
// ACLHandler and LifecycleHandler fields. When a field is set it takes
// precedence over the corresponding embedded default implementation
// (RPCHandlerImpl/AuthHandlerImpl/ACLHandlerImpl/LifecycleHandlerImpl);
// a zero-value HandlerImpl dispatches to the embedded defaults.
type HandlerImpl struct {
	proxypb.UnimplementedProxyServiceServer
	RPCHandlerImpl
	AuthHandlerImpl
	ACLHandlerImpl
	LifecycleHandlerImpl

	// RPCHandler overrides the RPC dispatch target when non-nil.
	RPCHandler RPCHandler
	// AuthHandler overrides the authentication dispatch target when non-nil.
	AuthHandler AuthHandler
	// ACLHandler overrides the ACL dispatch target when non-nil.
	ACLHandler ACLHandler
	// LifecycleHandler overrides the lifecycle dispatch target when non-nil.
	LifecycleHandler LifecycleHandler
}

// rpcHandler returns the RPC handler used for dispatch, defaulting to the
// embedded RPCHandlerImpl when no override is configured.
func (h *HandlerImpl) rpcHandler() RPCHandler {
	if h.RPCHandler != nil {
		return h.RPCHandler
	}
	return &h.RPCHandlerImpl
}

// authHandler returns the auth handler used for dispatch, defaulting to the
// embedded AuthHandlerImpl when no override is configured.
func (h *HandlerImpl) authHandler() AuthHandler {
	if h.AuthHandler != nil {
		return h.AuthHandler
	}
	return &h.AuthHandlerImpl
}

// aclHandler returns the ACL handler used for dispatch, defaulting to the
// embedded ACLHandlerImpl when no override is configured.
func (h *HandlerImpl) aclHandler() ACLHandler {
	if h.ACLHandler != nil {
		return h.ACLHandler
	}
	return &h.ACLHandlerImpl
}

// lifecycleHandler returns the lifecycle handler used for dispatch,
// defaulting to the embedded LifecycleHandlerImpl when no override is
// configured.
func (h *HandlerImpl) lifecycleHandler() LifecycleHandler {
	if h.LifecycleHandler != nil {
		return h.LifecycleHandler
	}
	return &h.LifecycleHandlerImpl
}

// RPC implements ProxyServiceServer.RPC.
func (h *HandlerImpl) RPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	slog.DebugContext(ctx, "received RPC request",
		"id", req.Id,
		"channel", req.Channel,
		"method", req.Method,
	)

	// Convert Payload to Message
	var payload *Message
	if pbPayload := req.GetPayload(); pbPayload != nil {
		payload = PayloadToMessage(payloadV1toV2(pbPayload), "")
	}

	rpcReq := &RPCRequest{
		ID:      req.Id,
		Channel: req.Channel,
		Method:  req.Method,
		Payload: payload,
	}

	resp, err := h.rpcHandler().HandleRPC(ctx, rpcReq)
	if err != nil {
		slog.ErrorContext(ctx, "RPC handler failed", "error", err)
		return &proxypb.RPCResponse{
			Id: req.Id,
			Error: &sharedpb.Error{
				Code:    "INTERNAL_ERROR",
				Type:    "server_error",
				Message: err.Error(),
			},
		}, nil
	}

	// A nil response would dereference below and panic inside the gRPC
	// handler, crashing the whole proxy process. Surface it as a gRPC
	// Internal error instead.
	if resp == nil {
		slog.ErrorContext(ctx, "RPC handler returned a nil response")
		return nil, status.Error(codes.Internal, "RPC handler returned a nil response")
	}

	if resp.Error != nil {
		slog.DebugContext(ctx, "RPC returned error",
			"code", resp.Error.Code,
			"message", resp.Error.Message,
		)
	}

	// Convert Message to Payload (bridging the SDK's v2 payload to the v1
	// proxy wire).
	var respPayload *sharedpb.Payload
	if resp.Payload != nil {
		p, err := resp.Payload.ToPayload()
		if err != nil {
			slog.ErrorContext(ctx, "failed to convert response payload", "error", err)
			return nil, status.Error(codes.Internal, fmt.Sprintf("failed to convert response payload: %v", err))
		}
		respPayload = payloadV2toV1(p)
	}

	return &proxypb.RPCResponse{
		Id:      req.Id,
		Error:   errorV2toV1(resp.Error),
		Payload: respPayload,
	}, nil
}

// Authenticate implements ProxyServiceServer.Authenticate.
func (h *HandlerImpl) Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.AuthenticateResponse, error) {
	slog.DebugContext(ctx, "received authenticate request",
		"client_id", req.ClientId,
		"client_type", req.ClientType,
	)

	authReq := &AuthenticateRequest{
		ClientID:   req.ClientId,
		Token:      req.Token,
		ClientType: req.ClientType,
	}

	resp, err := h.authHandler().Authenticate(ctx, authReq)
	if err != nil {
		slog.ErrorContext(ctx, "auth handler failed", "error", err)
		return &proxypb.AuthenticateResponse{
			Error: &sharedpb.Error{
				Code:    "AUTH_ERROR",
				Type:    "auth_error",
				Message: err.Error(),
			},
		}, nil
	}

	// A nil response would dereference below (resp.Error / resp.UserInfo) and
	// panic inside the gRPC handler, crashing the whole proxy process.
	if resp == nil {
		slog.ErrorContext(ctx, "auth handler returned a nil response")
		return nil, status.Error(codes.Internal, "auth handler returned a nil response")
	}

	return &proxypb.AuthenticateResponse{
		Error:    errorV2toV1(resp.Error),
		UserInfo: resp.UserInfo.ToProto(),
	}, nil
}

// SubscribeAcl implements ProxyServiceServer.SubscribeAcl.
func (h *HandlerImpl) SubscribeAcl(ctx context.Context, req *proxypb.SubscribeAclRequest) (*proxypb.SubscribeAclResponse, error) {
	slog.DebugContext(ctx, "received subscribe ACL request",
		"channel", req.Channel,
	)

	err := h.aclHandler().CheckSubscribeACL(ctx, req.Channel, req.Token)
	if err != nil {
		slog.ErrorContext(ctx, "subscription denied by ACL", "error", err)
		return &proxypb.SubscribeAclResponse{}, status.Error(codes.PermissionDenied, err.Error())
	}

	return &proxypb.SubscribeAclResponse{}, nil
}

// PublishAcl implements ProxyServiceServer.PublishAcl.
func (h *HandlerImpl) PublishAcl(ctx context.Context, req *proxypb.PublishAclRequest) (*proxypb.PublishAclResponse, error) {
	slog.DebugContext(ctx, "received publish ACL request",
		"channel", req.Channel,
	)

	err := h.aclHandler().CheckPublishACL(ctx, req.Channel, req.Token)
	if err != nil {
		slog.ErrorContext(ctx, "publish denied by ACL", "error", err)
		return &proxypb.PublishAclResponse{}, status.Error(codes.PermissionDenied, err.Error())
	}

	return &proxypb.PublishAclResponse{}, nil
}

// OnConnected implements ProxyServiceServer.OnConnected.
func (h *HandlerImpl) OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) (*proxypb.OnConnectedResponse, error) {
	slog.DebugContext(ctx, "received OnConnected hook",
		"session_id", req.SessionId,
		"username", req.Username,
	)

	if err := h.lifecycleHandler().OnConnected(ctx, req.SessionId, req.Username); err != nil {
		slog.ErrorContext(ctx, "OnConnected handler failed", "error", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proxypb.OnConnectedResponse{}, nil
}

// OnSubscribed implements ProxyServiceServer.OnSubscribed.
func (h *HandlerImpl) OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) (*proxypb.OnSubscribedResponse, error) {
	slog.DebugContext(ctx, "received OnSubscribed hook",
		"session_id", req.SessionId,
		"channel", req.Channel,
		"username", req.Username,
	)

	if err := h.lifecycleHandler().OnSubscribed(ctx, req.SessionId, req.Channel, req.Username); err != nil {
		slog.ErrorContext(ctx, "OnSubscribed handler failed", "error", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proxypb.OnSubscribedResponse{}, nil
}

// OnUnsubscribed implements ProxyServiceServer.OnUnsubscribed.
func (h *HandlerImpl) OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) (*proxypb.OnUnsubscribedResponse, error) {
	slog.DebugContext(ctx, "received OnUnsubscribed hook",
		"session_id", req.SessionId,
		"channel", req.Channel,
		"username", req.Username,
	)

	if err := h.lifecycleHandler().OnUnsubscribed(ctx, req.SessionId, req.Channel, req.Username); err != nil {
		slog.ErrorContext(ctx, "OnUnsubscribed handler failed", "error", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proxypb.OnUnsubscribedResponse{}, nil
}

// OnDisconnected implements ProxyServiceServer.OnDisconnected.
func (h *HandlerImpl) OnDisconnected(ctx context.Context, req *proxypb.OnDisconnectedRequest) (*proxypb.OnDisconnectedResponse, error) {
	slog.DebugContext(ctx, "received OnDisconnected hook",
		"session_id", req.SessionId,
		"username", req.Username,
	)

	if err := h.lifecycleHandler().OnDisconnected(ctx, req.SessionId, req.Username); err != nil {
		slog.ErrorContext(ctx, "OnDisconnected handler failed", "error", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proxypb.OnDisconnectedResponse{}, nil
}

// ProxyServerOptions configures the proxy server.
type ProxyServerOptions struct {
	// Addr is the address to listen on (e.g., ":9001")
	Addr string `yaml:"addr" json:"addr"`

	// Insecure disables TLS (default: true for development)
	Insecure bool `yaml:"insecure" json:"insecure"`
}

// NewProxyServer creates a new proxy gRPC server that integrates with the lynx framework.
// The handler parameter should implement the ProxyServiceServer interface (or embed HandlerImpl).
//
// TLS: this constructor never installs TLS credentials. With Insecure=false
// (the zero value) the server still listens in plaintext, so a MessageLoop
// server configured to dial this proxy with TLS will fail the handshake.
// Either set Insecure=true for explicit plaintext (development), or terminate
// TLS in front of the proxy.
func NewProxyServer(opts ProxyServerOptions, handler proxypb.ProxyServiceServer) (*ProxyServer, error) {
	grpcOpts := []grpc.ServerOption{}
	if opts.Insecure {
		grpcOpts = append(grpcOpts, grpc.Creds(insecure.NewCredentials()))
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	proxypb.RegisterProxyServiceServer(grpcServer, handler)

	conn, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", opts.Addr, err)
	}

	return &ProxyServer{
		grpc: grpcServer,
		conn: conn,
		opts: &opts,
	}, nil
}

// ProxyServer wraps a gRPC server that hosts the ProxyService.
// It implements the lynx.Service interface for lifecycle management.
type ProxyServer struct {
	grpc *grpc.Server
	conn net.Listener
	opts *ProxyServerOptions
}

// Name returns the component name.
func (s *ProxyServer) Name() string {
	return "proxy-server"
}

// Start starts the proxy server.
func (s *ProxyServer) Start(ctx context.Context) error {
	slog.InfoContext(ctx, "starting proxy gRPC server", "addr", s.opts.Addr)
	if !s.opts.Insecure {
		// The constructor never installs TLS credentials: with Insecure=false
		// the listener still serves plaintext, which mismatches a TLS-dialing
		// MessageLoop server. Warn so the misconfiguration is visible.
		slog.WarnContext(ctx, "proxy server has no TLS credentials and will serve plaintext; set Insecure=true for explicit plaintext or terminate TLS in front of the proxy")
	}
	return s.grpc.Serve(s.conn)
}

// Stop stops the proxy server gracefully.
func (s *ProxyServer) Stop(ctx context.Context) {
	slog.InfoContext(ctx, "stopping proxy gRPC server", "addr", s.opts.Addr)
	s.grpc.GracefulStop()
}
