package kcp

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// ErrTransportClosed is returned by Write after the transport has been closed.
var ErrTransportClosed = errors.New("kcp transport is closed")

const (
	defaultWriteTimeout = 10 * time.Second
	// disconnectFrameTimeout bounds the disconnect envelope write in Close so
	// a backed-up connection cannot block the close path for a full write timeout.
	disconnectFrameTimeout = 1 * time.Second
)

// Transport implements session.Transport over one TLS-secured KCP session.
// The wire format is identical to the QUIC transport: length-prefixed
// protocol frames over a reliable byte stream.
type Transport struct {
	conn         net.Conn
	marshaler    shared.Marshaler
	remoteAddr   string
	writeMu      sync.Mutex
	writeTimeout time.Duration
	closed       bool
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
		conn:         conn,
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
			_ = t.conn.SetWriteDeadline(time.Now().Add(timeout))
		}
		if err := shared.WriteFrame(t.conn, msg); err != nil {
			return err
		}
	}
	if timeout > 0 {
		_ = t.conn.SetWriteDeadline(time.Time{})
	}
	return nil
}

func (t *Transport) Close(disconnect protocol.Disconnect) error {
	t.writeMu.Lock()
	if t.closed {
		t.writeMu.Unlock()
		return nil
	}
	t.closed = true
	t.writeMu.Unlock()

	// Best-effort disconnect envelope so the client can decode the reason
	// from the stream (same DISCONNECT_ERROR metadata as the QUIC path)
	// before the session is torn down.
	_ = t.writeDisconnectFrame(disconnect)

	if t.conn != nil {
		return t.conn.Close()
	}
	return nil
}

func (t *Transport) writeDisconnectFrame(disconnect protocol.Disconnect) error {
	metadata := &structpb.Struct{Fields: map[string]*structpb.Value{
		"disconnect_code": structpb.NewNumberValue(float64(disconnect.Code)),
	}}
	msg := session.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
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
	_ = t.conn.SetWriteDeadline(time.Now().Add(disconnectFrameTimeout))
	err = shared.WriteFrame(t.conn, frame)
	_ = t.conn.SetWriteDeadline(time.Time{})
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

var _ session.Transport = (*Transport)(nil)
