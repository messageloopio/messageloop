package messageloopgo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// wsTransport is a WebSocket-based transport implementation.
type wsTransport struct {
	conn      *websocket.Conn
	marshaler Marshaler
	msgType   int
	sendMu    sync.Mutex
	recvMu    sync.Mutex
}

// Marshaler defines the interface for marshaling protocol messages.
// This is a type alias for SDK usage, backed by shared.Marshaler implementations.
type Marshaler = shared.Marshaler

// Re-export shared marshaler implementations for SDK usage.
var (
	JSONMarshaler     = shared.JSONMarshaler{}
	ProtobufMarshaler = shared.ProtobufMarshaler{}
)

// newWSTransport creates a new WebSocket transport.
func newWSTransport(url string, encoding EncodingType, timeout time.Duration) (*wsTransport, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: timeout,
	}

	subprotocol := encoding.Subprotocol()
	header := http.Header{}
	if subprotocol != "" {
		header.Set("Sec-WebSocket-Protocol", subprotocol)
	}

	conn, _, err := dialer.Dial(url, header)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	var marshaler Marshaler
	msgType := websocket.TextMessage
	switch encoding {
	case EncodingProtobuf:
		marshaler = ProtobufMarshaler
		msgType = websocket.BinaryMessage
	default:
		marshaler = JSONMarshaler
	}

	return &wsTransport{
		conn:      conn,
		marshaler: marshaler,
		msgType:   msgType,
	}, nil
}

// Send sends an InboundMessage to the server.
func (t *wsTransport) Send(ctx context.Context, msg *clientpb.InboundMessage) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()

	data, err := t.marshaler.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}

	if err := t.conn.WriteMessage(t.msgType, data); err != nil {
		return fmt.Errorf("write error: %w", err)
	}

	return nil
}

// Recv receives an OutboundMessage from the server.
func (t *wsTransport) Recv(ctx context.Context) (*clientpb.OutboundMessage, error) {
	t.recvMu.Lock()
	defer t.recvMu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		messageType, data, err := t.conn.ReadMessage()
		if err != nil {
			// gorilla surfaces close frames as a *CloseError from the read
			// instead of returning the CloseMessage message type: surface the
			// same typed disconnect the gRPC path decodes from the
			// DISCONNECT_ERROR envelope metadata.
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return nil, &DisconnectError{Code: uint32(closeErr.Code), Reason: closeErr.Text}
			}
			return nil, fmt.Errorf("read error: %w", err)
		}

		// Skip control messages
		if messageType == websocket.CloseMessage {
			return nil, closeFrameError(data)
		}
		if messageType == websocket.PingMessage {
			// Use sendMu to prevent concurrent write with Send()
			t.sendMu.Lock()
			_ = t.conn.WriteMessage(websocket.PongMessage, nil)
			t.sendMu.Unlock()
			continue
		}
		if messageType == websocket.PongMessage {
			continue
		}

		msg := &clientpb.OutboundMessage{}
		if err := t.marshaler.Unmarshal(data, msg); err != nil {
			return nil, fmt.Errorf("unmarshal error: %w", err)
		}

		return msg, nil
	}
}

// Close closes the WebSocket connection.
func (t *wsTransport) Close() error {
	if t.conn != nil {
		// Send a close frame
		_ = t.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		return t.conn.Close()
	}
	return nil
}

// closeFrameError builds a typed disconnect error from a WebSocket close
// frame. The numeric close code is the same disconnect code the server's gRPC
// transport encodes in the DISCONNECT_ERROR envelope metadata, so both
// transports surface the same *DisconnectError. A frame without a code (empty
// or one-byte payload) falls back to the normal closure code, mirroring the
// server's close-frame fallback.
func closeFrameError(payload []byte) error {
	code := uint32(websocket.CloseNormalClosure)
	reason := "connection closed"
	if len(payload) >= 2 {
		code = uint32(binary.BigEndian.Uint16(payload[:2]))
		if len(payload) > 2 {
			reason = string(payload[2:])
		}
	}
	return &DisconnectError{Code: code, Reason: reason}
}

// SetReadDeadline sets the read deadline on the connection.
func (t *wsTransport) SetReadDeadline(deadline time.Time) error {
	if t.conn != nil {
		return t.conn.SetReadDeadline(deadline)
	}
	return nil
}

// SetWriteDeadline sets the write deadline on the connection.
func (t *wsTransport) SetWriteDeadline(deadline time.Time) error {
	if t.conn != nil {
		return t.conn.SetWriteDeadline(deadline)
	}
	return nil
}
