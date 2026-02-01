package messageloopgo

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	proxypb "github.com/deeplooplabs/messageloop/genproto/proxy/v1"
	sharedpb "github.com/deeplooplabs/messageloop/genproto/shared/v1"
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
	Event   *RPCProxyEvent
}

// RPCProxyEvent wraps the CloudEvent from an RPC request.
type RPCProxyEvent struct {
	ID      string
	Channel string
	Method  string
	Event   *pb.CloudEvent
}

// RPCResponse represents the response to an RPC request.
type RPCResponse struct {
	Event interface{} // Can be *pb.CloudEvent or any proto.Message
	Error *sharedpb.Error
}

// AuthHandler defines the interface for handling authentication requests.
type AuthHandler interface {
	// Authenticate authenticates a client and returns user info.
	Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error)
}

// AuthenticateRequest represents an authentication request.
type AuthenticateRequest struct {
	Username   string
	Password   string
	ClientType string
	ClientID   string
}

// AuthenticateResponse represents the response to an authentication request.
type AuthenticateResponse struct {
	UserInfo *UserInfo
	Error    *sharedpb.Error
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

// ACLHandler defines the interface for handling subscription ACL checks.
type ACLHandler interface {
	// CheckSubscribeACL checks if a client is allowed to subscribe to a channel.
	CheckSubscribeACL(ctx context.Context, channel, token string) error
}

// LifecycleHandler defines the interface for handling client lifecycle events.
type LifecycleHandler interface {
	// OnConnected is called when a client connects.
	OnConnected(ctx context.Context, sessionID, username string) error
	// OnDisconnected is called when a client disconnects.
	OnDisconnected(ctx context.Context, sessionID, username string) error
	// OnSubscribed is called when a client subscribes to channels.
	OnSubscribed(ctx context.Context) error
	// OnUnsubscribed is called when a client unsubscribes from channels.
	OnUnsubscribed(ctx context.Context) error
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
		Error: &sharedpb.Error{
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

// LifecycleHandlerImpl implements the LifecycleHandler interface.
type LifecycleHandlerImpl struct{}

func (h *LifecycleHandlerImpl) OnConnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *LifecycleHandlerImpl) OnDisconnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *LifecycleHandlerImpl) OnSubscribed(ctx context.Context) error {
	return nil
}

func (h *LifecycleHandlerImpl) OnUnsubscribed(ctx context.Context) error {
	return nil
}

// HandlerImpl is a default implementation of all handlers.
// Services can embed this type and override only the methods they need.
type HandlerImpl struct {
	proxypb.UnimplementedProxyServiceServer
	RPCHandlerImpl
	AuthHandlerImpl
	ACLHandlerImpl
	LifecycleHandlerImpl
}

// RPC implements ProxyServiceServer.RPC.
func (h *HandlerImpl) RPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	slog.DebugContext(ctx, "received RPC request",
		"id", req.Id,
		"channel", req.Channel,
		"method", req.Method,
	)

	rpcReq := &RPCRequest{
		ID:      req.Id,
		Channel: req.Channel,
		Method:  req.Method,
		Event: &RPCProxyEvent{
			ID:      req.Id,
			Channel: req.Channel,
			Method:  req.Method,
			Event:   req.GetPayload(),
		},
	}

	resp, err := h.RPCHandlerImpl.HandleRPC(ctx, rpcReq)
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

	if resp.Error != nil {
		slog.DebugContext(ctx, "RPC returned error",
			"code", resp.Error.Code,
			"message", resp.Error.Message,
		)
	}

	// Handle Event - if it's already a CloudEvent, use it directly
	var event *pb.CloudEvent
	if ce, ok := resp.Event.(*pb.CloudEvent); ok {
		event = ce
	}

	return &proxypb.RPCResponse{
		Id:      req.Id,
		Error:   resp.Error,
		Payload: event,
	}, nil
}

// Authenticate implements ProxyServiceServer.Authenticate.
func (h *HandlerImpl) Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.AuthenticateResponse, error) {
	slog.DebugContext(ctx, "received authenticate request",
		"username", req.Username,
		"client_type", req.ClientType,
		"client_id", req.ClientId,
	)

	authReq := &AuthenticateRequest{
		Username:   req.Username,
		Password:   req.Password,
		ClientType: req.ClientType,
		ClientID:   req.ClientId,
	}

	resp, err := h.AuthHandlerImpl.Authenticate(ctx, authReq)
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

	return &proxypb.AuthenticateResponse{
		Error:    resp.Error,
		UserInfo: resp.UserInfo.ToProto(),
	}, nil
}

// SubscribeAcl implements ProxyServiceServer.SubscribeAcl.
func (h *HandlerImpl) SubscribeAcl(ctx context.Context, req *proxypb.SubscribeAclRequest) (*proxypb.SubscribeAclResponse, error) {
	slog.DebugContext(ctx, "received subscribe ACL request",
		"channel", req.Channel,
	)

	err := h.ACLHandlerImpl.CheckSubscribeACL(ctx, req.Channel, req.Token)
	if err != nil {
		slog.ErrorContext(ctx, "subscription denied by ACL", "error", err)
		return &proxypb.SubscribeAclResponse{}, status.Error(codes.PermissionDenied, err.Error())
	}

	return &proxypb.SubscribeAclResponse{}, nil
}

// OnConnected implements ProxyServiceServer.OnConnected.
func (h *HandlerImpl) OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) (*proxypb.OnConnectedResponse, error) {
	slog.DebugContext(ctx, "received OnConnected hook",
		"session_id", req.SessionId,
		"username", req.Username,
	)

	if err := h.LifecycleHandlerImpl.OnConnected(ctx, req.SessionId, req.Username); err != nil {
		slog.ErrorContext(ctx, "OnConnected handler failed", "error", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proxypb.OnConnectedResponse{}, nil
}

// OnSubscribed implements ProxyServiceServer.OnSubscribed.
func (h *HandlerImpl) OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) (*proxypb.OnSubscribedResponse, error) {
	slog.DebugContext(ctx, "received OnSubscribed hook")

	if err := h.LifecycleHandlerImpl.OnSubscribed(ctx); err != nil {
		slog.ErrorContext(ctx, "OnSubscribed handler failed", "error", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &proxypb.OnSubscribedResponse{}, nil
}

// OnUnsubscribed implements ProxyServiceServer.OnUnsubscribed.
func (h *HandlerImpl) OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) (*proxypb.OnUnsubscribedResponse, error) {
	slog.DebugContext(ctx, "received OnUnsubscribed hook")

	if err := h.LifecycleHandlerImpl.OnUnsubscribed(ctx); err != nil {
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

	if err := h.LifecycleHandlerImpl.OnDisconnected(ctx, req.SessionId, req.Username); err != nil {
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
// It implements the lynx.Component interface for lifecycle management.
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
	return s.grpc.Serve(s.conn)
}

// Stop stops the proxy server gracefully.
func (s *ProxyServer) Stop(ctx context.Context) {
	slog.InfoContext(ctx, "stopping proxy gRPC server", "addr", s.opts.Addr)
	s.grpc.GracefulStop()
}
