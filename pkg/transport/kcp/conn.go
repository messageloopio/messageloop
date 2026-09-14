package kcp

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"

	kcpgo "github.com/xtaci/kcp-go/v5"

	"github.com/messageloopio/messageloop/shared"
)

// closeGrace bounds how long a Close waits before tearing down the KCP
// session. kcp-go v5 transmits queued packets through an asynchronous
// pipeline (kcp.flush -> postProcess goroutine -> UDP socket), and
// UDPSession.Close tears that pipeline down without waiting for it. The final
// packets of a conversation — the TLS close_notify, or the server's
// disconnect envelope — are silently dropped when Close follows a Write
// immediately. Waiting a few update intervals here makes those final packets
// reach the peer reliably.
const closeGrace = 100 * time.Millisecond

// graceCloseConn wraps a KCP session so the first Close drains the transmit
// pipeline (by waiting) before the session is destroyed. tls.Conn.Close
// writes its close_notify through Write and then calls this Close, so the
// grace window covers exactly the final write.
type graceCloseConn struct {
	net.Conn

	closeOnce sync.Once
	closeErr  error
}

func (c *graceCloseConn) Close() error {
	c.closeOnce.Do(func() {
		time.Sleep(closeGrace)
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}

// tuneSession puts a KCP session into stream mode with realtime latency
// settings. Stream mode is required for length-prefixed framing: it removes
// the 255-fragment-per-write cap of KCP's message mode and lets consecutive
// frames pack efficiently. Both ends must enable it.
func tuneSession(sess *kcpgo.UDPSession) {
	sess.SetStreamMode(true)
	// nodelay=1 (turn on the no-delay path), interval=10 (internal clock ms),
	// resend=2 (fast retransmit on one duplicate ack), nc=1 (disable
	// congestion control in favor of the window limits).
	sess.SetNoDelay(1, 10, 2, 1)
	sess.SetWindowSize(DefaultWindowSize, DefaultWindowSize)
}

// Dial connects to a KCP server, layers TLS over the session (negotiating
// the MessageLoop ALPN) and completes the handshake. dataShards/parityShards
// must match the server's transport.kcp FEC configuration (0/0 when FEC is
// disabled, the default). The returned net.Conn is also a *tls.Conn.
func Dial(ctx context.Context, addr string, dataShards, parityShards int, tlsConf *tls.Config) (net.Conn, error) {
	sess, err := kcpgo.DialWithOptions(addr, nil, dataShards, parityShards)
	if err != nil {
		return nil, err
	}
	tuneSession(sess)

	tlsConf = tlsConf.Clone()
	if len(tlsConf.NextProtos) == 0 {
		tlsConf.NextProtos = shared.ALPNProtocols()
	}
	conn := tls.Client(&graceCloseConn{Conn: sess}, tlsConf)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = sess.Close()
		return nil, err
	}
	return conn, nil
}
