package ws_test

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	// Aliased so the standard runtime.MemStats can be used alongside the
	// node runtime in the same file.
	noderuntime "github.com/messageloopio/messageloop/internal/runtime"
	ws "github.com/messageloopio/messageloop/pkg/transport/ws"
)

// This file is the safety fixture for the WebSocket decompression-bomb fix
// (review finding #2 / mechanism gap G6). With permessage-deflate negotiated,
// gorilla's SetReadLimit only bounds the compressed bytes on the wire, while
// DEFLATE reaches ~1000:1 ratios; without a separate cap on the decompressed
// output a single 64KB frame can expand to ~64MB of buffered data. The tests
// below pin the bounded-read behavior so that reverting to an unbounded
// ReadMessage turns the suite red.

const (
	// bombDecompressedSize is the decompressed size of the bomb payload. It
	// must exceed max_message_size (64KB) by orders of magnitude while its
	// compressed wire form stays below max_message_size, so only the
	// decompression path (not gorilla's wire readLimit) can reject it.
	bombDecompressedSize = 16 << 20 // 16MB

	// bombHeapGrowthBudget caps how much the server may allocate while
	// processing the bomb frame. Rationale: the bounded read allocates at
	// most the capped 64KB+1 ReadAll buffer, the flate 32KB decompression
	// window and some session/log/test-process churn — measured at a stable
	// ~1.4MB across runs — while a regressed unbounded ReadMessage allocates
	// the full 16MB decompressed payload (typically 2x that in buffer
	// growth). The 4MB budget sits ~3x above the measured fixed path and
	// ~4x below the vulnerable path. TotalAlloc (cumulative bytes
	// allocated) is used instead of HeapAlloc so the assertion cannot be
	// defeated by a GC freeing the bomb buffer before sampling.
	bombHeapGrowthBudget = 4 << 20 // 4MB
)

// startCompressedWSServer is startTestWSServer with permessage-deflate
// enabled on the upgrader.
func startCompressedWSServer(t *testing.T, node *noderuntime.Node) *httptest.Server {
	t.Helper()
	opts := ws.Options{WsPath: "/ws", Compression: true, CheckOrigin: func(r *http.Request) bool { return true }}
	handler := ws.NewHandler(node, opts)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handler.ServeHTTP)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// dialWSCompressed dials with a permessage-deflate-capable client and fails
// the test unless the extension was actually negotiated — otherwise the
// fixture would silently exercise the uncompressed path.
func dialWSCompressed(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"messageloop+json"}, EnableCompression: true}
	conn, resp, err := dialer.Dial(url, nil)
	require.NoError(t, err)
	require.Contains(t, resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate",
		"permessage-deflate must be negotiated for this fixture to be meaningful")
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestWebSocket_CompressedNormalMessagesRoundTrip guards the fix against
// regressions in the happy path: with compression enabled on both ends,
// connect/subscribe/publish still work end to end.
func TestWebSocket_CompressedNormalMessagesRoundTrip(t *testing.T) {
	ctx := t.Context()
	node := noderuntime.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	server := startCompressedWSServer(t, node)

	sub := dialWSCompressed(t, server)
	sendJSON(t, sub, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "comp-sub", "version": "2.0.0"},
	})
	require.NotNil(t, readJSON(t, sub, 2*time.Second)["connected"])

	sendJSON(t, sub, map[string]any{
		"id":        "sub",
		"subscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "comp"}}},
	})
	require.NotNil(t, readJSON(t, sub, 2*time.Second)["subscribe_ack"])

	pub := dialWSCompressed(t, server)
	sendJSON(t, pub, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "comp-pub", "version": "2.0.0"},
	})
	_ = readJSON(t, pub, 2*time.Second)

	sendJSON(t, pub, map[string]any{
		"id":      "pub",
		"publish": map[string]any{"channel": "comp", "payload": map[string]any{"text": "compressed hello"}},
	})
	require.NotNil(t, readJSON(t, pub, 2*time.Second)["publish_ack"])

	require.NotNil(t, readPublication(t, sub, 2*time.Second)["publication"])
}

// TestWebSocket_CompressedMessageAtMaxSizeStillAccepted pins the boundary of
// the bounded read: a message whose decompressed size is exactly max_message_size
// must be accepted (readAllBounded reads maxSize+1 to tell "exactly at the cap"
// apart from "over it"), and the connection must stay usable afterwards.
func TestWebSocket_CompressedMessageAtMaxSizeStillAccepted(t *testing.T) {
	ctx := t.Context()
	node := noderuntime.NewNode(nil) // MaxMessageSize defaults to 64KB
	require.NoError(t, node.Run(ctx))

	server := startCompressedWSServer(t, node)
	conn := dialWSCompressed(t, server)

	sendJSON(t, conn, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "boundary-client", "version": "2.0.0"},
	})
	require.NotNil(t, readJSON(t, conn, 2*time.Second)["connected"])

	// Pad the publish envelope with the payload text so the whole frame is
	// exactly max_message_size bytes once marshaled.
	msg := map[string]any{
		"id": "boundary",
		"publish": map[string]any{
			"channel": "boundary",
			"payload": map[string]any{"text": ""},
		},
	}
	skeleton, err := json.Marshal(msg)
	require.NoError(t, err)
	pad := noderuntime.DefaultMaxMessageSize - len(skeleton)
	require.GreaterOrEqual(t, pad, 0, "envelope skeleton must fit under the cap")
	msg["publish"].(map[string]any)["payload"].(map[string]any)["text"] = strings.Repeat("A", pad)
	wire, err := json.Marshal(msg)
	require.NoError(t, err)
	require.Equal(t, noderuntime.DefaultMaxMessageSize, len(wire))

	// A write error is acceptable here (the kernel may observe the server's
	// state), but the message itself must not get the connection killed.
	if err := conn.WriteMessage(websocket.TextMessage, wire); err != nil {
		t.Fatalf("write of exactly-max-size message failed: %v", err)
	}

	// Drain the publish response (ack or error — session policy is not under
	// test) and then verify the read loop is still healthy via ping/pong.
	for i := 0; i < 5; i++ {
		resp := readJSON(t, conn, 2*time.Second)
		if resp["pong"] != nil {
			return
		}
		if _, isPing := resp["ping"]; isPing {
			continue
		}
		sendJSON(t, conn, map[string]any{"id": "alive", "ping": map[string]any{}})
	}
	t.Fatal("connection did not answer ping after an exactly-max-size message")
}

// TestWebSocket_DecompressionBombRejectedWithBoundedMemory is the core safety
// fixture: a highly compressible frame far above max_message_size once
// decompressed, but far below it on the wire, must be rejected by closing the
// connection, and the server's allocation while processing it must stay
// orders of magnitude below the decompressed size. The memory assertion is
// what keeps the fix honest: a future revert to unbounded ReadMessage
// allocates the full decompressed payload and fails this test even if the
// message would eventually be rejected by the decoder.
func TestWebSocket_DecompressionBombRejectedWithBoundedMemory(t *testing.T) {
	ctx := t.Context()
	node := noderuntime.NewNode(nil) // MaxMessageSize defaults to 64KB
	require.NoError(t, node.Run(ctx))

	server := startCompressedWSServer(t, node)
	conn := dialWSCompressed(t, server)

	// Establish the session first so the rejection is attributable to the
	// bomb frame rather than to connection setup.
	sendJSON(t, conn, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "bomb-client", "version": "2.0.0"},
	})
	require.NotNil(t, readJSON(t, conn, 2*time.Second)["connected"])

	// Build the bomb: a valid publish envelope whose payload decompresses
	// far above the cap but compresses far below it. Validity matters — a
	// regressed server would fully buffer and process it, which the client
	// would observe as a publish_ack instead of a disconnect.
	bomb := map[string]any{
		"id": "bomb",
		"publish": map[string]any{
			"channel": "bomb",
			"payload": map[string]any{"text": strings.Repeat("A", bombDecompressedSize)},
		},
	}
	wire, err := json.Marshal(bomb)
	require.NoError(t, err)
	require.Greater(t, len(wire), noderuntime.DefaultMaxMessageSize,
		"decompressed bomb must exceed max_message_size")

	// The wire form must stay below max_message_size, so only the
	// decompression path (never gorilla's wire readLimit) can reject the
	// frame — otherwise the fixture would pass for the wrong reason. gorilla
	// compresses with flate level 1 (defaultCompressionLevel), which mirrors
	// what the client will actually put on the wire.
	var compressed bytes.Buffer
	fw, _ := flate.NewWriter(&compressed, 1)
	_, err = fw.Write(wire)
	require.NoError(t, err)
	require.NoError(t, fw.Close())
	require.Less(t, compressed.Len(), noderuntime.DefaultMaxMessageSize,
		"compressed bomb must stay below max_message_size so the wire readLimit does not reject it first")
	t.Logf("bomb frame: %d bytes decompressed, %d bytes compressed on the wire", len(wire), compressed.Len())

	// Quiesce GC so the allocation delta measures the server's work on the
	// bomb frame rather than unrelated garbage collection timing.
	runtime.GC()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	// The server may tear the connection down as soon as the cap is hit —
	// before the client finishes writing the whole compressed frame — so a
	// write error is equally valid evidence of an early rejection.
	if err := conn.WriteMessage(websocket.TextMessage, wire); err != nil {
		t.Logf("client write error (server rejected the frame early): %v", err)
	}

	// The server must close the connection instead of acknowledging the
	// message.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, readErr := conn.ReadMessage()
	require.Error(t, readErr, "server must close the connection on an oversized decompressed frame")

	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)

	growth := after.TotalAlloc - before.TotalAlloc
	t.Logf("server allocation while processing the bomb: %d bytes (budget %d, decompressed %d)",
		growth, uint64(bombHeapGrowthBudget), len(wire))
	require.Less(t, growth, uint64(bombHeapGrowthBudget),
		"server allocated %d bytes for a %d-byte decompressed frame; the read loop appears to be unbounded",
		growth, len(wire))
}
