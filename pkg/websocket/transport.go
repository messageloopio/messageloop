package websocket

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/messageloopio/messageloop"
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
	conn    *websocket.Conn
	msgType int
	writeMu sync.Mutex
}

func newTransport(conn *websocket.Conn, msgType int) *Transport {
	return &Transport{conn: conn, msgType: msgType}
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
		if err := t.conn.WriteMessage(t.msgType, msg); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	t.writeMu.Lock()
	// Send a WebSocket close message
	deadline := time.Now().Add(5 * time.Second)
	err := t.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(int(disconnect.Code), disconnect.Reason),
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
	// Close the TCP connection
	err = t.conn.Close()
	if err != nil {
		return err
	}
	return nil
}

var _ messageloop.Transport = (*Transport)(nil)
