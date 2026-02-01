package messageloopgo

import (
	"context"
	"fmt"
	"sync"

	clientpb "github.com/deeplooplabs/messageloop/genproto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/protobuf/proto"
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

func (c *RawCodec) Name() string {
	return "proto"
}

func init() {
	// Register the raw codec to match the server's codec
	encoding.RegisterCodec(&RawCodec{})
}

// grpcTransport is a gRPC-based transport implementation.
type grpcTransport struct {
	client clientpb.MessagingServiceClient
	stream clientpb.MessagingService_MessageLoopClient
	conn   *grpc.ClientConn
	sendMu sync.Mutex
	recvMu sync.Mutex
}

// newGRPCTransport creates a new gRPC transport.
func newGRPCTransport(ctx context.Context, addr string, opts ...grpc.DialOption) (*grpcTransport, error) {
	// Default options
	defaultOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// Use the default codec which will now use our registered RawCodec
	}
	defaultOpts = append(defaultOpts, opts...)

	conn, err := grpc.DialContext(ctx, addr, defaultOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial failed: %w", err)
	}

	client := clientpb.NewMessagingServiceClient(conn)
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
