package grpcstream

import (
	"io"

	"github.com/deeplooplabs/messageloop"
	clientpb "github.com/deeplooplabs/messageloop/genproto/v1"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
)

type gRPCHandler struct {
	clientpb.UnimplementedMessagingServiceServer
	node *messageloop.Node
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]) error {
	transport := newGRPCTransport(stream)
	client, closeFn, err := messageloop.NewClientSession(stream.Context(), h.node, transport, messageloop.ProtobufMarshaler{})
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

func NewGRPCHandler(node *messageloop.Node) clientpb.MessagingServiceServer {
	return &gRPCHandler{
		node: node,
	}
}
