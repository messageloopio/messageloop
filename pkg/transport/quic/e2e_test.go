package quic_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/pkg/transport/quic"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

func startTestQUICServer(t *testing.T, node *runtime.Node) *quic.Server {
	t.Helper()
	server, err := quic.NewServer(quic.Options{
		Addr:         "127.0.0.1:0",
		Insecure:     true,
		WriteTimeout: 5 * time.Second,
	}, node)
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

func dialQUIC(t *testing.T, addr string, alpn string) (*quicgo.Conn, *quicgo.Stream) {
	t.Helper()
	if alpn == "" {
		alpn = shared.ALPNMessageLoopJSON
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	if host == "::" || host == "" {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	conn, err := quicgo.DialAddr(ctx, addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{alpn},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseWithError(0, "") })
	stream, err := conn.OpenStreamSync(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stream.Close() })
	return conn, stream
}

func sendInbound(t *testing.T, stream io.Writer, msg *clientpb.InboundMessage) {
	t.Helper()
	data, err := shared.JSONMarshaler{}.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, shared.WriteFrame(stream, data))
}

func recvOutbound(t *testing.T, stream io.Reader, timeout time.Duration) *clientpb.OutboundMessage {
	t.Helper()
	type result struct {
		msg *clientpb.OutboundMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := shared.ReadFrame(stream, 1<<20)
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

func recvUntil(t *testing.T, stream io.Reader, timeout time.Duration, match func(*clientpb.OutboundMessage) bool) *clientpb.OutboundMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		msg := recvOutbound(t, stream, time.Until(deadline))
		if match(msg) {
			return msg
		}
	}
	t.Fatal("timed out waiting for matching outbound frame")
	return nil
}

func connectSession(t *testing.T, stream *quicgo.Stream, clientID string) *clientpb.Connected {
	t.Helper()
	sendInbound(t, stream, &clientpb.InboundMessage{
		Id: "conn",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: clientID},
		},
	})
	out := recvUntil(t, stream, 3*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetConnected() != nil
	})
	return out.GetConnected()
}

func TestQUIC_ConnectSubscribePublish(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server := startTestQUICServer(t, node)
	_, subStream := dialQUIC(t, server.Addr(), "")
	connected := connectSession(t, subStream, "sub-client")
	require.NotEmpty(t, connected.GetSessionId())

	sendInbound(t, subStream, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "quic-ch"}},
			},
		},
	})
	ack := recvUntil(t, subStream, 3*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetSubscribeAck() != nil
	})
	require.NotNil(t, ack.GetSubscribeAck())

	_, pubStream := dialQUIC(t, server.Addr(), "")
	_ = connectSession(t, pubStream, "pub-client")
	sendInbound(t, pubStream, &clientpb.InboundMessage{
		Id: "pub-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "quic-ch",
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello quic"}},
			},
		},
	})
	pubAck := recvUntil(t, pubStream, 3*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetPublishAck() != nil
	})
	require.NotNil(t, pubAck.GetPublishAck())

	pub := recvUntil(t, subStream, 3*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetPublication() != nil
	})
	msgs := pub.GetPublication().GetMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, "quic-ch", msgs[0].Channel)
	require.Equal(t, "hello quic", msgs[0].GetPayload().GetText())
}

func TestQUIC_ProtobufALPN(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server := startTestQUICServer(t, node)
	_, stream := dialQUIC(t, server.Addr(), shared.ALPNMessageLoopProto)

	data, err := shared.ProtobufMarshaler{}.Marshal(&clientpb.InboundMessage{
		Id: "conn",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{Version: "2.0.0", ClientId: "proto-client"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, shared.WriteFrame(stream, data))

	frame, err := shared.ReadFrame(stream, 1<<20)
	require.NoError(t, err)
	out := &clientpb.OutboundMessage{}
	require.NoError(t, shared.ProtobufMarshaler{}.Unmarshal(frame, out))
	require.NotNil(t, out.GetConnected())
	require.NotEmpty(t, out.GetConnected().GetSessionId())
}

func TestQUIC_DisconnectCleansUpSubscriptions(t *testing.T) {
	ctx := t.Context()
	node := runtime.NewNode(nil)
	require.NoError(t, node.Run(ctx))
	t.Cleanup(node.Shutdown)

	server := startTestQUICServer(t, node)
	conn, stream := dialQUIC(t, server.Addr(), "")
	_ = connectSession(t, stream, "cleanup-client")
	sendInbound(t, stream, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "quic-cleanup"}},
			},
		},
	})
	_ = recvUntil(t, stream, 3*time.Second, func(m *clientpb.OutboundMessage) bool {
		return m.GetSubscribeAck() != nil
	})
	require.Equal(t, 1, node.Hub().NumSubscribers("quic-cleanup"))

	require.NoError(t, conn.CloseWithError(0, "client done"))
	require.Eventually(t, func() bool {
		return node.Hub().NumSubscribers("quic-cleanup") == 0
	}, 3*time.Second, 20*time.Millisecond)
}
