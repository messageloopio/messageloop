package websocket

import (
	"github.com/deeplooplabs/messageloop"
	"github.com/gorilla/websocket"
	"time"
)

type Transport struct {
	conn      *websocket.Conn
	marshaler messageloop.Marshaler
}

func newTransport(conn *websocket.Conn, marshaler messageloop.Marshaler) *Transport {
	return &Transport{conn: conn, marshaler: marshaler}
}

func (t *Transport) Write(msg []byte) error {
	return t.WriteMany(msg)
}

func (t *Transport) WriteMany(msgs ...[]byte) error {
	msgType := websocket.TextMessage
	if t.marshaler.Binary() {
		msgType = websocket.BinaryMessage
	}
	for _, msg := range msgs {
		if err := t.conn.WriteMessage(msgType, msg); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	// 正确的关闭 WebSocket 连接: https://medium.com/@blackhorseya/properly-closing-websocket-connections-in-golang-a902f97716c1

	// Send a WebSocket close message
	deadline := time.Now().Add(5 * time.Second)
	err := t.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(int(disconnect.Code), disconnect.Reason),
		deadline,
	)
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
