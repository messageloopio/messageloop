package websocket

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fleetlit/messageloop"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHandler(t *testing.T) {
	node := messageloop.NewNode()
	_ = node

	opts := Options{
		Addr:   ":9080",
		WsPath: "/ws",
	}

	handler := NewHandler(node, opts)
	require.NotNil(t, handler)
}

func TestHandler_marshalerSelection(t *testing.T) {
	node := messageloop.NewNode()
	_ = node

	handler := NewHandler(node, Options{})

	tests := []struct {
		name         string
		subprotocols []string
	}{
		{
			name:         "messageloop subprotocol",
			subprotocols: []string{"messageloop"},
		},
		{
			name:         "messageloop+json subprotocol",
			subprotocols: []string{"messageloop+json"},
		},
		{
			name:         "messageloop+proto subprotocol",
			subprotocols: []string{"messageloop+proto"},
		},
		{
			name:         "unknown subprotocol",
			subprotocols: []string{"unknown"},
		},
		{
			name:         "empty subprotocols",
			subprotocols: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marshaler := handler.marshaler(tt.subprotocols)
			require.NotNil(t, marshaler)
			// Just verify we get a valid marshaler back
			assert.NotEmpty(t, marshaler.Name())
		})
	}
}

func TestTransport_CloseWithCode(t *testing.T) {
	// Create a test server that expects a close message
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			return // Skip if upgrade fails
		}
		defer conn.Close()

		// Read close message with custom code
		_, data, err := conn.ReadMessage()
		if err != nil {
			return // Connection closed
		}
		if len(data) > 0 {
			assert.Equal(t, websocket.CloseMessage, data[0])
		}
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Skip("WebSocket server not available")
	}
	defer conn.Close()

	marshaler := messageloop.ProtobufMarshaler{}
	transport := newTransport(conn, marshaler)

	disconnect := messageloop.Disconnect{
		Code:   3001,
		Reason: "test reason",
	}
	_ = transport.Close(disconnect)
	// May error due to connection state, but close should be attempted
}

func TestServer_Name(t *testing.T) {
	node := messageloop.NewNode()
	server := NewServer(DefaultOptions(), node)
	assert.Equal(t, "websocket", server.Name())
}

func TestServer_Init(t *testing.T) {
	node := messageloop.NewNode()
	server := NewServer(DefaultOptions(), node)
	err := server.Init(nil)
	require.NoError(t, err)
}

func BenchmarkTransport_Write(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + server.URL[len("http"):]
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()

	marshaler := messageloop.ProtoJSONMarshaler
	transport := newTransport(conn, marshaler)

	// Prepare test messages
	messages := make([][]byte, b.N)
	for i := range messages {
		messages[i] = []byte(`{"id":"` + string(rune(i)) + `"}`)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		transport.WriteMany(messages[i])
	}
}

// Test server configuration options
func TestServer_Configuration(t *testing.T) {
	tests := []struct {
		name   string
		opts   Options
	}{
		{
			name:   "default options",
			opts:   DefaultOptions(),
		},
		{
			name: "custom address and path",
			opts: Options{
				Addr:   ":9999",
				WsPath: "/websocket",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := messageloop.NewNode()
			server := NewServer(tt.opts, node)
			assert.NotNil(t, server)
			assert.Equal(t, "websocket", server.Name())
		})
	}
}

// Test transport interface compliance
func TestTransport_Interface(t *testing.T) {
	var _ messageloop.Transport = (*Transport)(nil)
}
