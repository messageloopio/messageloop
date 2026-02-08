package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	cloudevents "github.com/cloudevents/sdk-go/v2"
	format "github.com/cloudevents/sdk-go/binding/format/protobuf/v2"
	pb "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v1"
)

func TestNewHTTPProxy(t *testing.T) {
	cfg := &ProxyConfig{
		Name:     "test-http",
		Endpoint: "http://example.com/rpc",
		HTTP: &HTTPProxyConfig{
			TLS: &TLSConfig{
				InsecureSkipVerify: true,
			},
			Headers: map[string]string{
				"X-Custom": "value",
			},
		},
	}

	p, err := NewHTTPProxy(cfg)
	require.NoError(t, err)
	assert.NotNil(t, p)
	assert.Equal(t, "test-http", p.Name())
	assert.NoError(t, p.Close())
}

func TestHTTPProxy_RPC(t *testing.T) {
	// Create a test HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		// Parse request body
		var reqBody map[string]interface{}
		err := json.NewDecoder(r.Body).Decode(&reqBody)
		require.NoError(t, err)

		// Build response using JSON directly to avoid protobuf marshaling issues
		respJSON := `{
			"id": "` + reqBody["id"].(string) + `",
			"payload": {
				"id": "resp-123",
				"source": "proxy-test",
				"type": "test.response",
				"spec_version": "1.0",
				"text_data": "{\"result\":\"success\"}"
			}
		}`

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(respJSON))
	}))
	defer server.Close()

	cfg := &ProxyConfig{
		Name:     "test-http",
		Endpoint: server.URL,
		HTTP:     &HTTPProxyConfig{},
	}

	p, err := NewHTTPProxy(cfg)
	require.NoError(t, err)
	defer p.Close()

	ctx := context.Background()
	event := cloudevents.NewEvent()
	event.SetID("req-123")
	event.SetSource("test.source")
	event.SetType("test.request")
	event.SetSpecVersion("1.0")
	event.SetDataContentType("application/json")
	_ = event.SetData("application/json", `{"input":"data"}`)

	req := &RPCProxyRequest{
		ID:      "req-123",
		Channel: "test.channel",
		Method:  "testMethod",
		Event:   &event,
	}

	resp, err := p.RPC(ctx, req)
	require.NoError(t, err)
	assert.NotNil(t, resp.Event)
	assert.Equal(t, "resp-123", resp.Event.ID())
}

func TestHTTPProxy_Timeout(t *testing.T) {
	// Create a test server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &ProxyConfig{
		Name:     "test-timeout",
		Endpoint: server.URL,
		Timeout:  50 * time.Millisecond,
		HTTP:     &HTTPProxyConfig{},
	}

	p, err := NewHTTPProxy(cfg)
	require.NoError(t, err)
	defer p.Close()

	ctx := context.Background()
	event := cloudevents.NewEvent()
	req := &RPCProxyRequest{
		ID:      "req-timeout",
		Channel: "test",
		Method:  "test",
		Event:   &event,
	}

	_, err = p.RPC(ctx, req)
	assert.Error(t, err)
}

func TestNewGRPCProxy(t *testing.T) {
	// Create a test gRPC server
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	server := grpc.NewServer()
	proxypb.RegisterProxyServiceServer(server, &mockGRPCServer{})
	go server.Serve(lis)
	defer server.Stop()

	cfg := &ProxyConfig{
		Name:     "test-grpc",
		Endpoint: lis.Addr().String(),
		GRPC: &GRPCProxyConfig{
			Insecure: true,
		},
	}

	p, err := NewGRPCProxy(cfg)
	require.NoError(t, err)
	assert.NotNil(t, p)
	assert.Equal(t, "test-grpc", p.Name())
	assert.NoError(t, p.Close())
}

func TestGRPCProxy_RPC(t *testing.T) {
	// Create a test gRPC server
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	expectedReq := &proxypb.RPCRequest{}
	serverReady := make(chan struct{})

	mockSrv := &mockGRPCServer{
		handler: func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
			expectedReq = req
			close(serverReady)

			// Create response event
			respEvent := cloudevents.NewEvent()
			respEvent.SetID("resp-grpc-123")
			respEvent.SetSource("grpc-proxy-test")
			respEvent.SetType("test.response")
			respEvent.SetSpecVersion("1.0")
			respEvent.SetDataContentType("application/json")
			_ = respEvent.SetData("application/json", `{"result":"grpc-success"}`)

			pbResp, _ := format.ToProto(&respEvent)

			return &proxypb.RPCResponse{
				Id:      req.Id,
				Payload: pbResp,
			}, nil
		},
	}

	s := grpc.NewServer()
	proxypb.RegisterProxyServiceServer(s, mockSrv)
	go s.Serve(lis)
	defer s.Stop()

	cfg := &ProxyConfig{
		Name:     "test-grpc",
		Endpoint: lis.Addr().String(),
		GRPC: &GRPCProxyConfig{
			Insecure: true,
		},
	}

	p, err := NewGRPCProxy(cfg)
	require.NoError(t, err)
	defer p.Close()

	ctx := context.Background()
	event := cloudevents.NewEvent()
	event.SetID("req-grpc-123")
	event.SetSource("grpc.source")
	event.SetType("test.request")
	event.SetSpecVersion("1.0")
	event.SetDataContentType("application/json")
	_ = event.SetData("application/json", `{"input":"grpc-data"}`)

	req := &RPCProxyRequest{
		ID:      "req-grpc-123",
		Channel: "grpc.channel",
		Method:  "grpcMethod",
		Event:   &event,
	}

	resp, err := p.RPC(ctx, req)
	require.NoError(t, err)

	// Wait for server to process
	<-serverReady

	assert.NotNil(t, resp.Event)
	assert.Equal(t, "resp-grpc-123", resp.Event.ID())
	assert.Equal(t, "req-grpc-123", expectedReq.Id)
	assert.Equal(t, "grpc.channel", expectedReq.Channel)
	assert.Equal(t, "grpcMethod", expectedReq.Method)
}

// mockGRPCServer is a mock implementation of proxypb.ProxyServiceServer.
type mockGRPCServer struct {
	proxypb.UnimplementedProxyServiceServer
	handler func(context.Context, *proxypb.RPCRequest) (*proxypb.RPCResponse, error)
}

func (m *mockGRPCServer) RPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	if m.handler != nil {
		return m.handler(ctx, req)
	}
	return &proxypb.RPCResponse{}, nil
}

func (m *mockGRPCServer) Authenticate(ctx context.Context, req *proxypb.AuthenticateRequest) (*proxypb.AuthenticateResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockGRPCServer) SubscribeAcl(ctx context.Context, req *proxypb.SubscribeAclRequest) (*proxypb.SubscribeAclResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockGRPCServer) OnConnected(ctx context.Context, req *proxypb.OnConnectedRequest) (*proxypb.OnConnectedResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockGRPCServer) OnSubscribed(ctx context.Context, req *proxypb.OnSubscribedRequest) (*proxypb.OnSubscribedResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockGRPCServer) OnUnsubscribed(ctx context.Context, req *proxypb.OnUnsubscribedRequest) (*proxypb.OnUnsubscribedResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockGRPCServer) OnDisconnected(ctx context.Context, req *proxypb.OnDisconnectedRequest) (*proxypb.OnDisconnectedResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func TestRPCProxyRequest_ToProtoRequest(t *testing.T) {
	event := cloudevents.NewEvent()
	event.SetID("event-1")
	event.SetSource("source")
	event.SetType("type")
	event.SetSpecVersion("1.0")

	req := &RPCProxyRequest{
		ID:        "test-id",
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
		Channel:   "test.channel",
		Method:    "testMethod",
		Event:     &event,
		Meta: map[string]string{
			"key": "value",
		},
	}

	protoReq, err := req.ToProtoRequest()
	require.NoError(t, err)

	assert.Equal(t, "test-id", protoReq.Id)
	assert.Equal(t, "test.channel", protoReq.Channel)
	assert.Equal(t, "testMethod", protoReq.Method)
	assert.Equal(t, "event-1", protoReq.Payload.Id)
}

func TestFromProtoReply(t *testing.T) {
	reply := &proxypb.RPCResponse{
		Id: "reply-id",
		Payload: &pb.CloudEvent{
			Id:          "event-1",
			Source:      "source",
			Type:        "type",
			SpecVersion: "1.0",
		},
	}

	resp, err := FromProtoReply(reply)
	require.NoError(t, err)

	assert.Equal(t, "event-1", resp.Event.ID())
	assert.Nil(t, resp.Error)
}

func TestFromProtoReply_Nil(t *testing.T) {
	resp, err := FromProtoReply(nil)
	require.NoError(t, err)

	assert.NotNil(t, resp)
	assert.Nil(t, resp.Event)
	assert.Nil(t, resp.Error)
}
