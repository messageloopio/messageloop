package websocket_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	ws "github.com/messageloopio/messageloop/pkg/websocket"
	"github.com/stretchr/testify/require"
)

func startTestWSServer(t *testing.T, node *messageloop.Node) *httptest.Server {
	t.Helper()
	opts := ws.Options{WsPath: "/ws", CheckOrigin: func(r *http.Request) bool { return true }}
	handler := ws.NewHandler(node, opts)
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handler.ServeHTTP)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func dialWS(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"messageloop+json"}}
	conn, _, err := dialer.Dial(url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sendJSON(t *testing.T, conn *websocket.Conn, msg any) {
	t.Helper()
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))
}

func readJSON(t *testing.T, conn *websocket.Conn, timeout time.Duration) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

func TestWebSocket_ConnectAndPublish(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)

	// Client 1: subscriber
	conn1 := dialWS(t, server)
	sendJSON(t, conn1, map[string]any{
		"id":      "1",
		"connect": map[string]any{"client_id": "sub-client"},
	})
	resp1 := readJSON(t, conn1, 2*time.Second)
	require.NotNil(t, resp1["connected"])

	// Subscribe client 1 to "chat"
	sendJSON(t, conn1, map[string]any{
		"id":        "2",
		"subscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "chat"}}},
	})
	subAck := readJSON(t, conn1, 2*time.Second)
	require.NotNil(t, subAck["subscribe_ack"])

	// Client 2: publisher
	conn2 := dialWS(t, server)
	sendJSON(t, conn2, map[string]any{
		"id":      "1",
		"connect": map[string]any{"client_id": "pub-client"},
	})
	_ = readJSON(t, conn2, 2*time.Second) // Connected

	// Publish a message to "chat"
	sendJSON(t, conn2, map[string]any{
		"id":      "3",
		"publish": map[string]any{"channel": "chat", "payload": map[string]any{"text": "hello world"}},
	})
	pubAck := readJSON(t, conn2, 2*time.Second)
	require.NotNil(t, pubAck["publish_ack"])

	// Client 1 should receive the publication
	pub := readJSON(t, conn1, 2*time.Second)
	require.NotNil(t, pub["publication"], "expected publication message, got: %v", pub)
}

func TestWebSocket_RateLimiting(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(&config.Server{
		Limits: config.Limits{MaxPublishesPerSecond: 2},
	})
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)
	conn := dialWS(t, server)

	// Connect
	sendJSON(t, conn, map[string]any{
		"id":      "1",
		"connect": map[string]any{"client_id": "rate-test"},
	})
	_ = readJSON(t, conn, 2*time.Second)

	// Rapid-fire publish
	gotRateLimited := false
	for i := 0; i < 20; i++ {
		sendJSON(t, conn, map[string]any{
			"id":      "p" + strings.Repeat("0", i),
			"publish": map[string]any{"channel": "test", "payload": map[string]any{"text": "msg"}},
		})
		resp := readJSON(t, conn, 2*time.Second)
		if errObj, ok := resp["error"]; ok {
			if errMap, ok := errObj.(map[string]any); ok {
				if errMap["code"] == "RATE_LIMITED" {
					gotRateLimited = true
					break
				}
			}
		}
	}
	require.True(t, gotRateLimited, "expected to hit rate limit")
}
