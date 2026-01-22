package proxy

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	proxypb "github.com/deeplooplabs/messageloop/genproto/proxy/v1"
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
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4*1024*1024)),
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
	// Apply timeout if not already set in context
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

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
