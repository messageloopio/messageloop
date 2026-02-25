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
	"google.golang.org/protobuf/types/known/structpb"

	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
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

		// Build response using JSON directly with new Payload structure
		respJSON := `{
			"id": "` + reqBody["id"].(string) + `",
			"payload": {
				"json": {"result": "success"}
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

	// Create Payload
	s, _ := structpb.NewStruct(map[string]interface{}{"input": "data"})
	req := &RPCProxyRequest{
		ID:      "req-123",
		Channel: "test.channel",
		Method:  "testMethod",
		Payload: &sharedpb.Payload{
			Data: &sharedpb.Payload_Json{
				Json: s,
			},
		},
	}

	resp, err := p.RPC(ctx, req)
	require.NoError(t, err)
	assert.NotNil(t, resp.Payload)
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
	s, _ := structpb.NewStruct(map[string]interface{}{})
	req := &RPCProxyRequest{
		ID:      "req-timeout",
		Channel: "test",
		Method:  "test",
		Payload: &sharedpb.Payload{
			Data: &sharedpb.Payload_Json{
				Json: s,
			},
		},
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

			// Create response payload
			s, _ := structpb.NewStruct(map[string]interface{}{"result": "grpc-success"})

			return &proxypb.RPCResponse{
				Id: req.Id,
				Payload: &sharedpb.Payload{
					Data: &sharedpb.Payload_Json{
						Json: s,
					},
				},
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

	// Create request payload
	reqPayload, _ := structpb.NewStruct(map[string]interface{}{"input": "grpc-data"})
	req := &RPCProxyRequest{
		ID:      "req-grpc-123",
		Channel: "grpc.channel",
		Method:  "grpcMethod",
		Payload: &sharedpb.Payload{
			Data: &sharedpb.Payload_Json{
				Json: reqPayload,
			},
		},
	}

	resp, err := p.RPC(ctx, req)
	require.NoError(t, err)

	// Wait for server to process
	<-serverReady

	assert.NotNil(t, resp.Payload)
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
	s, _ := structpb.NewStruct(map[string]interface{}{"data": "test"})

	req := &RPCProxyRequest{
		ID:        "test-id",
		ClientID:  "client-1",
		SessionID: "session-1",
		UserID:    "user-1",
		Channel:   "test.channel",
		Method:    "testMethod",
		Payload: &sharedpb.Payload{
			Data: &sharedpb.Payload_Json{
				Json: s,
			},
		},
		Meta: map[string]string{
			"key": "value",
		},
	}

	protoReq, err := req.ToProtoRequest()
	require.NoError(t, err)

	assert.Equal(t, "test-id", protoReq.Id)
	assert.Equal(t, "test.channel", protoReq.Channel)
	assert.Equal(t, "testMethod", protoReq.Method)
	assert.NotNil(t, protoReq.Payload)
}

func TestFromProtoReply(t *testing.T) {
	s, _ := structpb.NewStruct(map[string]interface{}{"result": "ok"})

	reply := &proxypb.RPCResponse{
		Id: "reply-id",
		Payload: &sharedpb.Payload{
			Data: &sharedpb.Payload_Json{
				Json: s,
			},
		},
	}

	resp, err := FromProtoReply(reply)
	require.NoError(t, err)

	assert.NotNil(t, resp.Payload)
	assert.Nil(t, resp.Error)
}

func TestFromProtoReply_Nil(t *testing.T) {
	resp, err := FromProtoReply(nil)
	require.NoError(t, err)

	assert.NotNil(t, resp)
	assert.Nil(t, resp.Payload)
	assert.Nil(t, resp.Error)
}
