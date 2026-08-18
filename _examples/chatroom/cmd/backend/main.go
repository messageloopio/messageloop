// Command backend is the ChatRoom demo backend service. It speaks the
// MessageLoop ProxyService gRPC protocol and provides:
//
//   - Authenticate: demo token -> user resolution
//   - RPC: chat.roll (dice), chat.stats / chat.history / chat.kick via the
//     server-side admin gRPC API
//   - ACL: private:* channels require a per-subscription token
//   - Lifecycle hooks: announce connect/disconnect to the lobby
//
// Run it with: go run ./_examples/chatroom/cmd/backend
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	"github.com/messageloopio/messageloop/_examples/chatroom/internal/chatroom"
	messageloopgo "github.com/messageloopio/messageloop/sdks/go"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// adminAddr points at the demo server's admin gRPC API; align with config.yaml.
const (
	adminAddr   = chatroom.DefaultAdminAddr
	adminToken  = "chatroom-admin"
	backendAddr = ":8090"
)

func main() {
	var addr string
	flag.StringVar(&addr, "addr", backendAddr, "listen address")
	flag.Parse()

	admin, err := chatroom.NewAdminClient(context.Background(), adminAddr, adminToken)
	if err != nil {
		log.Fatalf("connect admin API: %v", err)
	}
	defer admin.Close()

	service := &messageloopgo.HandlerImpl{}
	service.RPCHandler = newRPCMux(admin)
	service.AuthHandler = &authService{}
	service.ACLHandler = &aclService{}
	service.LifecycleHandler = &lifecycleService{admin: admin, room: chatroom.Lobby}

	server, err := messageloopgo.NewProxyServer(messageloopgo.ProxyServerOptions{
		Addr:     addr,
		Insecure: true,
	}, service)
	if err != nil {
		log.Fatalf("create proxy server: %v", err)
	}

	log.Printf("chatroom backend listening on %s (admin api %s)", addr, adminAddr)
	if err := server.Start(context.Background()); err != nil {
		log.Fatalf("proxy server: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Auth

// authService resolves a demo token ("token-alice", ...) into a user.
type authService struct{}

// Authenticate implements messageloopgo.AuthHandler.
func (s *authService) Authenticate(ctx context.Context, req *messageloopgo.AuthenticateRequest) (*messageloopgo.AuthenticateResponse, error) {
	if req.Token == "" {
		return &messageloopgo.AuthenticateResponse{
			Error: &sharedv2.Error{
				Code:    "INVALID_CREDENTIALS",
				Type:    "auth_error",
				Message: "token is required",
			},
		}, nil
	}
	user, ok := chatroom.LookupByToken(req.Token)
	if !ok {
		return &messageloopgo.AuthenticateResponse{
			Error: &sharedv2.Error{
				Code:    "INVALID_CREDENTIALS",
				Type:    "auth_error",
				Message: "unknown token: " + req.Token,
			},
		}, nil
	}
	log.Printf("[auth] client=%s user=%s role=%s", req.ClientID, user.Name, user.Role)
	return &messageloopgo.AuthenticateResponse{
		UserInfo: &messageloopgo.UserInfo{
			ID:         user.ID,
			Username:   user.Name,
			Token:      req.Token,
			ClientType: req.ClientType,
			ClientID:   req.ClientID,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// ACL

// aclService gates private:* channels behind a per-subscription/per-publish
// token so the demo can show both allow and deny paths.
type aclService struct{}

// CheckSubscribeACL implements messageloopgo.ACLHandler.
func (s *aclService) CheckSubscribeACL(ctx context.Context, channel, token string) error {
	if strings.HasPrefix(channel, chatroom.PrivateChannelPrefix) && token == "" {
		return status.Error(codes.PermissionDenied, "private channel requires a subscription token")
	}
	return nil
}

// CheckPublishACL implements messageloopgo.ACLHandler.
func (s *aclService) CheckPublishACL(ctx context.Context, channel, token string) error {
	if strings.HasPrefix(channel, chatroom.PrivateChannelPrefix) && token == "" {
		return status.Error(codes.PermissionDenied, "private channel requires a publish token")
	}
	return nil
}

// ---------------------------------------------------------------------------
// RPC

// newRPCMux registers the demo RPC methods with middleware.
func newRPCMux(admin *chatroom.AdminClient) *messageloopgo.RPCMux {
	rpc := &rpcService{admin: admin, room: chatroom.Lobby}
	mux := messageloopgo.NewRPCMux()
	mux.Use(loggingMiddleware)
	mux.Handle("chat.roll", rpc.handleRoll)
	mux.Handle("chat.stats", rpc.handleStats)
	mux.Handle("chat.history", rpc.handleHistory)
	mux.Handle("chat.kick", rpc.handleKick)
	mux.Handle("chat.whoami", rpc.handleWhoami)
	return mux
}

// rpcService implements the chat commands.
type rpcService struct {
	admin *chatroom.AdminClient
	room  string
}

// handleRoll returns a dice roll. Demonstrates the simplest RPC round-trip.
func (s *rpcService) handleRoll(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	n := rand.Intn(6) + 1
	return &messageloopgo.RPCResponse{
		Payload: messageloopgo.NewMessageWithData("chat.roll.response",
			messageloopgo.NewTextData(fmt.Sprintf("dice = %d", n))),
	}, nil
}

// handleStats calls the admin GetChannels + GetHistory APIs and returns a
// human-readable summary. Demonstrates backend -> admin API integration.
func (s *rpcService) handleStats(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	channels, err := s.admin.Channels(ctx)
	if err != nil {
		return rpcError("ADMIN_ERROR", "GetChannels failed: "+err.Error()), nil
	}
	var b strings.Builder
	b.WriteString("active channels:\n")
	for _, ch := range channels {
		b.WriteString(fmt.Sprintf("  %s  subscribers=%d", ch.Name, ch.Subscribers))
		if presence, perr := s.admin.Presence(ctx, ch.Name); perr == nil && len(presence) > 0 {
			b.WriteString(fmt.Sprintf("  present=%d", len(presence)))
		}
		b.WriteString("\n")
	}
	if history, herr := s.admin.History(ctx, s.room, 0, 5); herr == nil {
		b.WriteString(fmt.Sprintf("last %d messages in %s:\n", len(history), s.room))
		for _, pub := range history {
			b.WriteString(fmt.Sprintf("  #%d %s\n", pub.Offset, pub.Id))
		}
	}
	return &messageloopgo.RPCResponse{
		Payload: messageloopgo.NewMessageWithData("chat.stats.response",
			messageloopgo.NewTextData(b.String())),
	}, nil
}

// handleHistory returns persisted channel history via the admin GetHistory API.
func (s *rpcService) handleHistory(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	limit := 20
	if req.Payload != nil {
		var n int
		_ = req.Payload.DataAs(&n)
		if n > 0 && n <= 100 {
			limit = n
		}
	}
	history, err := s.admin.History(ctx, s.room, 0, limit)
	if err != nil {
		return rpcError("ADMIN_ERROR", "GetHistory failed: "+err.Error()), nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("history of %s (%d entries):\n", s.room, len(history)))
	for _, pub := range history {
		b.WriteString(fmt.Sprintf("  #%d %s\n", pub.Offset, pub.Id))
	}
	return &messageloopgo.RPCResponse{
		Payload: messageloopgo.NewMessageWithData("chat.history.response",
			messageloopgo.NewTextData(b.String())),
	}, nil
}

// handleKick force-disconnects a user through the admin Disconnect API and
// announces it to the lobby.
func (s *rpcService) handleKick(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	target := ""
	if req.Payload != nil {
		_ = req.Payload.DataAs(&target)
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return rpcError("INVALID_INPUT", "usage: /kick <user-name>"), nil
	}
	user, ok := chatroom.Users[chatroom.TokenForName(target)]
	if !ok {
		return rpcError("UNKNOWN_USER", "no such user: "+target), nil
	}
	results, err := s.admin.DisconnectUser(ctx, user.ID, 3400, "kicked by admin")
	if err != nil {
		return rpcError("ADMIN_ERROR", "Disconnect failed: "+err.Error()), nil
	}
	kicked := 0
	for _, ok := range results {
		if ok {
			kicked++
		}
	}
	_ = s.admin.PublishToChannel(ctx, s.room, "kick-"+target,
		&chatroom.ChatMessage{User: "system", Kind: "system", Text: target + " was kicked (" + fmt.Sprint(kicked) + " session(s))"},
		false)
	return &messageloopgo.RPCResponse{
		Payload: messageloopgo.NewMessageWithData("chat.kick.response",
			messageloopgo.NewTextData(fmt.Sprintf("kicked %s: %d session(s) disconnected", target, kicked))),
	}, nil
}

// handleWhoami echoes the RPC metadata back. Demonstrates that RPC requests
// reach the backend with id/channel/method context.
func (s *rpcService) handleWhoami(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
	return &messageloopgo.RPCResponse{
		Payload: messageloopgo.NewMessageWithData("chat.whoami.response",
			messageloopgo.NewTextData(fmt.Sprintf("rpc id=%s channel=%s method=%s", req.ID, req.Channel, req.Method))),
	}, nil
}

// rpcError builds an RPC response carrying a sharedv2.Error.
func rpcError(code, message string) *messageloopgo.RPCResponse {
	return &messageloopgo.RPCResponse{
		Error: &sharedv2.Error{Code: code, Type: "rpc_error", Message: message},
	}
}

// loggingMiddleware logs every RPC invocation.
func loggingMiddleware(next messageloopgo.RPCHandlerFunc) messageloopgo.RPCHandlerFunc {
	return func(ctx context.Context, req *messageloopgo.RPCRequest) (*messageloopgo.RPCResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		log.Printf("[rpc] channel=%s method=%s duration=%v", req.Channel, req.Method, time.Since(start))
		return resp, err
	}
}

// ---------------------------------------------------------------------------
// Lifecycle

// lifecycleService logs lifecycle hooks and announces joins/leaves to the
// lobby via the admin API.
type lifecycleService struct {
	admin *chatroom.AdminClient
	room  string
}

// OnConnected implements messageloopgo.LifecycleHandler.
func (s *lifecycleService) OnConnected(ctx context.Context, sessionID, username string) error {
	log.Printf("[lifecycle] connected session=%s user=%s", sessionID, username)
	return s.announce(username + " joined the room")
}

// OnDisconnected implements messageloopgo.LifecycleHandler.
func (s *lifecycleService) OnDisconnected(ctx context.Context, sessionID, username string) error {
	log.Printf("[lifecycle] disconnected session=%s user=%s", sessionID, username)
	return s.announce(username + " left the room")
}

// OnSubscribed implements messageloopgo.LifecycleHandler.
func (s *lifecycleService) OnSubscribed(ctx context.Context, sessionID, channel, username string) error {
	log.Printf("[lifecycle] subscribed session=%s channel=%s user=%s", sessionID, channel, username)
	return nil
}

// OnUnsubscribed implements messageloopgo.LifecycleHandler.
func (s *lifecycleService) OnUnsubscribed(ctx context.Context, sessionID, channel, username string) error {
	log.Printf("[lifecycle] unsubscribed session=%s channel=%s user=%s", sessionID, channel, username)
	return nil
}

// announce publishes a transient system message to the lobby.
func (s *lifecycleService) announce(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.admin.PublishToChannel(ctx, s.room, "sys-"+fmt.Sprint(time.Now().UnixNano()),
		&chatroom.ChatMessage{User: "system", Kind: "system", Text: text}, false)
}
