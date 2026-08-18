package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

func newTestHTTPProxy(t *testing.T, server *httptest.Server) *HTTPProxy {
	t.Helper()
	p, err := NewHTTPProxy(&ProxyConfig{
		Name:     "test-http",
		Endpoint: server.URL,
		HTTP:     &HTTPProxyConfig{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestHTTPProxy_NotificationMethods_PassThroughBackendError(t *testing.T) {
	// The backend answers every notification endpoint with a non-empty Error field.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"NOTIFY_REJECTED","message":"backend rejected the notification"}}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	assertBackendError := func(t *testing.T, err *sharedv2.Error) {
		t.Helper()
		require.NotNil(t, err, "backend Error field must not be swallowed")
		assert.Equal(t, "NOTIFY_REJECTED", err.Code)
		assert.Equal(t, "backend rejected the notification", err.Message)
	}

	t.Run("OnConnected", func(t *testing.T) {
		resp, err := p.OnConnected(ctx, &OnConnectedProxyRequest{SessionID: "s-1", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})

	t.Run("OnSubscribed", func(t *testing.T) {
		resp, err := p.OnSubscribed(ctx, &OnSubscribedProxyRequest{SessionID: "s-1", Channel: "chat.a", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})

	t.Run("OnUnsubscribed", func(t *testing.T) {
		resp, err := p.OnUnsubscribed(ctx, &OnUnsubscribedProxyRequest{SessionID: "s-1", Channel: "chat.a", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})

	t.Run("OnDisconnected", func(t *testing.T) {
		resp, err := p.OnDisconnected(ctx, &OnDisconnectedProxyRequest{SessionID: "s-1", Username: "alice"})
		require.NoError(t, err)
		assertBackendError(t, resp.Error)
	})
}

func TestHTTPProxy_Authenticate_RequestCarriesSessionAndRemoteAddr(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	_, err := p.Authenticate(ctx, &AuthenticateProxyRequest{
		ClientID:   "client-1",
		Token:      "token-1",
		ClientType: "web",
		SessionID:  "session-1",
		RemoteAddr: "10.0.0.1:4321",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "client-1", body["client_id"])
	assert.Equal(t, "token-1", body["token"])
	assert.Equal(t, "web", body["client_type"])
	assert.Equal(t, "session-1", body["session_id"])
	assert.Equal(t, "10.0.0.1:4321", body["remote_addr"])
}

func TestHTTPProxy_SubscribeAcl_RequestCarriesUserAndSession(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	_, err := p.SubscribeAcl(ctx, &SubscribeAclProxyRequest{
		Channel:   "chat.private",
		Token:     "token-1",
		UserID:    "user-1",
		SessionID: "session-1",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "chat.private", body["channel"])
	assert.Equal(t, "token-1", body["token"])
	assert.Equal(t, "user-1", body["user_id"])
	assert.Equal(t, "session-1", body["session_id"])
}

func TestHTTPProxy_PublishAcl_RequestCarriesUserAndSession(t *testing.T) {
	bodyCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodyCh <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	_, err := p.PublishAcl(ctx, &PublishAclProxyRequest{
		Channel:   "chat.private",
		Token:     "token-1",
		UserID:    "user-1",
		SessionID: "session-1",
	})
	require.NoError(t, err)

	body := <-bodyCh
	assert.Equal(t, "chat.private", body["channel"])
	assert.Equal(t, "token-1", body["token"])
	assert.Equal(t, "user-1", body["user_id"])
	assert.Equal(t, "session-1", body["session_id"])
}

// TestHTTPProxy_RPC_PayloadRoundTrip is the regression test for P1-B1: the
// sharedv2.Payload oneof must survive the HTTP round trip. Before the fix the
// payload was serialized with encoding/json, which drops the oneof Data field,
// so the backend never received any actual payload.
func TestHTTPProxy_RPC_PayloadRoundTrip(t *testing.T) {
	// The backend decodes the request with protojson, checks the payload
	// arrived, and echoes it back.
	type backendResult struct {
		reqPayload     *sharedv2.Payload
		reqMetadata    *sharedv2.Metadata
		protoReqParsed bool
	}
	resultCh := make(chan backendResult, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req proxypb.RPCRequest
		if err := protojson.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resultCh <- backendResult{
			reqPayload:     req.Payload,
			reqMetadata:    req.Metadata,
			protoReqParsed: true,
		}
		resp := &proxypb.RPCResponse{Id: req.Id, Payload: req.Payload, Metadata: req.Metadata}
		respBody, err := protojson.Marshal(resp)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respBody)
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)
	ctx := context.Background()

	cases := []struct {
		name    string
		payload *sharedv2.Payload
	}{
		{"json", &sharedv2.Payload{Data: &sharedv2.Payload_Json{Json: mustStruct(t, map[string]any{"input": "data", "n": 42})}}},
		{"text", &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "hello proxy"}}},
		{"binary", &sharedv2.Payload{Data: &sharedv2.Payload_Binary{Binary: []byte{0xde, 0xad, 0xbe, 0xef}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &RPCProxyRequest{
				ID:      "req-" + tc.name,
				Channel: "test.channel",
				Method:  "testMethod",
				Payload: tc.payload,
				Meta:    map[string]string{"trace": "t-1", "locale": "zh"},
			}
			resp, err := p.RPC(ctx, req)
			require.NoError(t, err)

			// The backend must have seen a non-nil payload with a non-nil
			// oneof Data — the pre-fix encoding/json path failed this.
			result := <-resultCh
			require.True(t, result.protoReqParsed, "backend could not decode the request")
			require.NotNil(t, result.reqPayload, "backend received no payload")
			require.NotNil(t, result.reqPayload.GetData(), "backend payload oneof Data was lost")
			// Metadata must be forwarded (P1-B5).
			require.NotNil(t, result.reqMetadata)
			require.Equal(t, "t-1", result.reqMetadata.GetEntries()["trace"])
			require.Equal(t, "zh", result.reqMetadata.GetEntries()["locale"])

			// The response payload must round-trip the oneof too.
			require.NotNil(t, resp.Payload, "response payload is nil")
			require.NotNil(t, resp.Payload.GetData(), "response payload oneof Data was lost")
			assert.Equal(t, tc.payload.GetContentType(), resp.Payload.GetContentType())
		})
	}
}

// TestHTTPProxy_RPC_NonOKStructuredError verifies that a non-200 response
// carrying a structured sharedv2.Error body surfaces as HTTPStatusError with
// the structured error preserved (P1-B5).
func TestHTTPProxy_RPC_NonOKStructuredError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"PROXY_REJECTED","type":"proxy_error","message":"backend refused the call"}}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)

	_, err := p.RPC(context.Background(), &RPCProxyRequest{ID: "r1", Channel: "c", Method: "m"})
	require.Error(t, err)

	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.StatusCode)
	require.NotNil(t, statusErr.Err, "structured backend error must be preserved")
	assert.Equal(t, "PROXY_REJECTED", statusErr.Err.Code)
	assert.Equal(t, "proxy_error", statusErr.Err.Type)
	assert.Equal(t, "backend refused the call", statusErr.Err.Message)
}

// TestHTTPProxy_RPC_NonOKFallbackTextError verifies that a non-200 response
// without a structured error body still returns an error containing the body
// text (P1-B5 fallback).
func TestHTTPProxy_RPC_NonOKFallbackTextError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("backend exploded"))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)

	_, err := p.RPC(context.Background(), &RPCProxyRequest{ID: "r1", Channel: "c", Method: "m"})
	require.Error(t, err)

	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusInternalServerError, statusErr.StatusCode)
	assert.Nil(t, statusErr.Err)
	assert.Contains(t, err.Error(), "backend exploded")

	// Non-200 without structured error must not be masked as a plain error.
	assert.False(t, errors.Is(err, context.DeadlineExceeded))
}

// TestHTTPProxy_RPC_NonOKStructuredErrorProtoJSONContract verifies that a
// non-200 error body emitted per the proto3 JSON contract (protojson
// encoding) parses into a structured sharedv2.Error: exact camelCase field
// names, a metadata Struct with nested values, tolerated unknown fields
// inside the error object, and an unrelated top-level member (A4).
func TestHTTPProxy_RPC_NonOKStructuredErrorProtoJSONContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","type":"proxy_error","message":"too many requests","metadata":{"attempt":3,"reason":"rate_limited","region":{"code":"cn"}},"future_field":"ignored"},"trace_id":"t-9"}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)

	_, err := p.RPC(context.Background(), &RPCProxyRequest{ID: "r1", Channel: "c", Method: "m"})
	require.Error(t, err)

	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.StatusCode)
	require.NotNil(t, statusErr.Err, "structured backend error must be preserved")
	assert.Equal(t, "RATE_LIMITED", statusErr.Err.Code)
	assert.Equal(t, "proxy_error", statusErr.Err.Type)
	assert.Equal(t, "too many requests", statusErr.Err.Message)
	md := statusErr.Err.GetMetadata()
	require.NotNil(t, md)
	assert.Equal(t, 3.0, md.GetFields()["attempt"].GetNumberValue())
	assert.Equal(t, "rate_limited", md.GetFields()["reason"].GetStringValue())
	assert.Equal(t, "cn", md.GetFields()["region"].GetStructValue().GetFields()["code"].GetStringValue())
}

// TestHTTPProxy_RPC_NonOKStructuredErrorExactFieldNames verifies that the
// error member is parsed with protojson, which honors only exact proto3 JSON
// field names. encoding/json matched wrong-case names case-insensitively and
// would populate Code from "Code"; protojson drops the non-contract member.
// This is the regression guard for A4: it fails against the old
// encoding/json-based implementation.
func TestHTTPProxy_RPC_NonOKStructuredErrorExactFieldNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"Code":"WRONG_CASE","message":"still parses"}}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)

	_, err := p.RPC(context.Background(), &RPCProxyRequest{ID: "r1", Channel: "c", Method: "m"})
	require.Error(t, err)

	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	require.NotNil(t, statusErr.Err)
	assert.Equal(t, "", statusErr.Err.Code, "non-contract field name must not populate code")
	assert.Equal(t, "still parses", statusErr.Err.Message)
}

// TestHTTPProxy_RPC_NonOKMalformedErrorMemberFallsBack verifies that a
// non-200 body whose error member is not a valid protojson object still
// falls back to the raw body text (A4 keeps the fallback behavior).
func TestHTTPProxy_RPC_NonOKMalformedErrorMemberFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":42,"note":"not an object"}`))
	}))
	defer server.Close()

	p := newTestHTTPProxy(t, server)

	_, err := p.RPC(context.Background(), &RPCProxyRequest{ID: "r1", Channel: "c", Method: "m"})
	require.Error(t, err)

	var statusErr *HTTPStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.StatusCode)
	assert.Nil(t, statusErr.Err)
	assert.Contains(t, err.Error(), "not an object")
}

func mustStruct(t *testing.T, v map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(v)
	require.NoError(t, err)
	return s
}
