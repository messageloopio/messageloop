package websocket

import (
	"context"
	"time"

	gorillaws "github.com/gorilla/websocket"
	v2 "github.com/messageloopio/messageloop/v2"
	v2pb "github.com/messageloopio/messageloop/shared/genproto/v2"
)

// TransportV2 implements the v2.Transport interface over a WebSocket connection.
type TransportV2 struct {
	conn     *gorillaws.Conn
	encoding v2pb.Encoding
	done     chan struct{}
}

var _ v2.Transport = (*TransportV2)(nil)

// NewTransportV2 creates a new v2 WebSocket transport.
// encoding determines whether binary or text message type is used.
func NewTransportV2(conn *gorillaws.Conn, encoding v2pb.Encoding) *TransportV2 {
	t := &TransportV2{
		conn:     conn,
		encoding: encoding,
		done:     make(chan struct{}),
	}
	return t
}

// Send sends frame bytes over the WebSocket connection.
// Uses binary message type for Protobuf, text message type for JSON.
func (t *TransportV2) Send(_ context.Context, data []byte) error {
	msgType := gorillaws.TextMessage
	if t.encoding == v2pb.Encoding_ENCODING_PROTOBUF {
		msgType = gorillaws.BinaryMessage
	}
	return t.conn.WriteMessage(msgType, data)
}

// Receive reads the next message from the WebSocket connection.
// Returns an error if the context is cancelled or the connection closes.
func (t *TransportV2) Receive(ctx context.Context) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		_, data, err := t.conn.ReadMessage()
		ch <- result{data, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			// Signal done on any read error (connection closed)
			select {
			case <-t.done:
			default:
				close(t.done)
			}
		}
		return r.data, r.err
	}
}

// Close sends a WebSocket close frame and closes the connection.
func (t *TransportV2) Close(code v2pb.DisconnectCode, reason string) error {
	defer func() {
		select {
		case <-t.done:
		default:
			close(t.done)
		}
	}()

	// Map v2 disconnect code to WebSocket close code
	wsCode := disconnectCodeToWSCode(code)
	closeMsg := gorillaws.FormatCloseMessage(wsCode, reason)

	deadline := time.Now().Add(5 * time.Second)
	writeErr := t.conn.WriteControl(gorillaws.CloseMessage, closeMsg, deadline)

	// Wait for the peer's close frame (or timeout)
	_ = t.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := t.conn.NextReader()
		if gorillaws.IsCloseError(err, gorillaws.CloseNormalClosure, gorillaws.CloseGoingAway) {
			break
		}
		if err != nil {
			break
		}
	}

	if err := t.conn.Close(); err != nil && writeErr == nil {
		return err
	}
	return writeErr
}

// Done returns a channel closed when the transport connection ends.
func (t *TransportV2) Done() <-chan struct{} {
	return t.done
}

// disconnectCodeToWSCode maps v2 DisconnectCode to WebSocket close codes.
// WebSocket close codes 4000-4999 are reserved for application use.
func disconnectCodeToWSCode(code v2pb.DisconnectCode) int {
	switch code {
	case v2pb.DisconnectCode_DISCONNECT_NORMAL:
		return gorillaws.CloseNormalClosure
	case v2pb.DisconnectCode_DISCONNECT_UNAUTHORIZED,
		v2pb.DisconnectCode_DISCONNECT_TOKEN_EXPIRED:
		return 4001
	case v2pb.DisconnectCode_DISCONNECT_KICKED:
		return 4002
	case v2pb.DisconnectCode_DISCONNECT_PROTOCOL_ERROR:
		return gorillaws.CloseProtocolError
	case v2pb.DisconnectCode_DISCONNECT_SERVER_SHUTDOWN,
		v2pb.DisconnectCode_DISCONNECT_SERVER_RESTART:
		return gorillaws.CloseGoingAway
	default:
		return 4000 + int(code)
	}
}
