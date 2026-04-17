package grpcstream_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

func startTestGRPCServer(t *testing.T, node *messageloop.Node) serverpb.APIServiceClient {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	s := grpc.NewServer()
	serverpb.RegisterAPIServiceServer(s, grpcstream.NewAPIServiceHandler(node))
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.GracefulStop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return serverpb.NewAPIServiceClient(conn)
}

// mockTransport is a minimal no-op transport for test clients.
type mockTransport struct{ closed bool }

func (m *mockTransport) Write([]byte) error                 { return nil }
func (m *mockTransport) WriteMany(...[]byte) error          { return nil }
func (m *mockTransport) Close(messageloop.Disconnect) error { m.closed = true; return nil }
func (m *mockTransport) RemoteAddr() string                 { return "127.0.0.1:0" }

func addTestClient(t *testing.T, node *messageloop.Node, sessionID, userID string) *messageloop.Client {
	t.Helper()
	ctx := context.Background()
	transport := &mockTransport{}
	client, _, err := messageloop.NewClient(ctx, node, transport, messageloop.ProtobufMarshaler{})
	require.NoError(t, err)
	// Force deterministic IDs for testing
	client.ForceTestIDs(sessionID, userID, "client-"+sessionID)
	require.NoError(t, node.AddClient(client))
	return client
}

func TestGRPC_AdminAPI_PublishAndDisconnect(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	api := startTestGRPCServer(t, node)

	// Add a test client and subscribe
	client := addTestClient(t, node, "sess-1", "user-1")
	require.NoError(t, node.AddSubscription(ctx, "chat", messageloop.NewSubscriber(client, false)))

	// Publish via API
	_, err := api.Publish(ctx, &serverpb.PublishRequest{
		RequestId: "req-1",
		Publications: []*serverpb.Publication{{
			Id:          "pub-1",
			Destination: &serverpb.Publication_Destination{Channels: []string{"chat"}},
			Payload:     &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "hello"}},
		}},
	})
	require.NoError(t, err)

	// Disconnect via API
	resp, err := api.Disconnect(ctx, &serverpb.DisconnectRequest{
		Sessions: []string{"sess-1"},
		Code:     3000,
		Reason:   "test disconnect",
	})
	require.NoError(t, err)
	require.True(t, resp.Results["sess-1"])
}

func TestGRPC_AdminAPI_SubscribeUnsubscribe(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	api := startTestGRPCServer(t, node)
	_ = addTestClient(t, node, "sess-2", "user-2")

	// Subscribe via API
	subResp, err := api.Subscribe(ctx, &serverpb.SubscribeRequest{
		SessionId: "sess-2",
		Channels:  []string{"news", "sports"},
	})
	require.NoError(t, err)
	require.True(t, subResp.Results["news"])
	require.True(t, subResp.Results["sports"])

	// GetChannels — should show the subscribed channels
	chResp, err := api.GetChannels(ctx, &serverpb.GetChannelsRequest{})
	require.NoError(t, err)
	channelNames := make(map[string]int32)
	for _, ch := range chResp.Channels {
		channelNames[ch.Name] = ch.Subscribers
	}
	require.Equal(t, int32(1), channelNames["news"])
	require.Equal(t, int32(1), channelNames["sports"])

	// Unsubscribe via API
	unsubResp, err := api.Unsubscribe(ctx, &serverpb.UnsubscribeRequest{
		SessionId: "sess-2",
		Channels:  []string{"news"},
	})
	require.NoError(t, err)
	require.True(t, unsubResp.Results["news"])

	// GetChannels — "news" should be gone
	chResp2, err := api.GetChannels(ctx, &serverpb.GetChannelsRequest{})
	require.NoError(t, err)
	channelNames2 := make(map[string]int32)
	for _, ch := range chResp2.Channels {
		channelNames2[ch.Name] = ch.Subscribers
	}
	require.Zero(t, channelNames2["news"])
	require.Equal(t, int32(1), channelNames2["sports"])
}

func TestGRPC_AdminAPI_GetHistory(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	api := startTestGRPCServer(t, node)

	// Publish some messages to build history
	for i := 0; i < 3; i++ {
		require.NoError(t, node.Publish("history-ch", []byte("msg"), false))
	}

	// Small delay for broker to process
	time.Sleep(50 * time.Millisecond)

	resp, err := api.GetHistory(ctx, &serverpb.GetHistoryRequest{
		Channel:     "history-ch",
		SinceOffset: 0,
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, resp.Publications, 3)
	require.Equal(t, uint64(1), resp.Publications[0].Offset)
	require.Equal(t, uint64(3), resp.Publications[2].Offset)
}
