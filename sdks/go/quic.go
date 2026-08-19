package messageloopgo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

const defaultQUICMaxFrameSize = 16 << 20

// quicTransport is a QUIC-based transport implementation. Each client
// session is one QUIC connection with a single bidirectional stream of
// length-prefixed protocol frames.
type quicTransport struct {
	conn      *quic.Conn
	stream    *quic.Stream
	marshaler Marshaler
	sendMu    sync.Mutex
	recvMu    sync.Mutex
}

func newQUICTransport(ctx context.Context, addr string, encoding EncodingType, timeout time.Duration, tlsConf *tls.Config) (*quicTransport, error) {
	if tlsConf == nil {
		tlsConf = &tls.Config{}
	} else {
		tlsConf = tlsConf.Clone()
	}
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = []string{encoding.Subprotocol()}
	}
	if tlsConf.MinVersion == 0 {
		tlsConf.MinVersion = tls.VersionTLS13
	}

	dialCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	conn, err := quic.DialAddr(dialCtx, addr, tlsConf, nil)
	if err != nil {
		return nil, fmt.Errorf("quic dial failed: %w", err)
	}
	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "open stream failed")
		return nil, fmt.Errorf("quic open stream failed: %w", err)
	}

	var marshaler Marshaler
	switch encoding {
	case EncodingProtobuf:
		marshaler = ProtobufMarshaler
	default:
		marshaler = JSONMarshaler
	}

	return &quicTransport{
		conn:      conn,
		stream:    stream,
		marshaler: marshaler,
	}, nil
}

func (t *quicTransport) Send(ctx context.Context, msg *clientpb.InboundMessage) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()

	data, err := t.marshaler.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = t.stream.SetWriteDeadline(deadline)
		defer t.stream.SetWriteDeadline(time.Time{})
	}
	if err := shared.WriteFrame(t.stream, data); err != nil {
		return fmt.Errorf("quic write error: %w", err)
	}
	return nil
}

func (t *quicTransport) Recv(ctx context.Context) (*clientpb.OutboundMessage, error) {
	t.recvMu.Lock()
	defer t.recvMu.Unlock()

	if deadline, ok := ctx.Deadline(); ok {
		_ = t.stream.SetReadDeadline(deadline)
		defer t.stream.SetReadDeadline(time.Time{})
	}

	data, err := shared.ReadFrame(t.stream, defaultQUICMaxFrameSize)
	if err != nil {
		var appErr *quic.ApplicationError
		if errors.As(err, &appErr) {
			return nil, &DisconnectError{Code: uint32(appErr.ErrorCode), Reason: appErr.ErrorMessage}
		}
		return nil, fmt.Errorf("quic read error: %w", err)
	}

	msg := &clientpb.OutboundMessage{}
	if err := t.marshaler.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("unmarshal error: %w", err)
	}
	return msg, nil
}

func (t *quicTransport) Close() error {
	if t.conn != nil {
		return t.conn.CloseWithError(0, "")
	}
	return nil
}
