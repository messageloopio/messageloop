package kcp_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/kcp"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

func startTestKCPServer(t *testing.T, node *runtime.Node, shards ...int) *kcp.Server {
	t.Helper()
	opts := kcp.Options{
		Addr:         "127.0.0.1:0",
		Insecure:     true,
		WriteTimeout: 5 * time.Second,
	}
	if len(shards) >= 2 {
		opts.DataShards = shards[0]
		opts.ParityShards = shards[1]
	}
	server, err := kcp.NewServer(opts, node)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = server.Close()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	})
	require.NotEmpty(t, server.Addr())
	return server
}

func dialKCP(t *testing.T, addr string, alpn string, shards ...int) net.Conn {
	t.Helper()
	if alpn == "" {
		alpn = shared.ALPNMessageLoopJSON
	}
	dataShards, parityShards := 0, 0
	if len(shards) >= 2 {
		dataShards, parityShards = shards[0], shards[1]
	}
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	if host == "::" || host == "" {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	conn, err := kcp.Dial(ctx, addr, dataShards, parityShards, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // self-signed test server
		NextProtos:         []string{alpn},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendInbound(t *testing.T, conn io.Writer, msg *clientpb.InboundMessage) {
	t.Helper()
	data, err := shared.JSONMarshaler{}.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, shared.WriteFrame(conn, data))
}

func recvOutbound(t *testing.T, conn io.Reader, timeout time.Duration) *clientpb.OutboundMessage {
	t.Helper()
	type result struct {
		msg *clientpb.OutboundMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := shared.ReadFrame(conn, 1<<20)
		if err != nil {
			ch <- result{err: err}
			return
		}
		msg := &clientpb.OutboundMessage{}
		if err := (shared.JSONMarshaler{}).Unmarshal(data, msg); err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{msg: msg}
	}()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for outbound frame")
		return nil
	}
}

func recvUntil(t *testing.T, conn io.Reader, timeout time.Duration, match func(*clientpb.OutboundMessage) bool) *clientpb.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg := recvOutbound(t, conn, time.Until(deadline))
		if match(msg) {
			return msg
		}
	}
	t.Fatal("timed out waiting for matching outbound frame")
	return nil
}

func connectSession(t *testing.T, conn io.ReadWriter, clientID string) *clientpb.Connected {
	t.Helper()
	sendInbound(t, conn, &clientpb.InboundMessage{
		Id: "conn",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: clientID},
		},
	})
	out := recvUntil(t, conn, 5*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetConnected() != nil
	})
	return out.GetConnected()
}

func TestKCP_ConnectSubscribePublish(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server := startTestKCPServer(t, node)
	subConn := dialKCP(t, server.Addr(), "")
	connected := connectSession(t, subConn, "sub-client")
	require.NotEmpty(t, connected.GetSessionId())

	sendInbound(t, subConn, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "kcp-ch"}},
			},
		},
	})
	ack := recvUntil(t, subConn, 5*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetSubscribeAck() != nil
	})
	require.NotNil(t, ack.GetSubscribeAck())

	pubConn := dialKCP(t, server.Addr(), "")
	_ = connectSession(t, pubConn, "pub-client")
	sendInbound(t, pubConn, &clientpb.InboundMessage{
		Id: "pub-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "kcp-ch",
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello kcp"}},
			},
		},
	})
	pubAck := recvUntil(t, pubConn, 5*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetPublishAck() != nil
	})
	require.NotNil(t, pubAck.GetPublishAck())

	pub := recvUntil(t, subConn, 5*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetPublication() != nil
	})
	msgs := pub.GetPublication().GetMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, "kcp-ch", msgs[0].Channel)
	require.Equal(t, "hello kcp", msgs[0].GetPayload().GetText())
}

func TestKCP_ProtobufALPN(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server := startTestKCPServer(t, node)
	conn := dialKCP(t, server.Addr(), shared.ALPNMessageLoopProto)

	data, err := shared.ProtobufMarshaler{}.Marshal(&clientpb.InboundMessage{
		Id: "conn",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "proto-client"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, shared.WriteFrame(conn, data))

	frame, err := shared.ReadFrame(conn, 1<<20)
	require.NoError(t, err)
	out := &clientpb.OutboundMessage{}
	require.NoError(t, shared.ProtobufMarshaler{}.Unmarshal(frame, out))
	require.NotNil(t, out.GetConnected())
	require.NotEmpty(t, out.GetConnected().GetSessionId())
}

func TestKCP_DisconnectCleansUpSubscriptions(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server := startTestKCPServer(t, node)
	conn := dialKCP(t, server.Addr(), "")
	_ = connectSession(t, conn, "cleanup-client")
	sendInbound(t, conn, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "kcp-cleanup"}},
			},
		},
	})
	_ = recvUntil(t, conn, 5*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetSubscribeAck() != nil
	})
	require.Equal(t, 1, node.Hub().NumSubscribers("kcp-cleanup"))

	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool {
		return node.Hub().NumSubscribers("kcp-cleanup") == 0
	}, 5*time.Second, 20*time.Millisecond)
}

func TestKCP_FEEShardsEndToEnd(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	// FEC-enabled listener; the client must dial with matching shard counts.
	server := startTestKCPServer(t, node, 10, 3)
	conn := dialKCP(t, server.Addr(), "", 10, 3)
	connected := connectSession(t, conn, "fec-client")
	require.NotEmpty(t, connected.GetSessionId())
}
