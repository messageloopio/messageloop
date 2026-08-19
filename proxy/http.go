package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lynx-go/x/log"
	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// maxResponseBodySize caps how much of a backend response body is read into
// memory: a misbehaving or compromised backend must not be able to exhaust
// server memory with an unbounded body.
const maxResponseBodySize = 4 << 20 // 4 MiB

// proxyJSONMarshal emits proto field names (snake_case) so HTTP backends that
// already consume the hand-rolled maps keep working, while still using the
// proto3 JSON encoder.
var proxyJSONMarshal = protojson.MarshalOptions{UseProtoNames: true}

func marshalProxyJSON(msg proto.Message) ([]byte, error) {
	return proxyJSONMarshal.Marshal(msg)
}

// notificationErrorFromBody reads the optional Error object on a 200
// notification response. The proto response messages have no fields, so the
// backend error is an extra JSON member parsed with protojson.
func notificationErrorFromBody(respBody []byte) (*sharedv2.Error, error) {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(respBody, &envelope) != nil || len(envelope.Error) == 0 {
		return nil, nil
	}
	var structured sharedv2.Error
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(envelope.Error, &structured); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &structured, nil
}

// HTTPStatusError is returned by the HTTP proxy when the backend answers with
// a non-200 status. When the body carries a structured sharedv2.Error it is
// preserved in Err; otherwise the raw body text is kept in Body. Callers may
// use errors.As to inspect the structured error.
type HTTPStatusError struct {
	StatusCode int
	Err        *sharedv2.Error
	Body       []byte
}

func (e *HTTPStatusError) Error() string {
	if e.Err != nil && e.Err.Message != "" {
		return fmt.Sprintf("proxy returned status %d: %s (code: %s)", e.StatusCode, e.Err.Message, e.Err.Code)
	}
	return fmt.Sprintf("proxy returned status %d: %s", e.StatusCode, string(e.Body))
}

var _ error = (*HTTPStatusError)(nil)

// HTTPProxy implements Proxy using HTTP transport.
type HTTPProxy struct {
	name     string
	endpoint string
	client   *http.Client
	headers  map[string]string
	timeout  time.Duration
}

// NewHTTPProxy creates a new HTTP proxy instance.
func NewHTTPProxy(cfg *ProxyConfig) (*HTTPProxy, error) {
	if cfg.HTTP == nil {
		cfg.HTTP = &HTTPProxyConfig{}
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultRPCTimeout
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.HTTP.TLS != nil && cfg.HTTP.TLS.InsecureSkipVerify,
		},
	}

	if cfg.HTTP.TLS != nil && cfg.HTTP.TLS.ServerName != "" {
		transport.TLSClientConfig.ServerName = cfg.HTTP.TLS.ServerName
	}

	headers := make(map[string]string)
	if cfg.HTTP.Headers != nil {
		for k, v := range cfg.HTTP.Headers {
			headers[k] = v
		}
	}
	// Set default content type
	headers["Content-Type"] = "application/json"

	return &HTTPProxy{
		name:     cfg.Name,
		endpoint: cfg.Endpoint,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
		headers: headers,
		timeout: timeout,
	}, nil
}

// RPC implements Proxy.RPC.
func (p *HTTPProxy) RPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	// Build the HTTP request
	protoReq, err := req.ToProtoRequest()
	if err != nil {
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}
	// Marshal the payload-bearing request with protojson: encoding/json cannot
	// round-trip the sharedv2.Payload oneof, which silently drops the Data
	// field. protojson matches the proto3 JSON contract of the gRPC path and
	// carries the request metadata through.
	body, err := protojson.Marshal(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "RPC", req.Channel, req.Method,
		func(respBody []byte) (any, error) {
			var protoResp proxypb.RPCResponse
			// protojson restores the payload oneof that encoding/json drops.
			if err := protojson.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoReply(&protoResp)
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*RPCProxyResponse), nil
}

// Authenticate implements Proxy.Authenticate.
func (p *HTTPProxy) Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "Authenticate", req.ClientID, "",
		func(respBody []byte) (any, error) {
			var protoResp proxypb.AuthenticateResponse
			// Parse with protojson like the RPC path: encoding/json cannot match
			// the proto3 JSON contract (camelCase names such as userInfo),
			// silently dropping fields when the backend emits it. protojson
			// accepts both the JSON name and the original proto field name.
			opts := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := opts.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoAuthenticateResponse(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*AuthenticateProxyResponse), nil
}

// SubscribeAcl implements Proxy.SubscribeAcl.
func (p *HTTPProxy) SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "SubscribeAcl", req.Channel, "",
		func(respBody []byte) (any, error) {
			var protoResp proxypb.SubscribeAclResponse
			opts := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := opts.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoSubscribeAclResponse(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*SubscribeAclProxyResponse), nil
}

// PublishAcl implements Proxy.PublishAcl.
func (p *HTTPProxy) PublishAcl(ctx context.Context, req *PublishAclProxyRequest) (*PublishAclProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "PublishAcl", req.Channel, "",
		func(respBody []byte) (any, error) {
			var protoResp proxypb.PublishAclResponse
			opts := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := opts.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoPublishAclResponse(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*PublishAclProxyResponse), nil
}

// OnConnected implements Proxy.OnConnected.
func (p *HTTPProxy) OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnConnected", req.SessionID, "",
		func(respBody []byte) (any, error) {
			errObj, err := notificationErrorFromBody(respBody)
			if err != nil {
				return nil, err
			}
			return &OnConnectedProxyResponse{Error: errObj}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnConnectedProxyResponse), nil
}

// OnSubscribed implements Proxy.OnSubscribed.
func (p *HTTPProxy) OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnSubscribed", req.SessionID, req.Channel,
		func(respBody []byte) (any, error) {
			errObj, err := notificationErrorFromBody(respBody)
			if err != nil {
				return nil, err
			}
			return &OnSubscribedProxyResponse{Error: errObj}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnSubscribedProxyResponse), nil
}

// OnUnsubscribed implements Proxy.OnUnsubscribed.
func (p *HTTPProxy) OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnUnsubscribed", req.SessionID, req.Channel,
		func(respBody []byte) (any, error) {
			errObj, err := notificationErrorFromBody(respBody)
			if err != nil {
				return nil, err
			}
			return &OnUnsubscribedProxyResponse{Error: errObj}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnUnsubscribedProxyResponse), nil
}

// OnDisconnected implements Proxy.OnDisconnected.
func (p *HTTPProxy) OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error) {
	ctx, cancel := p.withTimeout(ctx)
	defer cancel()

	protoReq := req.ToProtoRequest()
	body, err := marshalProxyJSON(protoReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnDisconnected", req.SessionID, "",
		func(respBody []byte) (any, error) {
			errObj, err := notificationErrorFromBody(respBody)
			if err != nil {
				return nil, err
			}
			return &OnDisconnectedProxyResponse{Error: errObj}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnDisconnectedProxyResponse), nil
}

// doRequest is a helper function for making HTTP requests.
func (p *HTTPProxy) doRequest(ctx context.Context, httpReq *http.Request, method, channel, extra string, parseFunc func([]byte) (any, error)) (any, error) {
	// Set headers
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}

	log.DebugContext(ctx, "proxying HTTP request",
		"proxy", p.name,
		"endpoint", httpReq.URL.String(),
		"method", method,
		"channel", channel,
		"extra", extra,
	)

	// Send the request
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response, bounded: a misbehaving or compromised backend must not
	// exhaust server memory with an unbounded body.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if len(respBody) > maxResponseBodySize {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBodySize)
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		// Prefer a structured sharedv2.Error from the body (same JSON shape as
		// notification responses); fall back to raw body text. The error
		// member is parsed with protojson to match the proto3 JSON contract
		// (exact field names, metadata Struct), tolerating unknown fields like
		// the root ProtoJSONMarshaler does.
		var envelope struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(respBody, &envelope) == nil && len(envelope.Error) > 0 {
			var structured sharedv2.Error
			opts := protojson.UnmarshalOptions{DiscardUnknown: true}
			if err := opts.Unmarshal(envelope.Error, &structured); err == nil {
				return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Err: &structured, Body: respBody}
			}
		}
		return nil, &HTTPStatusError{StatusCode: resp.StatusCode, Body: respBody}
	}

	return parseFunc(respBody)
}

// withTimeout applies the proxy timeout if not already set in context.
// The caller must defer the returned CancelFunc.
func (p *HTTPProxy) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return context.WithTimeout(ctx, p.timeout)
	}
	return ctx, func() {}
}

// Name implements Proxy.Name.
func (p *HTTPProxy) Name() string {
	return p.name
}

// Close implements Proxy.Close.
func (p *HTTPProxy) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
