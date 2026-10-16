// Command backend is the ChatRoom demo backend service. It speaks the
// MessageLoop ProxyService gRPC protocol and provides:
//
//   - Authenticate: demo token -> user resolution
//   - RPC: chat.roll (dice), chat.stats / chat.history / chat.kick via the
//     server-side api gRPC API
//   - ACL: private:* channels require a per-subscription token
//   - Lifecycle hooks: announce connect/disconnect to the lobby
//
// It is built on the proxyserver framework from the protocol module.
//
// Run it with: go run ./_examples/chatroom/cmd/backend
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/messageloopio/messageloop/_examples/chatroom/internal/chatroom"
	proxy "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/protocol/genproto/messageloop/shared/v2"
	"github.com/messageloopio/messageloop/protocol/proxyserver"
)

// apiAddr points at the demo server's api gRPC API; align with config.yaml.
const (
	apiAddr     = chatroom.DefaultAPIAddr
	apiToken    = "chatroom-api-token-0123456789"
	backendAddr = ":8090"
)

func main() {
	var addr string
	flag.StringVar(&addr, "addr", backendAddr, "listen address")
	flag.Parse()

	api, err := chatroom.NewAPIClient(context.Background(), apiAddr, apiToken)
	if err != nil {
		log.Fatalf("connect api API: %v", err)
	}
	defer api.Close()

	grpcServer := grpc.NewServer()
	proxyserver.New(proxyserver.Options{
		RPC:       newRPCMux(api),
		Auth:      proxyserver.AuthHandlerFunc((&authService{}).Authenticate),
		ACL:       &aclService{},
		Lifecycle: &lifecycleService{api: api, room: chatroom.Lobby},
	}).RegisterGRPC(grpcServer)

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Printf("chatroom backend listening on %s (api api %s)", addr, apiAddr)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("proxy server: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Auth

// authService resolves a demo token ("token-alice", ...) into a user. A
// returned error becomes the in-band AUTH_ERROR response.
type authService struct{}

func (s *authService) Authenticate(ctx context.Context, req *proxy.AuthenticateRequest) (*proxy.UserInfo, error) {
	if req.GetToken() == "" {
		return nil, rpcError("INVALID_CREDENTIALS", "token is required")
	}
	user, ok := chatroom.LookupByToken(req.GetToken())
	if !ok {
		return nil, rpcError("INVALID_CREDENTIALS", "unknown token: "+req.GetToken())
	}
	log.Printf("[auth] client=%s user=%s role=%s", req.GetClientId(), user.Name, user.Role)
	return &proxy.UserInfo{
		Id:         user.ID,
		Username:   user.Name,
		Token:      req.GetToken(),
		ClientType: req.GetClientType(),
		ClientId:   req.GetClientId(),
	}, nil
}

// ---------------------------------------------------------------------------
// ACL

// aclService gates private:* channels behind a per-subscription/per-publish
// token so the demo can show both allow and deny paths. Any returned error
// denies the operation.
type aclService struct{}

// CheckSubscribeACL implements proxyserver.ACLHandler.
func (s *aclService) CheckSubscribeACL(ctx context.Context, req *proxy.SubscribeAclRequest) error {
	if strings.HasPrefix(req.GetChannel(), chatroom.PrivateChannelPrefix) && req.GetToken() == "" {
		return fmt.Errorf("private channel requires a subscription token")
	}
	return nil
}

// CheckPublishACL implements proxyserver.ACLHandler.
func (s *aclService) CheckPublishACL(ctx context.Context, req *proxy.PublishAclRequest) error {
	if strings.HasPrefix(req.GetChannel(), chatroom.PrivateChannelPrefix) && req.GetToken() == "" {
		return fmt.Errorf("private channel requires a publish token")
	}
	return nil
}

// ---------------------------------------------------------------------------
// RPC

// newRPCMux registers the demo RPC methods with middleware. Routes match
// (channel, method) patterns; these commands are channel-agnostic, so they
// register via HandleMethod.
func newRPCMux(api *chatroom.APIClient) *proxyserver.RPCMux {
	rpc := &rpcService{api: api, room: chatroom.Lobby}
	mux := proxyserver.NewRPCMux()
	mux.Use(loggingMiddleware)
	must(mux.HandleMethod("chat.roll", rpc.handleRoll))
	must(mux.HandleMethod("chat.stats", rpc.handleStats))
	must(mux.HandleMethod("chat.history", rpc.handleHistory))
	must(mux.HandleMethod("chat.kick", rpc.handleKick))
	must(mux.HandleMethod("chat.whoami", rpc.handleWhoami))
	return mux
}

func must(err error) {
	if err != nil {
		log.Fatalf("route registration failed: %v", err)
	}
}

// rpcService implements the chat commands.
type rpcService struct {
	api  *chatroom.APIClient
	room string
}

// handleRoll returns a dice roll. Demonstrates the simplest RPC round-trip.
func (s *rpcService) handleRoll(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	n := rand.Intn(6) + 1
	return &proxy.RPCResponse{
		Payload: proxyserver.NewTextPayload(fmt.Sprintf("dice = %d", n)),
	}, nil
}

// handleStats calls the api GetChannels + GetHistory APIs and returns a
// human-readable summary. Demonstrates backend -> api API integration.
func (s *rpcService) handleStats(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	channels, err := s.api.Channels(ctx)
	if err != nil {
		return nil, rpcError("API_ERROR", "GetChannels failed: "+err.Error())
	}
	var b strings.Builder
	b.WriteString("active channels:\n")
	for _, ch := range channels {
		b.WriteString(fmt.Sprintf("  %s  subscribers=%d", ch.Name, ch.Subscribers))
		if presence, perr := s.api.Presence(ctx, ch.Name); perr == nil && len(presence) > 0 {
			b.WriteString(fmt.Sprintf("  present=%d", len(presence)))
		}
		b.WriteString("\n")
	}
	if history, herr := s.api.History(ctx, s.room, 0, 5); herr == nil {
		b.WriteString(fmt.Sprintf("last %d messages in %s:\n", len(history), s.room))
		for _, pub := range history {
			b.WriteString(fmt.Sprintf("  #%d %s\n", pub.GetPosition().GetOffset(), pub.Id))
		}
	}
	return &proxy.RPCResponse{
		Payload: proxyserver.NewTextPayload(b.String()),
	}, nil
}

// handleHistory returns persisted channel history via the api GetHistory API.
func (s *rpcService) handleHistory(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	limit := 20
	if payload := req.GetPayload(); payload != nil {
		var n int
		_ = proxyserver.PayloadJSON(payload, &n)
		if n > 0 && n <= 100 {
			limit = n
		}
	}
	history, err := s.api.History(ctx, s.room, 0, limit)
	if err != nil {
		return nil, rpcError("API_ERROR", "GetHistory failed: "+err.Error())
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("history of %s (%d entries):\n", s.room, len(history)))
	for _, pub := range history {
		b.WriteString(fmt.Sprintf("  #%d %s\n", pub.GetPosition().GetOffset(), pub.Id))
	}
	return &proxy.RPCResponse{
		Payload: proxyserver.NewTextPayload(b.String()),
	}, nil
}

// handleKick force-disconnects a user through the api Disconnect API and
// announces it to the lobby.
func (s *rpcService) handleKick(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	target := strings.TrimSpace(proxyserver.PayloadText(req.GetPayload()))
	if target == "" {
		return nil, rpcError("INVALID_INPUT", "usage: /kick <user-name>")
	}
	user, ok := chatroom.Users[chatroom.TokenForName(target)]
	if !ok {
		return nil, rpcError("UNKNOWN_USER", "no such user: "+target)
	}
	results, err := s.api.DisconnectUser(ctx, user.ID, 3400, "kicked by api")
	if err != nil {
		return nil, rpcError("API_ERROR", "Disconnect failed: "+err.Error())
	}
	kicked := 0
	for _, ok := range results {
		if ok {
			kicked++
		}
	}
	_ = s.api.PublishToChannel(ctx, s.room, "kick-"+target,
		&chatroom.ChatMessage{User: "system", Kind: "system", Text: target + " was kicked (" + fmt.Sprint(kicked) + " session(s))"},
		false)
	return &proxy.RPCResponse{
		Payload: proxyserver.NewTextPayload(fmt.Sprintf("kicked %s: %d session(s) disconnected", target, kicked)),
	}, nil
}

// handleWhoami echoes the RPC metadata back. Demonstrates that RPC requests
// reach the backend with id/channel/method context.
func (s *rpcService) handleWhoami(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	return &proxy.RPCResponse{
		Payload: proxyserver.NewTextPayload(fmt.Sprintf("rpc id=%s channel=%s method=%s", req.GetId(), req.GetChannel(), req.GetMethod())),
	}, nil
}

// rpcError builds an error carrying a sharedv2.Error; the framework places
// it in the response's in-band error field verbatim.
func rpcError(code, message string) error {
	return proxyserver.NewError(&sharedv2.Error{Code: code, Type: "rpc_error", Message: message})
}

// loggingMiddleware logs every RPC invocation.
func loggingMiddleware(next proxyserver.RPCHandlerFunc) proxyserver.RPCHandlerFunc {
	return func(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		log.Printf("[rpc] channel=%s method=%s duration=%v", req.GetChannel(), req.GetMethod(), time.Since(start))
		return resp, err
	}
}

// ---------------------------------------------------------------------------
// Lifecycle

// lifecycleService logs lifecycle hooks and announces joins/leaves to the
// lobby via the api API.
type lifecycleService struct {
	api  *chatroom.APIClient
	room string
}

// OnConnected implements proxyserver.LifecycleHandler.
func (s *lifecycleService) OnConnected(ctx context.Context, req *proxy.OnConnectedRequest) error {
	log.Printf("[lifecycle] connected session=%s user=%s", req.GetSessionId(), req.GetUsername())
	return s.announce(req.GetUsername() + " joined the room")
}

// OnDisconnected implements proxyserver.LifecycleHandler.
func (s *lifecycleService) OnDisconnected(ctx context.Context, req *proxy.OnDisconnectedRequest) error {
	log.Printf("[lifecycle] disconnected session=%s user=%s", req.GetSessionId(), req.GetUsername())
	return s.announce(req.GetUsername() + " left the room")
}

// OnSubscribed implements proxyserver.LifecycleHandler.
func (s *lifecycleService) OnSubscribed(ctx context.Context, req *proxy.OnSubscribedRequest) error {
	log.Printf("[lifecycle] subscribed session=%s channel=%s user=%s", req.GetSessionId(), req.GetChannel(), req.GetUsername())
	return nil
}

// OnUnsubscribed implements proxyserver.LifecycleHandler.
func (s *lifecycleService) OnUnsubscribed(ctx context.Context, req *proxy.OnUnsubscribedRequest) error {
	log.Printf("[lifecycle] unsubscribed session=%s channel=%s user=%s", req.GetSessionId(), req.GetChannel(), req.GetUsername())
	return nil
}

// announce publishes a transient system message to the lobby.
func (s *lifecycleService) announce(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.api.PublishToChannel(ctx, s.room, "sys-"+fmt.Sprint(time.Now().UnixNano()),
		&chatroom.ChatMessage{User: "system", Kind: "system", Text: text}, false)
}
