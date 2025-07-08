package grpcstream

import (
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	messageloop2 "github.com/deeplooplabs/messageloop/messageloop"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
	"io"
)

type gRPCHandler struct {
	clientv1.UnimplementedMessageLoopServiceServer
	node *messageloop2.Node
}

func (h *gRPCHandler) MessageLoop(stream grpc.BidiStreamingServer[clientv1.ClientMessage, clientv1.ServerMessage]) error {
	transport := newGRPCTransport(stream)
	client, closeFn, err := messageloop2.NewClient(stream.Context(), h.node, transport, messageloop2.DefaultProtoMarshaler)
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

func NewGRPCHandler(node *messageloop2.Node) clientv1.MessageLoopServiceServer {
	return &gRPCHandler{
		node: node,
	}
}
