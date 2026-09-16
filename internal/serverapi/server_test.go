package serverapi_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/serverapi"
	"github.com/messageloopio/messageloop/pkg/transport/grpc"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
)

// TestPrepareServer_RegistersOnlyAPIService verifies the Server API listener
// serves the APIService and not the client streaming service. The pre-D12
// in-package version inspected the server's service table directly; across
// packages the check goes through the wire instead.
func TestPrepareServer_RegistersOnlyAPIService(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server, err := serverapi.PrepareServer(grpc.Options{Addr: "127.0.0.1:0", APIAllowInsecure: true}, node, nil, nil)
	require.NoError(t, err)
	startPreparedServer(t, server)

	conn := dialPreparedServer(t, server.Addr())

	_, err = serverv2.NewAPIServiceClient(conn).GetChannels(context.Background(), &serverv2.GetChannelsRequest{})
	require.NoError(t, err, "APIService must be registered on the Server API listener")

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
	require.Equal(t, codes.Unimplemented, status.Code(err), "MessageLoopService must not be registered on the Server API listener")
}
