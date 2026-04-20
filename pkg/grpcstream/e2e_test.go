package grpcstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestGRPC_ClientStream_PubSub verifies the full gRPC client-streaming pub/sub
// flow: client A subscribes to a channel, client B publishes, client A receives.
func TestGRPC_ClientStream_PubSub(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server, err := grpcstream.PrepareClientServer(grpcstream.Options{
		Addr:         "127.0.0.1:0",
		WriteTimeout: 5 * time.Second,
	}, node)
	require.NoError(t, err)
	startPreparedServer(t, server)

	conn := dialPreparedServer(t, server.Addr())

	// Client A: connect + subscribe
	streamA, connA := connectClientStream(t, conn, "sub-client")
	require.NotEmpty(t, connA.GetSessionId())

	err = streamA.Send(&clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "grpc-ch"}},
			},
		},
	})
	require.NoError(t, err)
	outA, err := streamA.Recv()
	require.NoError(t, err)
	require.NotNil(t, outA.GetSubscribeAck(), "expected subscribe_ack, got: %v", outA)

	// Client B: connect + publish
	streamB, _ := connectClientStream(t, conn, "pub-client")
	err = streamB.Send(&clientpb.InboundMessage{
		Id: "pub-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "grpc-ch",
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "hello gRPC"}},
			},
		},
	})
	require.NoError(t, err)
	outB, err := streamB.Recv()
	require.NoError(t, err)
	require.NotNil(t, outB.GetPublishAck(), "expected publish_ack, got: %v", outB)

	// Client A: should receive the publication
	outPub, err := streamA.Recv()
	require.NoError(t, err)
	require.NotNil(t, outPub.GetPublication(), "expected publication, got: %v", outPub)

	msgs := outPub.GetPublication().GetMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, "grpc-ch", msgs[0].Channel)
}

// TestGRPC_ClientStream_SubscribeUnsubscribe verifies that unsubscribing stops
// message delivery through the gRPC streaming transport.
func TestGRPC_ClientStream_SubscribeUnsubscribe(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server, err := grpcstream.PrepareClientServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, server)

	conn := dialPreparedServer(t, server.Addr())

	// Connect and subscribe
	stream, _ := connectClientStream(t, conn, "unsub-test")
	err = stream.Send(&clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "temp-ch"}},
			},
		},
	})
	require.NoError(t, err)
	out, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, out.GetSubscribeAck())

	require.Equal(t, 1, node.Hub().NumSubscribers("temp-ch"))

	// Unsubscribe
	err = stream.Send(&clientpb.InboundMessage{
		Id: "unsub-1",
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "temp-ch"}},
			},
		},
	})
	require.NoError(t, err)
	out, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, out.GetUnsubscribeAck())

	require.Equal(t, 0, node.Hub().NumSubscribers("temp-ch"))
}

// TestGRPC_ClientStream_MultipleSubscribers verifies broadcast delivery to
// multiple gRPC streaming subscribers on the same channel.
func TestGRPC_ClientStream_MultipleSubscribers(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server, err := grpcstream.PrepareClientServer(grpcstream.Options{
		Addr:         "127.0.0.1:0",
		WriteTimeout: 5 * time.Second,
	}, node)
	require.NoError(t, err)
	startPreparedServer(t, server)

	conn := dialPreparedServer(t, server.Addr())
	const numSubs = 3

	streams := make([]grpc.BidiStreamingClient[clientpb.InboundMessage, clientpb.OutboundMessage], numSubs)
	for i := 0; i < numSubs; i++ {
		s, _ := connectClientStream(t, conn, "multi-sub-"+string(rune('A'+i)))
		streams[i] = s
		err = s.Send(&clientpb.InboundMessage{
			Id: "sub",
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{
					Subscriptions: []*clientpb.Subscription{{Channel: "multi-ch"}},
				},
			},
		})
		require.NoError(t, err)
		out, err := s.Recv()
		require.NoError(t, err)
		require.NotNil(t, out.GetSubscribeAck())
	}

	// Publish from a separate client
	pub, _ := connectClientStream(t, conn, "multi-publisher")
	err = pub.Send(&clientpb.InboundMessage{
		Id: "pub",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "multi-ch",
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "broadcast"}},
			},
		},
	})
	require.NoError(t, err)
	_, err = pub.Recv() // publish_ack
	require.NoError(t, err)

	// All subscribers should receive the publication
	for i := 0; i < numSubs; i++ {
		out, err := streams[i].Recv()
		require.NoError(t, err)
		require.NotNil(t, out.GetPublication(), "subscriber %d did not receive publication", i)
	}
}

// TestGRPC_ClientStream_DisconnectCleansUp verifies that closing a gRPC stream
// removes the client's subscriptions from the hub.
func TestGRPC_ClientStream_DisconnectCleansUp(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server, err := grpcstream.PrepareClientServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, server)

	conn := dialPreparedServer(t, server.Addr())

	// Create a separate dial so we can close it independently.
	conn2 := dialPreparedServer(t, server.Addr())

	stream, err := clientpb.NewMessageLoopServiceClient(conn2).MessageLoop(context.Background())
	require.NoError(t, err)

	// Connect
	err = stream.Send(&clientpb.InboundMessage{
		Id:       "conn",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "cleanup-grpc"}},
	})
	require.NoError(t, err)
	out, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, out.GetConnected())

	// Subscribe
	err = stream.Send(&clientpb.InboundMessage{
		Id: "sub",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "cleanup-ch"}},
			},
		},
	})
	require.NoError(t, err)
	out, err = stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, out.GetSubscribeAck())

	require.Equal(t, 1, node.Hub().NumSubscribers("cleanup-ch"))

	// Close the stream
	require.NoError(t, stream.CloseSend())

	// Wait for the hub to be cleaned up
	require.Eventually(t, func() bool {
		return node.Hub().NumSubscribers("cleanup-ch") == 0
	}, 3*time.Second, 50*time.Millisecond)

	// Verify we can still connect from conn (original dial)
	_, connected := connectClientStream(t, conn, "still-alive")
	require.NotEmpty(t, connected.GetSessionId())
}
