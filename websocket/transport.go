package websocket

import (
	"github.com/deeploopdev/messageloop"
	"github.com/gorilla/websocket"
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
	return t.conn.Close()
}

var _ messageloop.Transport = (*Transport)(nil)
