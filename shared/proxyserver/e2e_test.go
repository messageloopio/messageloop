package proxyserver

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/messageloop/shared/v2"
)

// TestBufconnEndToEnd registers the framework Server on a real gRPC server
// and dials it with the generated client, proving the wire surface matches
// what the MessageLoop server's GRPCProxy dials (messageloop.proxy.v2.
// ProxyService) end to end.
func TestBufconnEndToEnd(t *testing.T) {
	assert := assert.New(t)

	mux := NewRPCMux()
	require.NoError(t, mux.Handle("chat.*", "echo", func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		return &proxypb.RPCResponse{Payload: NewTextPayload("echo:" + PayloadText(req.GetPayload()))}, nil
	}))

	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	New(Options{
		RPC: mux,
		Auth: AuthHandlerFunc(func(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.UserInfo, error) {
			return &proxypb.UserInfo{Id: "u9", Username: req.GetClientId()}, nil
		}),
	}).RegisterGRPC(grpcServer)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := proxypb.NewProxyServiceClient(conn)

	// Routed RPC round-trips over the wire with the payload intact.
	resp, err := client.RPC(context.Background(), &proxypb.RPCRequest{
		Id: "wire-1", Channel: "chat.room1", Method: "echo",
		Payload: NewTextPayload("hi"),
	})
	require.NoError(t, err)
	assert.Equal("wire-1", resp.Id, "id echoed through the framework")
	assert.Equal("echo:hi", PayloadText(resp.Payload))
	assert.Nil(resp.Error)

	// Unrouted RPC comes back as an in-band NO_ROUTE, not a gRPC error —
	// the caller (MessageLoop server) forwards it to the client verbatim.
	resp, err = client.RPC(context.Background(), &proxypb.RPCRequest{
		Id: "wire-2", Channel: "chat.room1", Method: "missing",
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Error)
	assert.Equal("NO_ROUTE", resp.Error.Code)

	// Authenticate passes the UserInfo through the wire.
	authResp, err := client.Authenticate(context.Background(), &proxypb.AuthenticateRequest{ClientId: "alice", Token: "t"})
	require.NoError(t, err)
	assert.Nil(authResp.Error)
	require.NotNil(t, authResp.UserInfo)
	assert.Equal("u9", authResp.UserInfo.Id)
	assert.Equal("alice", authResp.UserInfo.Username)

	// A capability without a handler answers Unimplemented over the wire.
	_, err = client.SubscribeAcl(context.Background(), &proxypb.SubscribeAclRequest{Channel: "chat.room1"})
	assert.Equal(codes.Unimplemented, status.Code(err), "nil ACL capability must not look like a denial")

	// The wire payload shape stays a sharedv2.Payload oneof, so the
	// MessageLoop server's FromProtoReply consumes it unchanged.
	var _ *sharedv2.Payload = resp.Payload
}
