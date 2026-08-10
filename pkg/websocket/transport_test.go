package websocket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/require"
)

func TestCloseCode_FallsBackToNormalClosureWhenZero(t *testing.T) {
	got := closeCode(messageloop.Disconnect{})
	if got != websocket.CloseNormalClosure {
		t.Errorf("closeCode(Disconnect{}) = %d, want %d", got, websocket.CloseNormalClosure)
	}
}

func TestCloseCode_PreservesNonZeroCode(t *testing.T) {
	got := closeCode(messageloop.Disconnect{Code: 3500, Reason: "invalid token"})
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
	defer clientConn.Close()
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
