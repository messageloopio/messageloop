package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/lynx-go/x/log"
	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v1"
)

// GRPCProxy implements Proxy using gRPC transport.
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
		// For secure connections, use TLS with system CA pool
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	}

	conn, err := grpc.NewClient(cfg.Endpoint, dialOpts...)
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

// RPC implements Proxy.RPC.
func (p *GRPCProxy) RPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq, err := req.ToProtoRequest()
	if err != nil {
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}

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

	return FromProtoReply(resp)
}

// Authenticate implements Proxy.Authenticate.
func (p *GRPCProxy) Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC Authenticate request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"client_id", req.ClientID,
	)

	resp, err := p.client.Authenticate(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC authenticate failed: %w", err)
	}

	return FromProtoAuthenticateResponse(resp), nil
}

// SubscribeAcl implements Proxy.SubscribeAcl.
func (p *GRPCProxy) SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

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

// PublishAcl implements Proxy.PublishAcl.
func (p *GRPCProxy) PublishAcl(ctx context.Context, req *PublishAclProxyRequest) (*PublishAclProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()

	log.DebugContext(ctx, "proxying gRPC PublishAcl request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"channel", req.Channel,
	)

	resp, err := p.client.PublishAcl(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC publish acl failed: %w", err)
	}

	return FromProtoPublishAclResponse(resp), nil
}

// OnConnected implements Proxy.OnConnected.
func (p *GRPCProxy) OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

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

// OnSubscribed implements Proxy.OnSubscribed.
func (p *GRPCProxy) OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

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

// OnUnsubscribed implements Proxy.OnUnsubscribed.
func (p *GRPCProxy) OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

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

// OnDisconnected implements Proxy.OnDisconnected.
func (p *GRPCProxy) OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

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
// The caller must defer the returned CancelFunc.
func (p *GRPCProxy) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return context.WithTimeout(ctx, p.timeout)
	}
	return ctx, func() {}
}

// Name implements Proxy.Name.
func (p *GRPCProxy) Name() string {
	return p.name
}

// Close implements Proxy.Close.
func (p *GRPCProxy) Close() error {
	if p.conn != nil {
		return p.conn.Close()
	}
	return nil
}
