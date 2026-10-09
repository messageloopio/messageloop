package kcp

import (
	"errors"
	"net"
	"time"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/pkg/transport/framing"
	"github.com/messageloopio/messageloop/shared"
)

// ErrTransportClosed is returned by Write after the transport has been closed.
var ErrTransportClosed = errors.New("kcp transport is closed")

// Transport implements session.Transport over one TLS-secured KCP session.
// The wire format is identical to the QUIC transport — length-prefixed
// protocol frames over a reliable byte stream — and so is the bounded
// framing discipline, which lives in the shared framing kit. This adapter
// only contributes the net.Conn and its close semantics.
type Transport struct {
	conn       net.Conn
	w          *framing.Writer
	marshaler  shared.Marshaler
	remoteAddr string
}

func newTransport(conn net.Conn, marshaler shared.Marshaler, writeTimeout time.Duration) *Transport {
	remote := ""
	if conn != nil && conn.RemoteAddr() != nil {
		remote = conn.RemoteAddr().String()
	}
	if marshaler == nil {
		marshaler = shared.ProtobufMarshaler{}
	}
	return &Transport{
		conn:       conn,
		w:          framing.NewWriter(conn, conn.SetWriteDeadline, writeTimeout, ErrTransportClosed),
		marshaler:  marshaler,
		remoteAddr: remote,
	}
}

func (t *Transport) RemoteAddr() string {
	return t.remoteAddr
}

func (t *Transport) Write(msg []byte) error {
	return t.WriteMany(msg)
}

func (t *Transport) WriteMany(msgs ...[]byte) error {
	return t.w.WriteMany(msgs...)
}

func (t *Transport) Close(disconnect protocol.Disconnect) error {
	if !t.w.MarkClosed() {
		return nil
	}

	// Best-effort disconnect envelope so the client can decode the reason
	// from the stream before the session is torn down.
	_ = t.w.WriteDisconnectFrame(t.marshaler, disconnect)

	if t.conn != nil {
		return t.conn.Close()
	}
	return nil
}

var _ session.Transport = (*Transport)(nil)
