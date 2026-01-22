package proxy

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	proxypb "github.com/fleetlit/messageloop/genproto/proxy/v1"
	"github.com/lynx-go/x/log"
)

// GRPCProxy implements RPCProxy using gRPC transport.
type GRPCProxy struct {
	name     string
	endpoint string
	client   proxypb.ProxyServiceClient
	conn     *grpc.ClientConn
	timeout  time.Duration
}

// NewGRPCProxy creates a new gRPC proxy instance.
func NewGRPCProxy(cfg *ProxyConfig) (*GRPCProxy, error) {
	if cfg.GRPC == nil {
		cfg.GRPC = &GRPCProxyConfig{}
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultRPCTimeout
	}

	dialOpts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4 * 1024 * 1024)),
	}

	if cfg.GRPC.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		// For secure connections, use the system's default TLS credentials
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(nil))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, cfg.Endpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server: %w", err)
	}

	return &GRPCProxy{
		name:     cfg.Name,
		endpoint: cfg.Endpoint,
		client:   proxypb.NewProxyServiceClient(conn),
		conn:     conn,
		timeout:  timeout,
	}, nil
}

// ProxyRPC implements RPCProxy.ProxyRPC.
func (p *GRPCProxy) ProxyRPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC RPC request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"channel", req.Channel,
		"method", req.Method,
	)

	resp, err := p.client.RPC(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC request failed: %w", err)
	}

	return FromProtoReply(resp), nil
}

// Authenticate implements RPCProxy.Authenticate.
func (p *GRPCProxy) Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC Authenticate request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"username", req.Username,
	)

	resp, err := p.client.Authenticate(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC authenticate failed: %w", err)
	}

	return FromProtoAuthenticateResponse(resp), nil
}

// SubscribeAcl implements RPCProxy.SubscribeAcl.
func (p *GRPCProxy) SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC SubscribeAcl request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"channel", req.Channel,
	)

	resp, err := p.client.SubscribeAcl(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC subscribe acl failed: %w", err)
	}

	return FromProtoSubscribeAclResponse(resp), nil
}

// OnConnected implements RPCProxy.OnConnected.
func (p *GRPCProxy) OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC OnConnected request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"session_id", req.SessionID,
	)

	resp, err := p.client.OnConnected(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC on connected failed: %w", err)
	}

	return FromProtoOnConnectedResponse(resp), nil
}

// OnSubscribed implements RPCProxy.OnSubscribed.
func (p *GRPCProxy) OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC OnSubscribed request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"session_id", req.SessionID,
		"channel", req.Channel,
	)

	resp, err := p.client.OnSubscribed(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC on subscribed failed: %w", err)
	}

	return FromProtoOnSubscribedResponse(resp), nil
}

// OnUnsubscribed implements RPCProxy.OnUnsubscribed.
func (p *GRPCProxy) OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC OnUnsubscribed request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"session_id", req.SessionID,
		"channel", req.Channel,
	)

	resp, err := p.client.OnUnsubscribed(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC on unsubscribed failed: %w", err)
	}

	return FromProtoOnUnsubscribedResponse(resp), nil
}

// OnDisconnected implements RPCProxy.OnDisconnected.
func (p *GRPCProxy) OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC OnDisconnected request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"session_id", req.SessionID,
	)

	resp, err := p.client.OnDisconnected(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC on disconnected failed: %w", err)
	}

	return FromProtoOnDisconnectedResponse(resp), nil
}

// withTimeout applies the proxy timeout if not already set in context.
func (p *GRPCProxy) withTimeout(ctx context.Context) context.Context {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		_ = cancel // Caller is responsible for using the context
	}
	return ctx
}

// Name implements RPCProxy.Name.
func (p *GRPCProxy) Name() string {
	return p.name
}

// Close implements RPCProxy.Close.
func (p *GRPCProxy) Close() error {
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}
