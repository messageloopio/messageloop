package grpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	"github.com/stretchr/testify/require"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func startPreparedServer(t *testing.T, server *grpc.Server) {
	t.Helper()
	go func() {
		_ = server.Start(context.Background())
	}()
	t.Cleanup(func() {
		_ = server.Stop(context.Background())
	})
}

func dialPreparedServer(t *testing.T, addr string) *googlegrpc.ClientConn {
	t.Helper()

	var conn *googlegrpc.ClientConn
	require.Eventually(t, func() bool {
		var err error
		conn, err = googlegrpc.NewClient(addr, googlegrpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return false
		}
		// Verify connectivity with a short deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		conn.Connect()
		state := conn.GetState()
		_ = conn.WaitForStateChange(ctx, state)
		return conn.GetState() == connectivity.Ready
	}, 3*time.Second, 25*time.Millisecond)

	t.Cleanup(func() {
		_ = conn.Close()
	})
	return conn
}

func connectClientStream(t *testing.T, conn *googlegrpc.ClientConn, clientID string) (googlegrpc.BidiStreamingClient[clientpb.InboundMessage, clientpb.OutboundMessage], *clientpb.Connected) {
	t.Helper()

	stream, err := clientpb.NewMessageLoopServiceClient(conn).MessageLoop(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = stream.CloseSend()
	})

	err = stream.Send(&clientpb.InboundMessage{
		Id: "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: clientID},
		},
	})
	require.NoError(t, err)

	out, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, out.GetConnected())
	return stream, out.GetConnected()
}

func TestGRPC_ClientPort_MessageLoopConnects(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	clientServer, err := grpc.PrepareClientServer(grpc.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, clientServer)

	conn := dialPreparedServer(t, clientServer.Addr())
	_, connected := connectClientStream(t, conn, "grpc-client")
	require.NotEmpty(t, connected.GetSessionId())
}

func TestGRPC_ClientPort_DoesNotExposeAdminAPI(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	clientServer, err := grpc.PrepareClientServer(grpc.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, clientServer)

	conn := dialPreparedServer(t, clientServer.Addr())
	api := serverv2.NewAPIServiceClient(conn)
	_, err = api.GetChannels(context.Background(), &serverv2.GetChannelsRequest{}, googlegrpc.WaitForReady(false))
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unimplemented, st.Code())
}
