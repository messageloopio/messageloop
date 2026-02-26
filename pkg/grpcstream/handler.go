package grpcstream

import (
	"io"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

type gRPCHandler struct {
	clientpb.UnimplementedMessageLoopServiceServer
	node *messageloop.Node
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]) error {
	// Get peer info for remote address
	var remoteAddr string
	if p, ok := peer.FromContext(stream.Context()); ok {
		remoteAddr = p.Addr.String()
	}
	transport := newGRPCTransport(stream, remoteAddr)
	client, closeFn, err := messageloop.NewClientSession(stream.Context(), h.node, transport, messageloop.ProtobufMarshaler{}, messageloop.WithProtocol("grpc"))
	if err != nil {
		return err
	}
	defer closeFn()
	ctx := stream.Context()
	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())

	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-transport.closeCh:
			return nil

		default:
			in, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if err := client.HandleMessage(ctx, in); err != nil {
				return err
			}
		}
	}
}

func NewGRPCHandler(node *messageloop.Node) clientpb.MessageLoopServiceServer {
	return &gRPCHandler{
		node: node,
	}
}
