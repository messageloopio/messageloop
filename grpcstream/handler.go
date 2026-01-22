package grpcstream

import (
	"io"

	clientpb "github.com/deeplooplabs/messageloop/genproto/v1"
	"github.com/deeplooplabs/messageloop"
	"github.com/deeplooplabs/messageloop/protocol"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
)

type gRPCHandler struct {
	clientpb.UnimplementedMessageLoopServiceServer
	node *messageloop.Node
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]) error {
	transport := newGRPCTransport(stream)
	client, closeFn, err := messageloop.NewClient(stream.Context(), h.node, transport, protocol.ProtobufMarshaler{})
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
