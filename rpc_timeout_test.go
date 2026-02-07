package messageloop

import (
	"context"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/genproto/v1"
	"github.com/messageloopio/messageloop/proxy"
	"github.com/stretchr/testify/assert"
)

// MockSlowProxy simulates a slow RPC backend
type MockSlowProxy struct {
	delay time.Duration
}

func (m *MockSlowProxy) RPC(ctx context.Context, req *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	select {
	case <-time.After(m.delay):
		return &proxy.RPCProxyResponse{
			Event: &cloudevents.CloudEvent{
				Id:          req.ID,
				Source:      req.Channel,
				Type:        req.Method,
				SpecVersion: "1.0",
			},
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *MockSlowProxy) Authenticate(ctx context.Context, req *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	return &proxy.AuthenticateProxyResponse{}, nil
}

func (m *MockSlowProxy) SubscribeAcl(ctx context.Context, req *proxy.SubscribeAclProxyRequest) (*proxy.SubscribeAclProxyResponse, error) {
	return &proxy.SubscribeAclProxyResponse{}, nil
}

func (m *MockSlowProxy) OnConnected(ctx context.Context, req *proxy.OnConnectedProxyRequest) (*proxy.OnConnectedProxyResponse, error) {
	return &proxy.OnConnectedProxyResponse{}, nil
}

func (m *MockSlowProxy) OnSubscribed(ctx context.Context, req *proxy.OnSubscribedProxyRequest) (*proxy.OnSubscribedProxyResponse, error) {
	return &proxy.OnSubscribedProxyResponse{}, nil
}

func (m *MockSlowProxy) OnUnsubscribed(ctx context.Context, req *proxy.OnUnsubscribedProxyRequest) (*proxy.OnUnsubscribedProxyResponse, error) {
	return &proxy.OnUnsubscribedProxyResponse{}, nil
}

func (m *MockSlowProxy) OnDisconnected(ctx context.Context, req *proxy.OnDisconnectedProxyRequest) (*proxy.OnDisconnectedProxyResponse, error) {
	return &proxy.OnDisconnectedProxyResponse{}, nil
}

func (m *MockSlowProxy) Name() string {
	return "mock-slow-proxy"
}

func (m *MockSlowProxy) Close() error {
	return nil
}

// MockTransport for testing
type MockTransport struct {
	messages []*clientpb.OutboundMessage
}

func (m *MockTransport) Write(data []byte) error {
	return nil
}

func (m *MockTransport) WriteMany(data ...[]byte) error {
	return nil
}

func (m *MockTransport) Close(disconnect Disconnect) error {
	return nil
}

func TestRPCTimeout_FastResponse(t *testing.T) {
	// Create node with 1 second timeout
	cfg := &config.Server{
		RPCTimeout: "1s",
	}
	node := NewNode(cfg)

	// Add a fast proxy (100ms delay)
	fastProxy := &MockSlowProxy{delay: 100 * time.Millisecond}
	err := node.AddProxy(fastProxy, "test.*", "*")
	assert.NoError(t, err)

	// Create client session
	ctx := context.Background()
	transport := &MockTransport{}
	client, _, err := NewClientSession(ctx, node, transport, &JSONMarshaler{})
	assert.NoError(t, err)

	// Simulate authenticated client
	client.mu.Lock()
	client.authenticated = true
	client.client = "test-client"
	client.mu.Unlock()

	// Send RPC request
	event := &cloudevents.CloudEvent{
		Id:          "test-1",
		Source:      "test.channel",
		Type:        "test.method",
		SpecVersion: "1.0",
	}

	in := &clientpb.InboundMessage{
		Id:      "msg-1",
		Channel: "test.channel",
		Method:  "test.method",
	}

	start := time.Now()
	err = client.onRPC(ctx, in, event)
	duration := time.Since(start)

	// Should succeed without timeout
	assert.NoError(t, err)
	assert.Less(t, duration, 1*time.Second, "Fast RPC should complete quickly")
}

func TestRPCTimeout_SlowResponse(t *testing.T) {
	// Create node with 500ms timeout
	cfg := &config.Server{
		RPCTimeout: "500ms",
	}
	node := NewNode(cfg)

	// Add a slow proxy (2 second delay)
	slowProxy := &MockSlowProxy{delay: 2 * time.Second}
	err := node.AddProxy(slowProxy, "test.*", "*")
	assert.NoError(t, err)

	// Create client session
	ctx := context.Background()
	transport := &MockTransport{}
	client, _, err := NewClientSession(ctx, node, transport, &JSONMarshaler{})
	assert.NoError(t, err)

	// Simulate authenticated client
	client.mu.Lock()
	client.authenticated = true
	client.client = "test-client"
	client.mu.Unlock()

	// Send RPC request
	event := &cloudevents.CloudEvent{
		Id:          "test-2",
		Source:      "test.channel",
		Type:        "test.method",
		SpecVersion: "1.0",
	}

	in := &clientpb.InboundMessage{
		Id:      "msg-2",
		Channel: "test.channel",
		Method:  "test.method",
	}

	start := time.Now()
	err = client.onRPC(ctx, in, event)
	duration := time.Since(start)

	// Should not return error (error is sent to client via Send)
	assert.NoError(t, err)
	// Should timeout around 500ms, not wait for full 2 seconds
	assert.Less(t, duration, 1*time.Second, "Should timeout quickly")
	assert.GreaterOrEqual(t, duration, 500*time.Millisecond, "Should wait at least timeout duration")
}

func TestRPCTimeout_DefaultTimeout(t *testing.T) {
	// Create node without timeout config (should use default)
	cfg := &config.Server{}
	node := NewNode(cfg)

	// Check default timeout is set
	timeout := node.GetRPCTimeout()
	assert.Equal(t, proxy.DefaultRPCTimeout, timeout, "Should use default RPC timeout")
}

func TestRPCTimeout_CustomTimeout(t *testing.T) {
	// Create node with custom timeout
	cfg := &config.Server{
		RPCTimeout: "5s",
	}
	node := NewNode(cfg)

	// Check custom timeout is set
	timeout := node.GetRPCTimeout()
	assert.Equal(t, 5*time.Second, timeout, "Should use custom RPC timeout")
}

func TestRPCTimeout_InvalidTimeout(t *testing.T) {
	// Create node with invalid timeout (should fallback to default)
	cfg := &config.Server{
		RPCTimeout: "invalid",
	}
	node := NewNode(cfg)

	// Check default timeout is used
	timeout := node.GetRPCTimeout()
	assert.Equal(t, proxy.DefaultRPCTimeout, timeout, "Should fallback to default on invalid timeout")
}
