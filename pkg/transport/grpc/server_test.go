package grpc

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/runtime"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
)

func TestPrepareClientServer_RegistersOnlyMessageLoopService(t *testing.T) {
	node := runtime.NewNode(nil)
	server, err := PrepareClientServer(Options{Addr: "127.0.0.1:0"}, node)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })

	serviceInfo := server.grpc.GetServiceInfo()
	require.Contains(t, serviceInfo, clientpb.MessageLoopService_ServiceDesc.ServiceName)
	require.NotContains(t, serviceInfo, serverv2.APIService_ServiceDesc.ServiceName)
}
func TestPrepareClientServer_RejectsPartialTLSConfig(t *testing.T) {
	node := runtime.NewNode(nil)

	_, err := PrepareClientServer(Options{
		Addr:        "127.0.0.1:0",
		TLSCertFile: "./testdata/server.crt",
	}, node)
	require.EqualError(t, err, "grpc-client-server tls cert_file and key_file must both be set")
}
