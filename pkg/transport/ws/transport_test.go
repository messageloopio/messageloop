package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/protocol"
)

// TestTransport_CloseClosesFDWhenPeerRST is the regression test for P1-B6: a
// peer that resets the TCP connection (instead of closing cleanly) must not
// leave the client-side fd open. Before the fix, Close returned as soon as
// WriteControl failed and skipped conn.Close(), leaking the fd.
func TestTransport_CloseClosesFDWhenPeerRST(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	// A raw peer socket that answers the WebSocket client handshake so
	// gorilla's NewClient returns a usable *websocket.Conn.
	accepted := make(chan net.Conn, 1)
	peerErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			peerErr <- err
			return
		}
		accepted <- c
		peerErr <- answerHandshake(c)
	}()

	clientSide, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientSide.Close() })
	peer := <-accepted

	// Dial sends the upgrade request and blocks for the 101; the peer
	// goroutine answers it concurrently. The dialer runs over the already
	// connected socket so the test keeps the raw fd for the final assertion.
	dialer := websocket.Dialer{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		NetDial: func(network, addr string) (net.Conn, error) {
			return clientSide, nil
		},
	}
	wsConn, _, err := dialer.Dial("ws://localhost/", nil)
	require.NoError(t, err)
	require.NoError(t, <-peerErr)
	transport := newTransport(wsConn, websocket.TextMessage, time.Second)

	// RST the peer: SO_LINGER 0 followed by close aborts the connection
	// instead of the normal FIN handshake.
	tcpPeer := peer.(*net.TCPConn)
	require.NoError(t, tcpPeer.SetLinger(0))
	require.NoError(t, tcpPeer.Close())
	// Give the RST a moment to reach the client socket.
	time.Sleep(50 * time.Millisecond)

	// Close must close the client-side fd on every path, including the
	// WriteControl failure the reset socket triggers.
	_ = transport.Close(protocol.Disconnect{Code: 3500, Reason: "bye"})

	// Reading must return net.ErrClosed — not a fresh "connection reset by
	// peer" from an fd that is still open.
	_, readErr := clientSide.Read(make([]byte, 1))
	require.Error(t, readErr)
	require.ErrorIs(t, readErr, net.ErrClosed, "fd must be closed even when the peer reset the connection")
}

// answerHandshake reads the WebSocket client upgrade request from c and
// replies with a 101 response, so gorilla's NewClient completes its
// handshake over c.
func answerHandshake(c net.Conn) error {
	reader := bufio.NewReader(c)
	key := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "sec-websocket-key:") {
			key = strings.TrimSpace(trimmed[len("sec-websocket-key:"):])
		}
	}
	if key == "" {
		return fmt.Errorf("no Sec-WebSocket-Key in handshake")
	}
	_, err := c.Write([]byte(
		"HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + websocketAccept(key) + "\r\n\r\n"))
	return err
}

// websocketAccept computes the Sec-WebSocket-Accept value for a client key.
func websocketAccept(key string) string {
	h := sha1.New()
	_, _ = h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// TestTransport_CloseUnblocksReadLoop pins the single-reader contract: while
// the handler's read loop is blocked in ReadMessage, Close must not read
// frames itself (that raced with the loop under -race); closing the conn
// unblocks the loop, and the peer still receives the close frame carrying the
// disconnect code.
func TestTransport_CloseUnblocksReadLoop(t *testing.T) {
	upgrader := websocket.Upgrader{}
	readErr := make(chan error, 1)
	closeDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		transport := newTransport(conn, websocket.TextMessage, time.Second)
		// The handler's read loop: the only reader on this connection.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					readErr <- err
					return
				}
			}
		}()
		// Let the read loop block in ReadMessage, then close concurrently.
		time.Sleep(50 * time.Millisecond)
		_ = transport.Close(protocol.Disconnect{Code: 3503, Reason: "shutdown"})
		close(closeDone)
	}))
	defer srv.Close()

	dialer := websocket.Dialer{}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = clientConn.Close() }()

	// The peer receives the close frame carrying the disconnect code.
	_, _, err = clientConn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, 3503, closeErr.Code)

	// The server read loop is unblocked by conn.Close.
	select {
	case err := <-readErr:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("read loop stayed blocked after Close")
	}
	<-closeDone
}

func TestCloseCode_FallsBackToNormalClosureWhenZero(t *testing.T) {
	got := closeCode(protocol.Disconnect{})
	if got != websocket.CloseNormalClosure {
		t.Errorf("closeCode(Disconnect{}) = %d, want %d", got, websocket.CloseNormalClosure)
	}
}

func TestCloseCode_PreservesNonZeroCode(t *testing.T) {
	got := closeCode(protocol.Disconnect{Code: 3500, Reason: "invalid token"})
	if got != 3500 {
		t.Errorf("closeCode() = %d, want 3500", got)
	}
}

func TestDefaultOptions_WriteTimeout(t *testing.T) {
	opts := DefaultOptions()
	if opts.WriteTimeout != DefaultWSWriteTimeout {
		t.Errorf("DefaultOptions().WriteTimeout = %v, want %v", opts.WriteTimeout, DefaultWSWriteTimeout)
	}
}

// TestTransport_WriteTimesOutWhenPeerStopsReading verifies that a slow
// consumer (peer that stops reading) causes Write to fail after the write
// deadline instead of blocking the broadcast forever.
func TestTransport_WriteTimesOutWhenPeerStopsReading(t *testing.T) {
	writeErr := make(chan error, 1)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			writeErr <- err
			return
		}
		transport := newTransport(conn, websocket.TextMessage, 100*time.Millisecond)
		// Push data until the peer's receive window closes and the write
		// deadline fires.
		payload := make([]byte, 32*1024)
		for {
			if err := transport.Write(payload); err != nil {
				writeErr <- err
				return
			}
		}
	}))
	defer srv.Close()

	dialer := websocket.Dialer{}
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, _, err := dialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer func() { _ = clientConn.Close() }()
	// Never read from clientConn: the peer stops reading.

	// The server transport must observe the write deadline and return an
	// error rather than blocking forever.
	select {
	case err := <-writeErr:
		require.Error(t, err, "Write must fail after the write deadline")
	case <-time.After(3 * time.Second):
		t.Fatal("write timeout did not fire: Write kept blocking")
	}
}
