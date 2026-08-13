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
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
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
	t.Cleanup(func() { _ = conn.Close() })
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

// TestWebSocket_SubprotocolNegotiation is the e2e regression test for P1-B2:
// the negotiated subprotocol and the frame type must always agree, regardless
// of the order of the client's offers. Before the fix, offers like
// ["messageloop+proto", "messageloop"] negotiated "messageloop" (text frames)
// while the protobuf marshaler was selected from the offer list, so the
// connection could never decode a frame.
func TestWebSocket_SubprotocolNegotiation(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	cases := []struct {
		name            string
		offers          []string
		wantSubprotocol string
	}{
		{"plain", []string{"messageloop"}, "messageloop"},
		{"json suffix", []string{"messageloop+json"}, "messageloop+json"},
		{"proto", []string{"messageloop+proto"}, "messageloop+proto"},
		// Server-side protocol list order wins over the client offer order:
		// the negotiated result is "messageloop" even though the first offer
		// mentions proto.
		{"proto then plain", []string{"messageloop+proto", "messageloop"}, "messageloop"},
		{"plain then proto", []string{"messageloop", "messageloop+proto"}, "messageloop"},
		{"no subprotocol", nil, ""},
		{"unknown subprotocol", []string{"xproto"}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dialer := websocket.Dialer{Subprotocols: tc.offers}
			conn, resp, err := dialer.Dial(url, nil)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			require.Equal(t, tc.wantSubprotocol, resp.Header.Get("Sec-Websocket-Protocol"), "negotiated subprotocol")

			// The connection must actually work with the frame type that
			// matches the negotiated subprotocol: JSON text frames for every
			// case except messageloop+proto, which uses binary protobuf.
			if tc.wantSubprotocol == "messageloop+proto" {
				protoRoundTrip(t, conn)
			} else {
				jsonRoundTrip(t, conn)
			}
		})
	}
}

// jsonRoundTrip connects with a JSON text frame and expects a JSON Connected
// response, proving the JSON marshaler is paired with text frames.
func jsonRoundTrip(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	sendJSON(t, conn, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "negotiation-json"},
	})
	resp := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, resp["connected"], "expected JSON Connected response, got: %v", resp)
}

// protoRoundTrip connects with a binary protobuf frame and expects a binary
// protobuf Connected response, proving the protobuf marshaler is paired with
// binary frames.
func protoRoundTrip(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	protoMarshaler := messageloop.ProtobufMarshaler{}
	msg := &clientpb.InboundMessage{
		Id: "conn",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "negotiation-proto"},
		},
	}
	data, err := protoMarshaler.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, data))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msgType, payload, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.BinaryMessage, msgType, "protobuf connection must use binary frames")

	var out clientpb.OutboundMessage
	require.NoError(t, protoMarshaler.Unmarshal(payload, &out))
	require.NotNil(t, out.GetConnected(), "expected protobuf Connected response, got: %v", &out)
}
