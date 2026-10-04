package quic

import (
	"errors"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/pkg/transport/framing"
	"github.com/messageloopio/messageloop/shared"
)

// ErrTransportClosed is returned by Write after the transport has been closed.
var ErrTransportClosed = errors.New("quic transport is closed")

// Transport implements session.Transport over one QUIC bidirectional
// stream. The bounded framing discipline (serialized frames, write
// deadlines, disconnect envelope) lives in the shared framing kit; this
// adapter only contributes the QUIC stream and its close semantics.
type Transport struct {
	conn       *quic.Conn
	stream     *quic.Stream
	w          *framing.Writer
	marshaler  shared.Marshaler
	remoteAddr string
}

func newTransport(conn *quic.Conn, stream *quic.Stream, marshaler shared.Marshaler, writeTimeout time.Duration) *Transport {
	remote := ""
	if conn != nil && conn.RemoteAddr() != nil {
		remote = conn.RemoteAddr().String()
	}
	if marshaler == nil {
		marshaler = shared.ProtobufMarshaler{}
	}
	return &Transport{
		conn:       conn,
		stream:     stream,
		w:          framing.NewWriter(stream, stream.SetWriteDeadline, writeTimeout, ErrTransportClosed),
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
	// from the stream before the connection is torn down.
	_ = t.w.WriteDisconnectFrame(t.marshaler, disconnect)

	code := quic.ApplicationErrorCode(disconnect.Code)
	if t.conn != nil {
		return t.conn.CloseWithError(code, disconnect.Reason)
	}
	return nil
}

var _ session.Transport = (*Transport)(nil)
