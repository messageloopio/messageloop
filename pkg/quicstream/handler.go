package quicstream

import (
	"context"
	"errors"
	"io"
	"net"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/quic-go/quic-go"
)

// streamAcceptTimeout bounds how long the server waits for the client to
// open the session stream after the QUIC handshake completes.
const streamAcceptTimeout = 10 * time.Second

func (s *Server) handleConn(conn *quic.Conn) {
	defer func() { _ = conn.CloseWithError(0, "") }()

	ctx := conn.Context()
	acceptCtx, cancel := context.WithTimeout(ctx, streamAcceptTimeout)
	stream, err := conn.AcceptStream(acceptCtx)
	cancel()
	if err != nil {
		log.ErrorContext(ctx, "quic accept stream error", err)
		return
	}
	defer func() { _ = stream.Close() }()

	alpn := conn.ConnectionState().TLS.NegotiatedProtocol
	var marshaler messageloop.Marshaler
	if alpn == shared.ALPNMessageLoopProto {
		marshaler = messageloop.ProtobufMarshaler{}
	} else {
		marshaler = messageloop.ProtoJSONMarshaler
	}

	transport := newTransport(conn, stream, marshaler, s.opts.WriteTimeout)
	client, closeFn, err := messageloop.NewClient(ctx, s.node, transport, marshaler, messageloop.WithProtocol("quic"))
	if err != nil {
		log.ErrorContext(ctx, "create quic client error", err)
		_ = conn.CloseWithError(quic.ApplicationErrorCode(messageloop.DisconnectInternal.Code), err.Error())
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
			_ = stream.SetReadDeadline(time.Now().Add(readTimeout))
		}
		data, err := shared.ReadFrame(stream, maxSize)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.InfoContext(ctx, "quic stream closed")
				return
			}
			var appErr *quic.ApplicationError
			if errors.As(err, &appErr) {
				log.InfoContext(ctx, "quic connection closed", "code", appErr.ErrorCode, "reason", appErr.ErrorMessage)
				return
			}
			if errors.Is(err, shared.ErrFrameTooLarge) {
				log.ErrorContext(ctx, "quic frame too large", err)
				_ = client.Send(ctx, messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
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
			log.ErrorContext(ctx, "quic read error", err)
			return
		}

		msg := &clientpb.InboundMessage{}
		if err := marshaler.Unmarshal(data, msg); err != nil {
			log.ErrorContext(ctx, "decode quic client message error", err)
			_ = client.Send(ctx, messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
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
			log.ErrorContext(ctx, "handle quic message error", err)
			continue
		}
	}
}

// heartbeatReadTimeout computes the stream read deadline from the heartbeat
// configuration. The rules match the WebSocket handler:
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
