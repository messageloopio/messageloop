// Package framing holds the shared machinery every length-prefixed
// transport adapter needs: the bounded write path (mutex + per-write
// deadline + closed flag) and the DISCONNECT_ERROR envelope. The QUIC and
// KCP adapters are thin variations over Writer (byte stream + deadline
// setter); the gRPC adapter reuses the envelope builder for its shutdown
// frame. Before this package each adapter carried a private copy of both.
package framing

import (
	"fmt"
	"io"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/session"
	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// DefaultWriteTimeout bounds one WriteMany when the adapter was built with
// no explicit write timeout.
const DefaultWriteTimeout = 10 * time.Second

// DisconnectFrameTimeout bounds the disconnect envelope write in Close so a
// backed-up stream cannot block the close path for a full write timeout.
const DisconnectFrameTimeout = 1 * time.Second

// SetWriteDeadlineFunc sets the write deadline on the underlying stream.
type SetWriteDeadlineFunc func(time.Time) error

// Writer is the bounded length-prefixed write half shared by the QUIC and
// KCP adapters: serialized frames over one reliable byte stream, each write
// deadline-bounded, with a closed flag that fails subsequent writes and a
// best-effort disconnect frame on close.
type Writer struct {
	w            io.Writer
	setDeadline  SetWriteDeadlineFunc // nil = the stream has no deadline support
	closedErr    error                // transport-specific closed sentinel
	writeMu      sync.Mutex
	writeTimeout time.Duration
	closed       bool
}

// NewWriter wraps w with the bounded write discipline. closedErr is the
// sentinel returned after MarkClosed (each adapter keeps its own
// transport-worded error).
func NewWriter(w io.Writer, setDeadline SetWriteDeadlineFunc, writeTimeout time.Duration, closedErr error) *Writer {
	return &Writer{
		w:            w,
		setDeadline:  setDeadline,
		closedErr:    closedErr,
		writeTimeout: writeTimeout,
	}
}

// EffectiveTimeout resolves the per-write budget: the configured value or
// DefaultWriteTimeout.
func (fw *Writer) EffectiveTimeout() time.Duration {
	if fw.writeTimeout > 0 {
		return fw.writeTimeout
	}
	return DefaultWriteTimeout
}

// WriteMany writes each msg as one length-prefixed frame, serialized with
// other writes and bounded by one deadline for the whole batch.
func (fw *Writer) WriteMany(msgs ...[]byte) error {
	fw.writeMu.Lock()
	defer fw.writeMu.Unlock()
	if fw.closed {
		return fw.closedErr
	}
	timeout := fw.EffectiveTimeout()
	for _, msg := range msgs {
		fw.setDeadlineLocked(time.Now().Add(timeout))
		if err := shared.WriteFrame(fw.w, msg); err != nil {
			return err
		}
	}
	fw.setDeadlineLocked(time.Time{})
	return nil
}

// MarkClosed flips the closed flag and reports whether this call was the
// one that closed it (Close stays idempotent at the adapter level).
func (fw *Writer) MarkClosed() bool {
	fw.writeMu.Lock()
	defer fw.writeMu.Unlock()
	if fw.closed {
		return false
	}
	fw.closed = true
	return true
}

// WriteDisconnectFrame marshals and writes the DISCONNECT_ERROR envelope
// best-effort, bounded by DisconnectFrameTimeout instead of the write
// timeout, so a backed-up stream cannot stall the close path.
func (fw *Writer) WriteDisconnectFrame(m shared.Marshaler, disconnect protocol.Disconnect) error {
	frame, err := marshalDisconnect(m, disconnect)
	if err != nil {
		return err
	}
	fw.writeMu.Lock()
	defer fw.writeMu.Unlock()
	fw.setDeadlineLocked(time.Now().Add(DisconnectFrameTimeout))
	err = shared.WriteFrame(fw.w, frame)
	fw.setDeadlineLocked(time.Time{})
	return err
}

func (fw *Writer) setDeadlineLocked(t time.Time) {
	if fw.setDeadline != nil {
		_ = fw.setDeadline(t)
	}
}

func marshalDisconnect(m shared.Marshaler, disconnect protocol.Disconnect) ([]byte, error) {
	data, err := m.Marshal(DisconnectMessage(int32(disconnect.Code), disconnect.Reason))
	if err != nil {
		return nil, fmt.Errorf("marshal disconnect frame: %w", err)
	}
	return data, nil
}

// DisconnectMessage builds the outbound DISCONNECT_ERROR envelope carrying
// the numeric disconnect code (3500-3514) in metadata: the QUIC/KCP byte
// streams and the gRPC shutdown frame all deliver the close reason through
// it, because none of those transports has a close frame the way WebSocket
// does.
func DisconnectMessage(code int32, reason string) *clientpb.OutboundMessage {
	metadata := &structpb.Struct{Fields: map[string]*structpb.Value{
		"disconnect_code": structpb.NewNumberValue(float64(code)),
	}}
	return session.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:     "DISCONNECT_ERROR",
				Type:     "transport_error",
				Message:  reason,
				Metadata: metadata,
			},
		}
	})
}
