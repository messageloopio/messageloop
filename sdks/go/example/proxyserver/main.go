// Example proxyserver demonstrates implementing a MessageLoop proxy backend
// with the proxyserver framework from the protocol module: implement only the
// capabilities you need, route RPC by (channel, method) patterns, and let the
// framework handle payload conversion, error mapping and gRPC wiring.
//
// Pass -registry-url to additionally self-register with a MessageLoop server
// that has registry discovery enabled (proxy[].service), so RPC traffic is
// routed to this instance without any manual endpoint configuration.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	proxy "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/protocol/genproto/messageloop/shared/v2"
	"github.com/messageloopio/messageloop/protocol/proxyserver"
	"github.com/messageloopio/messageloop/protocol/registrant"
)

func main() {
	var (
		listenAddr       = flag.String("addr", ":9001", "gRPC listen address")
		registryURL      = flag.String("registry-url", "", "MessageLoop admin base URL (e.g. http://127.0.0.1:8080); empty disables registration")
		registryToken    = flag.String("registry-token", "", "admin listener bearer token (server.http.auth_token)")
		registryService  = flag.String("registry-service", "example-backend", "registry service name; must equal proxy[].service")
		registryID       = flag.String("registry-id", "", "instance ID (stable per process); defaults to hostname")
		advertiseAddress = flag.String("advertise-address", "", "host:port advertised in the registry; defaults to <hostname>:<listen port>")
	)
	flag.Parse()

	mux := proxyserver.NewRPCMux()

	// Middleware runs in registration order. Panic recovery is built into
	// the framework (a panicking handler becomes an in-band INTERNAL_ERROR),
	// so only cross-cutting concerns belong here.
	mux.Use(logging)

	// Routes match (channel, method) glob patterns; first registered wins.
	must(mux.Handle("chat.*", "echo", handleEcho))
	must(mux.Handle("chat.*", "getUser", handleGetUser))
	must(mux.Handle("*", "sum", handleSum))

	grpcServer := grpc.NewServer()
	proxyserver.New(proxyserver.Options{
		RPC:  mux, // capabilities left nil answer gRPC Unimplemented
		Auth: proxyserver.AuthHandlerFunc(auth),
		ACL:  aclHandler{},
		Lifecycle: lifecycleFuncs{
			onConnected: func(ctx context.Context, req *proxy.OnConnectedRequest) error {
				log.Printf("[Lifecycle] connected: session=%s username=%s", req.GetSessionId(), req.GetUsername())
				return nil
			},
			onDisconnected: func(ctx context.Context, req *proxy.OnDisconnectedRequest) error {
				log.Printf("[Lifecycle] disconnected: session=%s username=%s", req.GetSessionId(), req.GetUsername())
				return nil
			},
		},
	}).RegisterGRPC(grpcServer)

	addr := *listenAddr
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}
	log.Printf("Proxy server starting on %s...", addr)

	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Self-register with the MessageLoop registry once the listener is
	// accepting connections. The advertised address must be dialable from
	// the MessageLoop server — a bare ":9001" is rejected by the API.
	var reg *registrant.Registration
	if *registryURL != "" {
		id := *registryID
		if id == "" {
			id, _ = os.Hostname()
		}
		address := *advertiseAddress
		if address == "" {
			host, _ := os.Hostname()
			_, port, _ := net.SplitHostPort(*listenAddr)
			if port == "" {
				port = "9001"
			}
			address = net.JoinHostPort(host, port)
		}
		var err error
		reg, err = registrant.Register(context.Background(), registrant.Options{
			ServerURL: *registryURL,
			Service:   *registryService,
			ID:        id,
			Address:   address,
			Token:     *registryToken,
		})
		if err != nil {
			log.Fatalf("Registry registration failed: %v", err)
		}
		log.Printf("Registered with registry: service=%s id=%s address=%s", *registryService, id, address)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down proxy server...")
	grpcServer.GracefulStop()
	if reg != nil {
		// Leave the routing view before the process exits so MessageLoop
		// stops dialing this instance immediately.
		if err := reg.Close(); err != nil {
			log.Printf("Deregister failed: %v", err)
		}
	}
	log.Println("Server stopped")
}

func handleEcho(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	return &proxy.RPCResponse{Payload: req.GetPayload()}, nil
}

func handleGetUser(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	var userID string
	if err := proxyserver.PayloadJSON(req.GetPayload(), &userID); err != nil {
		userID = proxyserver.PayloadText(req.GetPayload())
	}
	return &proxy.RPCResponse{Payload: proxyserver.NewTextPayload("User: " + userID)}, nil
}

func handleSum(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
	var a, b int
	if _, err := fmt.Sscanf(proxyserver.PayloadText(req.GetPayload()), "%d,%d", &a, &b); err != nil {
		// NewError carries a custom in-band error code back to the caller;
		// a plain error would map to INTERNAL_ERROR instead.
		return nil, proxyserver.NewError(&sharedv2.Error{
			Code:    "INVALID_INPUT",
			Type:    "validation_error",
			Message: "Expected format: a,b (e.g. 10,20)",
		})
	}
	return &proxy.RPCResponse{Payload: proxyserver.NewTextPayload(fmt.Sprintf("%d", a+b))}, nil
}

func auth(ctx context.Context, req *proxy.AuthenticateRequest) (*proxy.UserInfo, error) {
	if req.GetClientId() == "" || req.GetToken() == "" {
		return nil, fmt.Errorf("client ID and token are required")
	}
	return &proxy.UserInfo{
		Id:         "user-" + req.GetClientId(),
		Username:   req.GetClientId(),
		Token:      req.GetToken(),
		ClientType: req.GetClientType(),
		ClientId:   req.GetClientId(),
	}, nil
}

// aclHandler denies private channels without a token; a returned error
// denies the operation (default code PERMISSION_DENIED).
type aclHandler struct{}

func (aclHandler) CheckSubscribeACL(ctx context.Context, req *proxy.SubscribeAclRequest) error {
	const privatePrefix = "private."
	if len(req.GetChannel()) >= len(privatePrefix) && req.GetChannel()[:len(privatePrefix)] == privatePrefix && req.GetToken() == "" {
		return fmt.Errorf("authentication required for private channels")
	}
	return nil
}

func (aclHandler) CheckPublishACL(ctx context.Context, req *proxy.PublishAclRequest) error {
	return nil
}

// lifecycleFuncs adapts the two hooks this example cares about; the other
// two default to success.
type lifecycleFuncs struct {
	onConnected    func(ctx context.Context, req *proxy.OnConnectedRequest) error
	onDisconnected func(ctx context.Context, req *proxy.OnDisconnectedRequest) error
}

func (l lifecycleFuncs) OnConnected(ctx context.Context, req *proxy.OnConnectedRequest) error {
	if l.onConnected == nil {
		return nil
	}
	return l.onConnected(ctx, req)
}

func (l lifecycleFuncs) OnDisconnected(ctx context.Context, req *proxy.OnDisconnectedRequest) error {
	if l.onDisconnected == nil {
		return nil
	}
	return l.onDisconnected(ctx, req)
}

func (lifecycleFuncs) OnSubscribed(ctx context.Context, req *proxy.OnSubscribedRequest) error {
	return nil
}

func (lifecycleFuncs) OnUnsubscribed(ctx context.Context, req *proxy.OnUnsubscribedRequest) error {
	return nil
}

func logging(next proxyserver.RPCHandlerFunc) proxyserver.RPCHandlerFunc {
	return func(ctx context.Context, req *proxy.RPCRequest) (*proxy.RPCResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		log.Printf("[RPC] channel=%s method=%s duration=%v error=%v", req.GetChannel(), req.GetMethod(), time.Since(start), err)
		return resp, err
	}
}

func must(err error) {
	if err != nil {
		log.Fatalf("route registration failed: %v", err)
	}
}
