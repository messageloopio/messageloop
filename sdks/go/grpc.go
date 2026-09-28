package messageloopgo

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// rawFrame is a type alias for raw protobuf bytes.
type rawFrame []byte

// RawCodec allows sending/receiving raw protobuf bytes without additional wrapping.
// This matches the server's codec for compatibility.
type RawCodec struct{}

func (c *RawCodec) Marshal(v interface{}) ([]byte, error) {
	out, ok := v.(rawFrame)
	if ok {
		return out, nil
	}
	vv, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to marshal, message is %T, want proto.Message or rawFrame", v)
	}
	return proto.Marshal(vv)
}

func (c *RawCodec) Unmarshal(data []byte, v interface{}) error {
	vv, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}
	return proto.Unmarshal(data, vv)
}

// Name returns the codec name used as the gRPC content-subtype. The name is
// package-prefixed instead of the default "proto" so that this codec is never
// registered in the process-global codec registry under the default name
// (which would override the standard proto codec for every gRPC connection in
// the process). The codec is applied per-connection via ForceCodec in
// newGRPCTransport; it must match the codec name used by the server.
func (c *RawCodec) Name() string {
	return "messageloop-proto"
}

// grpcTransport is a gRPC-based transport implementation.
type grpcTransport struct {
	client clientpb.MessageLoopServiceClient
	stream clientpb.MessageLoopService_MessageLoopClient
	conn   *grpc.ClientConn
	sendMu sync.Mutex
	recvMu sync.Mutex
}

// grpcDialOptions returns the dial option carrying the client gRPC transport
// credentials. It is the single derivation point shared by the first dial and
// every reconnect, so a TLS-configured client can never fall back to
// plaintext on a re-dial.
func grpcDialOptions(opts *Options) grpc.DialOption {
	return grpc.WithTransportCredentials(grpcTransportCredentials(opts))
}

// grpcTransportCredentials selects the transport credentials: TLS when the
// client options carry a TLS configuration (WithTLS or WithTLSConfig),
// plaintext insecure otherwise — the default local development path stays
// unchanged.
func grpcTransportCredentials(opts *Options) credentials.TransportCredentials {
	if opts == nil || opts.TLSConfig == nil {
		return insecure.NewCredentials()
	}
	return credentials.NewTLS(grpcTLSConfig(opts))
}

// grpcTLSConfig builds the TLS configuration for a gRPC dial. The caller's
// configuration is cloned so applying InsecureSkipVerify never mutates it
// (the same contract as quicTLSConfig). The server name is taken from the
// dial address unless the configuration sets ServerName.
func grpcTLSConfig(opts *Options) *tls.Config {
	cfg := opts.TLSConfig.Clone()
	if opts.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

// newGRPCTransport creates a new gRPC transport. The caller supplies the
// transport credentials through opts (grpcDialOptions); requiring them at the
// constructor keeps the first dial and every reconnect on the same
// derivation and fails closed instead of silently dialing plaintext if a
// future call site omits them.
func newGRPCTransport(ctx context.Context, addr string, opts ...grpc.DialOption) (*grpcTransport, error) {
	// Force the raw codec per-connection instead of registering it globally
	// in init(): a global "proto" registration would override the standard
	// codec for every gRPC client in the process.
	defaultOpts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.ForceCodec(&RawCodec{})),
	}
	defaultOpts = append(defaultOpts, opts...)

	conn, err := grpc.DialContext(ctx, addr, defaultOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial failed: %w", err)
	}

	client := clientpb.NewMessageLoopServiceClient(conn)
	stream, err := client.MessageLoop(ctx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("create stream failed: %w", err)
	}

	return &grpcTransport{
		client: client,
		stream: stream,
		conn:   conn,
	}, nil
}

// Send sends an InboundMessage to the server.
func (t *grpcTransport) Send(ctx context.Context, msg *clientpb.InboundMessage) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()

	if err := t.stream.Send(msg); err != nil {
		return fmt.Errorf("grpc send error: %w", err)
	}

	return nil
}

// Recv receives an OutboundMessage from the server.
func (t *grpcTransport) Recv(ctx context.Context) (*clientpb.OutboundMessage, error) {
	t.recvMu.Lock()
	defer t.recvMu.Unlock()

	msg, err := t.stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("grpc recv error: %w", err)
	}

	return msg, nil
}

// Close closes the gRPC connection.
func (t *grpcTransport) Close() error {
	if t.conn != nil {
		// Close the stream first
		if t.stream != nil {
			_ = t.stream.CloseSend()
		}
		return t.conn.Close()
	}
	return nil
}

// SetReadDeadline is a no-op for gRPC (not supported).
func (t *grpcTransport) SetReadDeadline(deadline interface{}) error {
	// gRPC streams don't support per-read deadlines
	return nil
}

// SetWriteDeadline is a no-op for gRPC (not supported).
func (t *grpcTransport) SetWriteDeadline(deadline interface{}) error {
	// gRPC streams don't support per-write deadlines
	return nil
}
