package admin_test

import (
	"context"
	"testing"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/admin"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	"github.com/stretchr/testify/require"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestPrepareAdminServer_RegistersOnlyAPIService verifies the admin listener
// serves the APIService and not the client streaming service. The pre-D12
// in-package version inspected the server's service table directly; across
// packages the check goes through the wire instead.
func TestPrepareAdminServer_RegistersOnlyAPIService(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server, err := admin.PrepareAdminServer(grpc.Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	startPreparedServer(t, server)

	conn := dialPreparedServer(t, server.Addr())

	_, err = serverv2.NewAPIServiceClient(conn).GetChannels(context.Background(), &serverv2.GetChannelsRequest{})
	require.NoError(t, err, "APIService must be registered on the admin listener")

	stream, err := clientpb.NewMessageLoopServiceClient(conn).MessageLoop(context.Background())
	if err == nil {
		err = stream.Send(&clientpb.InboundMessage{
			Id: "connect-1",
			Envelope: &clientpb.InboundMessage_Connect{
				Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "wrong-port"},
			},
		})
	}
	if err == nil {
		_, err = stream.Recv()
	}
	require.Error(t, err)
	require.Equal(t, codes.Unimplemented, status.Code(err), "MessageLoopService must not be registered on the admin listener")
}

func TestAdminAuthInterceptor(t *testing.T) {
	const token = "super-secret-token"
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	interceptor := grpc.AdminAuthInterceptor(token)

	t.Run("valid token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Bearer "+token))
		resp, err := interceptor(ctx, nil, &googlegrpc.UnaryServerInfo{}, handler)
		require.NoError(t, err)
		require.Equal(t, "ok", resp)
	})

	t.Run("invalid token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Bearer wrong-token"))
		_, err := interceptor(ctx, nil, &googlegrpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("missing metadata", func(t *testing.T) {
		_, err := interceptor(context.Background(), nil, &googlegrpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("missing authorization header", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-other", "v"))
		_, err := interceptor(ctx, nil, &googlegrpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("invalid authorization format", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Token "+token))
		_, err := interceptor(ctx, nil, &googlegrpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}
