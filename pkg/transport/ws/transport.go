package ws

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/internal/protocol"
)

// msgTypeFromSubprotocol returns the WebSocket message type for the given negotiated subprotocol.
// "messageloop+proto" uses binary frames; all others use text frames.
func msgTypeFromSubprotocol(subprotocol string) int {
	if subprotocol == "messageloop+proto" {
		return websocket.BinaryMessage
	}
	return websocket.TextMessage
}

type Transport struct {
	conn         *websocket.Conn
	msgType      int
	writeMu      sync.Mutex
	writeTimeout time.Duration
}

func newTransport(conn *websocket.Conn, msgType int, writeTimeout time.Duration) *Transport {
	return &Transport{conn: conn, msgType: msgType, writeTimeout: writeTimeout}
}

func (t *Transport) RemoteAddr() string {
	return t.conn.RemoteAddr().String()
}

func (t *Transport) Write(msg []byte) error {
	return t.WriteMany(msg)
}

func (t *Transport) WriteMany(msgs ...[]byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	for _, msg := range msgs {
		if t.writeTimeout > 0 {
			_ = t.conn.SetWriteDeadline(time.Now().Add(t.writeTimeout))
		}
		if err := t.conn.WriteMessage(t.msgType, msg); err != nil {
			return err
		}
	}
	if t.writeTimeout > 0 {
		_ = t.conn.SetWriteDeadline(time.Time{})
	}
	return nil
}

func (t *Transport) Close(disconnect protocol.Disconnect) error {
	// Always close the underlying connection, even when the peer has already
	// torn the socket down (e.g. RST): WriteControl and the read loop below
	// fail early on such sockets, and skipping conn.Close() would leak the fd.
	defer func() { _ = t.conn.Close() }()

	t.writeMu.Lock()
	// Send a WebSocket close message
	deadline := time.Now().Add(5 * time.Second)
	err := t.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(closeCode(disconnect), disconnect.Reason),
		deadline,
	)
	t.writeMu.Unlock()
	if err != nil {
		return err
	}

	// Set deadline for reading the next message
	err = t.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		return err
	}
	// Read messages until the close message is confirmed
	for {
		_, _, err = t.conn.NextReader()
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			break
		}
		if err != nil {
			break
		}
	}
	return nil
}

var _ session.Transport = (*Transport)(nil)

// closeCode returns the WebSocket close code for the given Disconnect.
// A zero Code is reserved by RFC 6455 ("no status code") but is used for
// normal closures by the server core, so it falls back to 1000
// (CloseNormalClosure) to keep the reason string deliverable.
func closeCode(disconnect protocol.Disconnect) int {
	if disconnect.Code == 0 {
		return websocket.CloseNormalClosure
	}
	return int(disconnect.Code)
}
