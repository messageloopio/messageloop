package grpcstream

import (
	"context"
	"testing"

	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestPrepareClientServer_RegistersOnlyMessageLoopService(t *testing.T) {
	node := messageloop.NewNode(nil)
	server, err := PrepareClientServer(Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	serviceInfo := server.grpc.GetServiceInfo()
	require.Contains(t, serviceInfo, clientpb.MessageLoopService_ServiceDesc.ServiceName)
	require.NotContains(t, serviceInfo, serverpb.APIService_ServiceDesc.ServiceName)
}

func TestPrepareAdminServer_RegistersOnlyAPIService(t *testing.T) {
	node := messageloop.NewNode(nil)
	server, err := PrepareAdminServer(Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	serviceInfo := server.grpc.GetServiceInfo()
	require.Contains(t, serviceInfo, serverpb.APIService_ServiceDesc.ServiceName)
	require.NotContains(t, serviceInfo, clientpb.MessageLoopService_ServiceDesc.ServiceName)
}

func TestPrepareClientServer_RejectsPartialTLSConfig(t *testing.T) {
	node := messageloop.NewNode(nil)
	_, err := PrepareClientServer(Options{
		Addr:        "127.0.0.1:0",
		TLSCertFile: "./testdata/server.crt",
	}, node)
	require.EqualError(t, err, "grpc-client-server tls cert_file and key_file must both be set")
}

func TestAdminAuthInterceptor(t *testing.T) {
	const token = "super-secret-token"
	handler := func(ctx context.Context, req any) (any, error) { return "ok", nil }
	interceptor := adminAuthInterceptor(token)

	t.Run("valid token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Bearer "+token))
		resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
		require.NoError(t, err)
		require.Equal(t, "ok", resp)
	})

	t.Run("invalid token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Bearer wrong-token"))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("missing metadata", func(t *testing.T) {
		_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("missing authorization header", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-other", "v"))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("invalid authorization format", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Token "+token))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}
