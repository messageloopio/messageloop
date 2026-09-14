package messageloopgo

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	kcpgo "github.com/xtaci/kcp-go/v5"

	"github.com/messageloopio/messageloop/shared"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

const (
	defaultKCPMaxFrameSize = 16 << 20
	// kcpCloseGrace bounds how long Close waits before tearing down the KCP
	// session. kcp-go v5 transmits queued packets through an asynchronous
	// pipeline and UDPSession.Close tears it down without waiting, so the
	// final write (the TLS close_notify) is dropped when Close follows a
	// Write immediately. Waiting a few update intervals lets it reach the
	// server so the session is cleaned up right away instead of at the
	// server's heartbeat idle timeout.
	kcpCloseGrace = 100 * time.Millisecond
	// kcpWindowSize is the KCP send/receive window in packets; the server
	// applies the same value to accepted sessions.
	kcpWindowSize = 256
)

// kcpTransport is a KCP-based transport implementation: one UDP KCP session
// per client session, secured by a TLS overlay, carrying the same
// length-prefixed frames as the QUIC transport.
type kcpTransport struct {
	sess      *kcpgo.UDPSession
	conn      *tls.Conn
	marshaler Marshaler
	sendMu    sync.Mutex
	recvMu    sync.Mutex
}

func newKCPTransport(ctx context.Context, addr string, encoding EncodingType, timeout time.Duration, tlsConf *tls.Config, dataShards, parityShards int) (*kcpTransport, error) {
	sess, err := kcpgo.DialWithOptions(addr, nil, dataShards, parityShards)
	if err != nil {
		return nil, fmt.Errorf("kcp dial failed: %w", err)
	}
	sess.SetStreamMode(true)
	// Realtime latency settings (nodelay=1, 10ms clock, fast retransmit,
	// no congestion control); must match the server's accepted sessions.
	sess.SetNoDelay(1, 10, 2, 1)
	sess.SetWindowSize(kcpWindowSize, kcpWindowSize)

	dialCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if tlsConf == nil {
		tlsConf = &tls.Config{}
	} else {
		tlsConf = tlsConf.Clone()
	}
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = []string{encoding.Subprotocol()}
	}
	if tlsConf.MinVersion == 0 {
		tlsConf.MinVersion = tls.VersionTLS12
	}

	conn := tls.Client(&kcpGraceCloseConn{Conn: sess}, tlsConf)
	if err := conn.HandshakeContext(dialCtx); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("kcp tls handshake failed: %w", err)
	}

	var marshaler Marshaler
	switch encoding {
	case EncodingProtobuf:
		marshaler = ProtobufMarshaler
	default:
		marshaler = JSONMarshaler
	}

	return &kcpTransport{
		sess:      sess,
		conn:      conn,
		marshaler: marshaler,
	}, nil
}

// kcpGraceCloseConn makes the first Close wait for the transmit pipeline to
// drain before the underlying session is destroyed (see kcpCloseGrace).
// tls.Conn.Close writes its close_notify through Write and then calls this
// Close, so the grace window covers exactly the final write.
type kcpGraceCloseConn struct {
	net.Conn

	closeOnce sync.Once
	closeErr  error
}

func (c *kcpGraceCloseConn) Close() error {
	c.closeOnce.Do(func() {
		time.Sleep(kcpCloseGrace)
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

func (t *kcpTransport) Send(ctx context.Context, msg *clientpb.InboundMessage) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()

	data, err := t.marshaler.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal error: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = t.conn.SetWriteDeadline(deadline)
		defer t.conn.SetWriteDeadline(time.Time{})
	}
	if err := shared.WriteFrame(t.conn, data); err != nil {
		return fmt.Errorf("kcp write error: %w", err)
	}
	return nil
}

func (t *kcpTransport) Recv(ctx context.Context) (*clientpb.OutboundMessage, error) {
	t.recvMu.Lock()
	defer t.recvMu.Unlock()

	if deadline, ok := ctx.Deadline(); ok {
		_ = t.conn.SetReadDeadline(deadline)
		defer t.conn.SetReadDeadline(time.Time{})
	}

	data, err := shared.ReadFrame(t.conn, defaultKCPMaxFrameSize)
	if err != nil {
		// A remote close (close_notify) or a vanished peer surfaces as EOF,
		// a read timeout, or a connection error; the server encodes the
		// reason in the DISCONNECT_ERROR envelope, which the client loop
		// handles before the transport sees EOF.
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil, &DisconnectError{Reason: "connection closed"}
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("kcp read error: %w", err)
	}

	msg := &clientpb.OutboundMessage{}
	if err := t.marshaler.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("unmarshal error: %w", err)
	}
	return msg, nil
}

func (t *kcpTransport) Close() error {
	if t.conn != nil {
		return t.conn.Close()
	}
	if t.sess != nil {
		return t.sess.Close()
	}
	return nil
}
