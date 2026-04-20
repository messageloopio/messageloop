package grpcstream

import (
	"testing"

	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	"github.com/stretchr/testify/require"
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
