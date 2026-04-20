package grpcstream_test

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func startPreparedServer(t *testing.T, server *grpcstream.Server) {
	t.Helper()
	go func() {
		_ = server.Start(context.Background())
	}()
	t.Cleanup(func() {
		server.Stop(context.Background())
	})
}

func dialPreparedServer(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()

	var conn *grpc.ClientConn
	require.Eventually(t, func() bool {
		var err error
		conn, err = grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

func connectClientStream(t *testing.T, conn *grpc.ClientConn, clientID string) (grpc.BidiStreamingClient[clientpb.InboundMessage, clientpb.OutboundMessage], *clientpb.Connected) {
	t.Helper()

	stream, err := clientpb.NewMessageLoopServiceClient(conn).MessageLoop(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = stream.CloseSend()
	})

	err = stream.Send(&clientpb.InboundMessage{
		Id: "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: clientID},
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
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	clientServer, err := grpcstream.PrepareClientServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, clientServer)

	conn := dialPreparedServer(t, clientServer.Addr())
	_, connected := connectClientStream(t, conn, "grpc-client")
	require.NotEmpty(t, connected.GetSessionId())
}

func TestGRPC_AdminPort_DisconnectsSharedClientSession(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	clientServer, err := grpcstream.PrepareClientServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	adminServer, err := grpcstream.PrepareAdminServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, clientServer)
	startPreparedServer(t, adminServer)

	clientConn := dialPreparedServer(t, clientServer.Addr())
	stream, connected := connectClientStream(t, clientConn, "grpc-admin-target")

	adminConn := dialPreparedServer(t, adminServer.Addr())
	api := serverpb.NewAPIServiceClient(adminConn)
	resp, err := api.Disconnect(context.Background(), &serverpb.DisconnectRequest{
		Sessions: []string{connected.GetSessionId()},
		Code:     3001,
		Reason:   "admin test",
	})
	require.NoError(t, err)
	require.True(t, resp.Results[connected.GetSessionId()])

	out, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, out.GetError())
	if errObj := out.GetError(); errObj != nil {
		require.Equal(t, "DISCONNECT_ERROR", errObj.Code)
	}
}

func TestGRPC_ClientPort_DoesNotExposeAdminAPI(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	clientServer, err := grpcstream.PrepareClientServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, clientServer)

	conn := dialPreparedServer(t, clientServer.Addr())
	api := serverpb.NewAPIServiceClient(conn)
	_, err = api.GetChannels(context.Background(), &serverpb.GetChannelsRequest{}, grpc.WaitForReady(false))
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestGRPC_AdminPort_DoesNotExposeMessageLoopStream(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	adminServer, err := grpcstream.PrepareAdminServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, adminServer)

	conn := dialPreparedServer(t, adminServer.Addr())
	stream, err := clientpb.NewMessageLoopServiceClient(conn).MessageLoop(context.Background())
	if err == nil {
		err = stream.Send(&clientpb.InboundMessage{
			Id: "connect-1",
			Envelope: &clientpb.InboundMessage_Connect{
				Connect: &clientpb.Connect{ClientId: "wrong-port"},
			},
		})
	}
	if err == nil {
		_, err = stream.Recv()
	}
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unimplemented, st.Code())
}

func TestGRPC_AdminPort_ServesUnaryAPI(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	adminServer, err := grpcstream.PrepareAdminServer(grpcstream.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, adminServer)

	conn := dialPreparedServer(t, adminServer.Addr())
	api := serverpb.NewAPIServiceClient(conn)
	_, err = api.GetChannels(context.Background(), &serverpb.GetChannelsRequest{})
	require.NoError(t, err)

	// Sanity check that the admin port remains unary-capable after the split.
	_, err = api.GetPresence(context.Background(), &serverpb.GetPresenceRequest{Channel: "chat"})
	require.NoError(t, err)

	_ = emptypb.Empty{}
}
