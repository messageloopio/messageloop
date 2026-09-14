package kcp

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"

	"github.com/lynx-go/x/log"
	kcpgo "github.com/xtaci/kcp-go/v5"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

const (
	// tlsHandshakeTimeout bounds the TLS handshake layered over the KCP
	// session; a client that never completes the handshake must not hold a
	// goroutine and a UDP slot indefinitely.
	tlsHandshakeTimeout = 10 * time.Second
)

func (s *Server) handleConn(sess *kcpgo.UDPSession) {
	defer func() { _ = sess.Close() }()
	tuneSession(sess)

	tlsConn := tls.Server(&graceCloseConn{Conn: sess}, s.tlsConf)
	ctx := context.Background()
	handshakeCtx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	err := tlsConn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		log.ErrorContext(ctx, "kcp tls handshake error", err)
		return
	}
	defer func() { _ = tlsConn.Close() }()

	alpn := tlsConn.ConnectionState().NegotiatedProtocol
	marshaler := shared.MarshalerForALPN(alpn)

	transport := newTransport(tlsConn, marshaler, s.opts.WriteTimeout)
	client, closeFn, err := runtime.NewClient(ctx, s.node, transport, marshaler, session.WithProtocol("kcp"))
	if err != nil {
		log.ErrorContext(ctx, "create kcp client error", err)
		_ = transport.Close(protocol.Disconnect{Code: protocol.DisconnectInternal.Code, Reason: err.Error()})
		return
	}
	defer func() { _ = closeFn() }()

	ctx = log.Context(ctx, log.FromContext(ctx), "client_id", client.SessionID())
	maxSize := s.node.MaxMessageSize()
	readTimeout := heartbeatReadTimeout(
		s.node.GetHeartbeatConfig().IdleTimeout,
		s.node.GetHeartbeatConfig().PingInterval,
		s.opts.ReadTimeout,
	)

	for {
		if readTimeout > 0 {
			_ = tlsConn.SetReadDeadline(time.Now().Add(readTimeout))
		}
		data, err := shared.ReadFrame(tlsConn, maxSize)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.InfoContext(ctx, "kcp connection closed")
				return
			}
			// The heartbeat read deadline covers KCP's lack of a transport
			// keepalive: a silent peer is disconnected here.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				log.InfoContext(ctx, "kcp read deadline exceeded")
				return
			}
			if errors.Is(err, shared.ErrFrameTooLarge) {
				log.ErrorContext(ctx, "kcp frame too large", err)
				_ = client.Send(ctx, session.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
					out.Envelope = &clientpb.OutboundMessage_Error{
						Error: &sharedpb.Error{
							Code:    "BAD_REQUEST",
							Type:    "client_error",
							Message: "Frame exceeds max message size",
						},
					}
				}))
				return
			}
			log.ErrorContext(ctx, "kcp read error", err)
			return
		}

		msg := &clientpb.InboundMessage{}
		if err := marshaler.Unmarshal(data, msg); err != nil {
			log.ErrorContext(ctx, "decode kcp client message error", err)
			_ = client.Send(ctx, session.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
				out.Envelope = &clientpb.OutboundMessage_Error{
					Error: &sharedpb.Error{
						Code:    "BAD_REQUEST",
						Type:    "client_error",
						Message: "Failed to decode message",
					},
				}
			}))
			continue
		}

		if err := client.HandleMessage(ctx, msg); err != nil {
			log.ErrorContext(ctx, "handle kcp message error", err)
			continue
		}
	}
}

// heartbeatReadTimeout computes the connection read deadline from the
// heartbeat configuration. The rules match the WebSocket and QUIC handlers:
//
//   - idle == 0 && ping == 0: 60s, overridden by an explicit configured value
//   - otherwise: a floor of max(2*idle, 3*ping, 10s); an explicit configured
//     value may raise but never lower it
func heartbeatReadTimeout(idle, ping, configured time.Duration) time.Duration {
	if idle == 0 && ping == 0 {
		if configured > 0 {
			return configured
		}
		return 60 * time.Second
	}
	floor := 10 * time.Second
	if t := 2 * idle; t > floor {
		floor = t
	}
	if t := 3 * ping; t > floor {
		floor = t
	}
	if configured > floor {
		return configured
	}
	return floor
}
