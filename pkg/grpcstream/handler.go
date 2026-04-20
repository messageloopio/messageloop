package grpcstream

import (
	"io"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

type gRPCHandler struct {
	clientpb.UnimplementedMessageLoopServiceServer
	node         *messageloop.Node
	writeTimeout time.Duration
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]) error {
	// Get peer info for remote address
	var remoteAddr string
	if p, ok := peer.FromContext(stream.Context()); ok {
		remoteAddr = p.Addr.String()
	}
	transport := newGRPCTransport(stream, remoteAddr, h.writeTimeout)
	client, closeFn, err := messageloop.NewClient(stream.Context(), h.node, transport, messageloop.ProtobufMarshaler{}, messageloop.WithProtocol("grpc"))
	if err != nil {
		return err
	}
	defer func() { _ = closeFn() }()
	ctx := stream.Context()
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())

	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// Check if transport was closed intentionally.
			select {
			case <-transport.closeCh:
				return nil
			default:
			}
			return err
		}
		if err := client.HandleMessage(ctx, in); err != nil {
			return err
		}
	}
}

func NewGRPCHandler(node *messageloop.Node, opts ...GRPCHandlerOption) clientpb.MessageLoopServiceServer {
	h := &gRPCHandler{
		node: node,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// GRPCHandlerOption configures the gRPC handler.
type GRPCHandlerOption func(*gRPCHandler)

// WithWriteTimeout sets the write timeout for gRPC streams.
func WithWriteTimeout(d time.Duration) GRPCHandlerOption {
	return func(h *gRPCHandler) {
		h.writeTimeout = d
	}
}
