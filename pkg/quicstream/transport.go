package quicstream

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/types/known/structpb"
)

// ErrTransportClosed is returned by Write after the transport has been closed.
var ErrTransportClosed = errors.New("quic transport is closed")

const (
	defaultWriteTimeout = 10 * time.Second
	// disconnectFrameTimeout bounds the disconnect envelope write in Close so
	// a backed-up stream cannot block the close path for a full write timeout.
	disconnectFrameTimeout = 1 * time.Second
)

// Transport implements messageloop.Transport over one QUIC bidirectional stream.
type Transport struct {
	conn         *quic.Conn
	stream       *quic.Stream
	marshaler    messageloop.Marshaler
	remoteAddr   string
	writeMu      sync.Mutex
	writeTimeout time.Duration
	closed       bool
}

func newTransport(conn *quic.Conn, stream *quic.Stream, marshaler messageloop.Marshaler, writeTimeout time.Duration) *Transport {
	remote := ""
	if conn != nil && conn.RemoteAddr() != nil {
		remote = conn.RemoteAddr().String()
	}
	if marshaler == nil {
		marshaler = messageloop.ProtobufMarshaler{}
	}
	return &Transport{
		conn:         conn,
		stream:       stream,
		marshaler:    marshaler,
		remoteAddr:   remote,
		writeTimeout: writeTimeout,
	}
}

func (t *Transport) RemoteAddr() string {
	return t.remoteAddr
}

func (t *Transport) Write(msg []byte) error {
	return t.WriteMany(msg)
}

func (t *Transport) WriteMany(msgs ...[]byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.closed {
		return ErrTransportClosed
	}
	timeout := t.effectiveTimeout()
	for _, msg := range msgs {
		if timeout > 0 {
			_ = t.stream.SetWriteDeadline(time.Now().Add(timeout))
		}
		if err := shared.WriteFrame(t.stream, msg); err != nil {
			return err
		}
	}
	if timeout > 0 {
		_ = t.stream.SetWriteDeadline(time.Time{})
	}
	return nil
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	t.writeMu.Lock()
	if t.closed {
		t.writeMu.Unlock()
		return nil
	}
	t.closed = true
	t.writeMu.Unlock()

	// Best-effort disconnect envelope so the client can decode the reason
	// from the stream (same DISCONNECT_ERROR metadata as the gRPC path)
	// before the connection is torn down.
	_ = t.writeDisconnectFrame(disconnect)

	code := quic.ApplicationErrorCode(disconnect.Code)
	if t.conn != nil {
		return t.conn.CloseWithError(code, disconnect.Reason)
	}
	return nil
}

func (t *Transport) writeDisconnectFrame(disconnect messageloop.Disconnect) error {
	metadata := &structpb.Struct{Fields: map[string]*structpb.Value{
		"disconnect_code": structpb.NewNumberValue(float64(disconnect.Code)),
	}}
	msg := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:     "DISCONNECT_ERROR",
				Type:     "transport_error",
				Message:  disconnect.Reason,
				Metadata: metadata,
			},
		}
	})
	frame, err := t.marshalDisconnect(msg)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = t.stream.SetWriteDeadline(time.Now().Add(disconnectFrameTimeout))
	err = shared.WriteFrame(t.stream, frame)
	_ = t.stream.SetWriteDeadline(time.Time{})
	return err
}

func (t *Transport) marshalDisconnect(msg *clientpb.OutboundMessage) ([]byte, error) {
	data, err := t.marshaler.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal disconnect frame: %w", err)
	}
	return data, nil
}

func (t *Transport) effectiveTimeout() time.Duration {
	if t.writeTimeout > 0 {
		return t.writeTimeout
	}
	return defaultWriteTimeout
}

var _ messageloop.Transport = (*Transport)(nil)
