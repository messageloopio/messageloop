package grpc

import (
	"io"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

type gRPCHandler struct {
	clientpb.UnimplementedMessageLoopServiceServer
	node         *runtime.Node
	writeTimeout time.Duration
}

func (h *gRPCHandler) MessageLoop(stream googlegrpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]) error {
	// Get peer info for remote address
	var remoteAddr string
	if p, ok := peer.FromContext(stream.Context()); ok {
		remoteAddr = p.Addr.String()
	}
	transport := newGRPCTransport(stream, remoteAddr, h.writeTimeout)
	client, closeFn, err := runtime.NewClient(stream.Context(), h.node, transport, shared.ProtobufMarshaler{}, session.WithProtocol("grpc"))
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
			// Soft-fail like the WS/QUIC read loops: HandleMessage already
			// answered with an INTERNAL_ERROR envelope for non-Disconnect
			// errors (and Disconnect errors close the transport, surfacing as
			// a Recv error above). Returning here would tear down the whole
			// stream and leak the raw error text as the gRPC status message.
			log.ErrorContext(ctx, "handle grpc message error", err)
			continue
		}
	}
}

func NewGRPCHandler(node *runtime.Node, opts ...GRPCHandlerOption) clientpb.MessageLoopServiceServer {
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
