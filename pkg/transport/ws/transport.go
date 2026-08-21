package ws

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/session"
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
	// gorilla/websocket documents WriteControl (and Close) as safe to call
	// concurrently with every other method, so Close must NOT take writeMu:
	// a writerLoop stuck in WriteMessage (a write deadline of zero is
	// constructible in tests) would otherwise block Close — and the deferred
	// conn.Close — indefinitely. The 5s WriteControl deadline bounds the
	// close-frame attempt; closing the conn afterwards unblocks any stuck
	// writer and the read loop, which owns all reads on this connection
	// (gorilla/websocket requires a single reader, so Close must not drain
	// frames itself).
	defer func() { _ = t.conn.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	return t.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(closeCode(disconnect), disconnect.Reason),
		deadline,
	)
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
