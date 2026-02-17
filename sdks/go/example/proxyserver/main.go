package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	protobuf "github.com/cloudevents/sdk-go/binding/format/protobuf/v2"
	pb "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	"github.com/messageloopio/messageloop/sdks/go"
	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ============================================================================
// Approach 1: Traditional switch-based handler
// ============================================================================

// MyRPCHandler implements custom RPC handling using a switch statement.
// This approach works well for simple cases but can become unwieldy
// as the number of methods grows.
type MyRPCHandler struct{}

func (h *MyRPCHandler) HandleRPC(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	log.Printf("[RPC] channel=%s method=%s", req.Channel, req.Method)

	switch req.Method {
	case "echo":
		// Echo back the request
		return &messageloopgo.RPCResponse{
			Payload: req.Payload,
		}, nil

	case "getUser":
		// Example: Get user by ID
		var userID string
		if req.Payload != nil {
			_ = req.Payload.DataAs(&userID)
		}
		responseEvent := messageloopgo.NewTextCloudEvent(
			req.ID,
			"/proxy/echo",
			"getUser.response",
			"User: "+userID,
		)
		return &messageloopgo.RPCResponse{
			Payload: responseEvent,
		}, nil

	case "sum":
		// Example: Parse and calculate sum
		var data string
		if req.Payload != nil {
			_ = req.Payload.DataAs(&data)
		}
		var a, b int
		if _, err := fmt.Sscanf(data, "%d,%d", &a, &b); err == nil {
			result := fmt.Sprintf("%d", a+b)
			responseEvent := messageloopgo.NewTextCloudEvent(
				req.ID,
				"/proxy/sum",
				"sum.response",
				result,
			)
			return &messageloopgo.RPCResponse{
				Payload: responseEvent,
			}, nil
		}
		return &messageloopgo.RPCResponse{
			Error: &sharedpb.Error{
				Code:    "INVALID_INPUT",
				Type:    "validation_error",
				Message: "Expected format: a,b (e.g., 10,20)",
			},
		}, nil

	default:
		return &messageloopgo.RPCResponse{
			Error: &sharedpb.Error{
				Code:    "UNKNOWN_METHOD",
				Type:    "rpc_error",
				Message: "Unknown method: " + req.Method,
			},
		}, nil
	}
}

// ============================================================================
// Approach 2: RPCMux-based handler (recommended for larger services)
// ============================================================================

// newRPCMux creates an RPC multiplexer with handlers and middleware.
// This approach provides better organization and extensibility.
func newRPCMux() *messageloopgo.RPCMux {
	mux := messageloopgo.NewRPCMux()

	// Register middleware (applied in registration order: logging -> recovery -> handler)
	mux.Use(loggingMiddleware)
	mux.Use(recoveryMiddleware)

	// Register handlers
	mux.Handle("echo", handleEcho)
	mux.Handle("getUser", handleGetUser)
	mux.Handle("sum", handleSum)

	return mux
}

// loggingMiddleware logs RPC requests and their duration.
func loggingMiddleware(next messageloopgo.RPCHandlerFunc) messageloopgo.RPCHandlerFunc {
	return func(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		duration := time.Since(start)

		if err != nil {
			log.Printf("[RPC] method=%s duration=%v error=%v", req.Method, duration, err)
		} else if resp.Error != nil {
			log.Printf("[RPC] method=%s duration=%v error_code=%s", req.Method, duration, resp.Error.Code)
		} else {
			log.Printf("[RPC] method=%s duration=%v success=true", req.Method, duration)
		}

		return resp, err
	}
}

// recoveryMiddleware recovers from panics and returns an error response.
func recoveryMiddleware(next messageloopgo.RPCHandlerFunc) messageloopgo.RPCHandlerFunc {
	return func(ctx context.Context, req *messageloopgo.RPCRequest) (resp *messageloopgo.RPCResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[RPC] PANIC: method=%s recovered=%v", req.Method, r)
				resp = &messageloopgo.RPCResponse{
					Error: &sharedpb.Error{
						Code:    "INTERNAL_ERROR",
						Type:    "server_error",
						Message: fmt.Sprintf("Internal server error: %v", r),
					},
				}
				err = nil
			}
		}()
		return next(ctx, req)
	}
}

// handleEcho echoes back the request.
func handleEcho(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	return &messageloopgo.RPCResponse{
		Payload: req.Payload,
	}, nil
}

// handleGetUser simulates getting user info.
func handleGetUser(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	var userID string
	if req.Payload != nil {
		_ = req.Payload.DataAs(&userID)
	}
	responseEvent := messageloopgo.NewTextCloudEvent(
		req.ID,
		"/proxy/getUser",
		"getUser.response",
		"User: "+userID,
	)
	return &messageloopgo.RPCResponse{
		Payload: responseEvent,
	}, nil
}

// handleSum calculates the sum of two numbers.
func handleSum(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	var data string
	if req.Payload != nil {
		_ = req.Payload.DataAs(&data)
	}
	var a, b int
	if _, err := fmt.Sscanf(data, "%d,%d", &a, &b); err != nil {
		return &messageloopgo.RPCResponse{
			Error: &sharedpb.Error{
				Code:    "INVALID_INPUT",
				Type:    "validation_error",
				Message: "Expected format: a,b (e.g., 10,20)",
			},
		}, nil
	}

	result := fmt.Sprintf("%d", a+b)
	responseEvent := messageloopgo.NewTextCloudEvent(
		req.ID,
		"/proxy/sum",
		"sum.response",
		result,
	)
	return &messageloopgo.RPCResponse{
		Payload: responseEvent,
	}, nil
}

// MyAuthHandler implements custom authentication.
type MyAuthHandler struct{}

func (h *MyAuthHandler) Authenticate(ctx context.Context, req *messageloopgo.AuthenticateRequest) (*messageloopgo.AuthenticateResponse, error) {
	log.Printf("[Auth] username=%s client_type=%s", req.Username, req.ClientType)

	// Simple authentication: accept any non-empty username/password
	if req.Username == "" || req.Password == "" {
		return &messageloopgo.AuthenticateResponse{
			Error: &sharedpb.Error{
				Code:    "INVALID_CREDENTIALS",
				Type:    "auth_error",
				Message: "Username and password are required",
			},
		}, nil
	}

	// Return user info on successful auth
	return &messageloopgo.AuthenticateResponse{
		UserInfo: &messageloopgo.UserInfo{
			ID:         "user-" + req.Username,
			Username:   req.Username,
			Token:      "token-" + req.Username,
			ClientType: req.ClientType,
			ClientID:   req.ClientID,
		},
	}, nil
}

// MyACLHandler implements custom ACL checks.
type MyACLHandler struct{}

func (h *MyACLHandler) CheckSubscribeACL(ctx context.Context, channel, token string) error {
	log.Printf("[ACL] channel=%s token=%s", channel, token)

	// Simple ACL: allow all channels starting with "public."
	// Require valid token for "private." channels
	if hasPrefix(channel, "private.") && token == "" {
		return status.Error(codes.PermissionDenied, "Authentication required for private channels")
	}

	return nil
}

// MyLifecycleHandler implements custom lifecycle hooks.
type MyLifecycleHandler struct{}

func (h *MyLifecycleHandler) OnConnected(ctx context.Context, sessionID, username string) error {
	log.Printf("[Lifecycle] Client connected: sessionID=%s username=%s", sessionID, username)
	return nil
}

func (h *MyLifecycleHandler) OnDisconnected(ctx context.Context, sessionID, username string) error {
	log.Printf("[Lifecycle] Client disconnected: sessionID=%s username=%s", sessionID, username)
	return nil
}

func (h *MyLifecycleHandler) OnSubscribed(ctx context.Context) error {
	log.Printf("[Lifecycle] Client subscribed")
	return nil
}

func (h *MyLifecycleHandler) OnUnsubscribed(ctx context.Context) error {
	log.Printf("[Lifecycle] Client unsubscribed")
	return nil
}

// MyProxyService combines all handlers.
type MyProxyService struct {
	proxypb.UnimplementedProxyServiceServer
	rpcHandler       messageloopgo.RPCHandler
	authHandler      messageloopgo.AuthHandler
	aclHandler       messageloopgo.ACLHandler
	lifecycleHandler messageloopgo.LifecycleHandler
}

// RPC implements ProxyServiceServer.RPC.
func (s *MyProxyService) RPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	log.Printf("[RPC Request] id=%s channel=%s method=%s", req.Id, req.Channel, req.Method)

	// Convert pb.CloudEvent to cloudevents.Event
	var payload *cloudevents.Event
	if pbPayload := req.GetPayload(); pbPayload != nil {
		if ce, err := protobuf.FromProto(pbPayload); err == nil {
			payload = ce
		}
	}

	rpcReq := &messageloopgo.RPCRequest{
		ID:      req.Id,
		Channel: req.Channel,
		Method:  req.Method,
		Payload: payload,
	}

	resp, err := s.rpcHandler.HandleRPC(ctx, rpcReq)
	if err != nil {
		log.Printf("[RPC Error] id=%s error=%s", req.Id, err.Error())
		return &proxypb.RPCResponse{
			Id: req.Id,
			Error: &sharedpb.Error{
				Code:    "INTERNAL_ERROR",
				Type:    "server_error",
				Message: err.Error(),
			},
		}, nil
	}

	// Convert cloudevents.Event to pb.CloudEvent
	var event *pb.CloudEvent
	if resp.Payload != nil {
		if pbEvent, err := protobuf.ToProto(resp.Payload); err == nil {
			event = pbEvent
		}
	}

	if resp.Error != nil {
		log.Printf("[RPC Response] id=%s error_code=%s error_type=%s error_msg=%s", req.Id, resp.Error.Code, resp.Error.Type, resp.Error.Message)
	} else {
		log.Printf("[RPC Response] id=%s success=true", req.Id)
	}

	return &proxypb.RPCResponse{
		Id:      req.Id,
		Error:   resp.Error,
		Payload: event,
	}, nil
}

// Authenticate implements ProxyServiceServer.Authenticate.
func (s *MyProxyService) Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.AuthenticateResponse, error) {
	log.Printf("[Authenticate Request] username=%s client_type=%s client_id=%s", req.Username, req.ClientType, req.ClientId)

	authReq := &messageloopgo.AuthenticateRequest{
		Username:   req.Username,
		Password:   req.Password,
		ClientType: req.ClientType,
		ClientID:   req.ClientId,
	}

	resp, err := s.authHandler.Authenticate(ctx, authReq)
	if err != nil {
		log.Printf("[Authenticate Error] username=%s error=%s", req.Username, err.Error())
		return &proxypb.AuthenticateResponse{
			Error: &sharedpb.Error{
				Code:    "AUTH_ERROR",
				Type:    "auth_error",
				Message: err.Error(),
			},
		}, nil
	}

	if resp.Error != nil {
		log.Printf("[Authenticate Response] username=%s error_code=%s error_type=%s", req.Username, resp.Error.Code, resp.Error.Type)
	} else if resp.UserInfo != nil {
		log.Printf("[Authenticate Response] username=%s user_id=%s success=true", req.Username, resp.UserInfo.ID)
	} else {
		log.Printf("[Authenticate Response] username=%s success=true", req.Username)
	}

	return &proxypb.AuthenticateResponse{
		Error:    resp.Error,
		UserInfo: resp.UserInfo.ToProto(),
	}, nil
}

// SubscribeAcl implements ProxyServiceServer.SubscribeAcl.
func (s *MyProxyService) SubscribeAcl(ctx context.Context, req *proxypb.SubscribeAclRequest) (*proxypb.SubscribeAclResponse, error) {
	log.Printf("[SubscribeAcl Request] channel=%s token=%s", req.Channel, req.Token)

	err := s.aclHandler.CheckSubscribeACL(ctx, req.Channel, req.Token)
	if err != nil {
		log.Printf("[SubscribeAcl Response] channel=%s denied: %s", req.Channel, err.Error())
		return &proxypb.SubscribeAclResponse{}, status.Error(codes.PermissionDenied, err.Error())
	}

	log.Printf("[SubscribeAcl Response] channel=%s allowed=true", req.Channel)
	return &proxypb.SubscribeAclResponse{}, nil
}

// OnConnected implements ProxyServiceServer.OnConnected.
func (s *MyProxyService) OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) (*proxypb.OnConnectedResponse, error) {
	log.Printf("[OnConnected Request] session_id=%s username=%s", req.SessionId, req.Username)

	if err := s.lifecycleHandler.OnConnected(ctx, req.SessionId, req.Username); err != nil {
		log.Printf("[OnConnected Error] session_id=%s error=%s", req.SessionId, err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Printf("[OnConnected Response] session_id=%s success=true", req.SessionId)
	return &proxypb.OnConnectedResponse{}, nil
}

// OnSubscribed implements ProxyServiceServer.OnSubscribed.
func (s *MyProxyService) OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) (*proxypb.OnSubscribedResponse, error) {
	log.Printf("[OnSubscribed Request]")

	if err := s.lifecycleHandler.OnSubscribed(ctx); err != nil {
		log.Printf("[OnSubscribed Error] error=%s", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Printf("[OnSubscribed Response] success=true")
	return &proxypb.OnSubscribedResponse{}, nil
}

// OnUnsubscribed implements ProxyServiceServer.OnUnsubscribed.
func (s *MyProxyService) OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) (*proxypb.OnUnsubscribedResponse, error) {
	log.Printf("[OnUnsubscribed Request]")

	if err := s.lifecycleHandler.OnUnsubscribed(ctx); err != nil {
		log.Printf("[OnUnsubscribed Error] error=%s", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Printf("[OnUnsubscribed Response] success=true")
	return &proxypb.OnUnsubscribedResponse{}, nil
}

// OnDisconnected implements ProxyServiceServer.OnDisconnected.
func (s *MyProxyService) OnDisconnected(ctx context.Context, req *proxypb.OnDisconnectedRequest) (*proxypb.OnDisconnectedResponse, error) {
	log.Printf("[OnDisconnected Request] session_id=%s username=%s", req.SessionId, req.Username)

	if err := s.lifecycleHandler.OnDisconnected(ctx, req.SessionId, req.Username); err != nil {
		log.Printf("[OnDisconnected Error] session_id=%s error=%s", req.SessionId, err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}

	log.Printf("[OnDisconnected Response] session_id=%s success=true", req.SessionId)
	return &proxypb.OnDisconnectedResponse{}, nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func main() {
	// Choose RPC handler approach:
	//
	// Approach 1: Traditional switch-based handler (simpler for few methods)
	// rpcHandler := &MyRPCHandler{}
	//
	// Approach 2: RPCMux-based handler (recommended for larger services)
	// - Better organization with explicit method registration
	// - Middleware support for logging, recovery, auth, etc.
	// - Easy to extend with new methods
	rpcHandler := newRPCMux()

	// Create the proxy service with custom handlers
	handler := &MyProxyService{
		rpcHandler:       rpcHandler,
		authHandler:      &MyAuthHandler{},
		aclHandler:       &MyACLHandler{},
		lifecycleHandler: &MyLifecycleHandler{},
	}

	// Create gRPC server
	grpcServer := grpc.NewServer()
	proxypb.RegisterProxyServiceServer(grpcServer, handler)

	// Listen on port
	addr := ":9001"
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	log.Printf("Proxy server starting on %s...", addr)

	// Start server in background
	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down proxy server...")
	grpcServer.GracefulStop()
	log.Println("Server stopped")
}
