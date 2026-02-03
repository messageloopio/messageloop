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

	proxypb "github.com/fleetlit/messageloop/genproto/proxy/v1"
	"github.com/lynx-go/x/log"
)

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
	ctx = p.withTimeout(ctx)

	// Build the HTTP request
	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"id":      protoReq.Id,
		"channel": protoReq.Channel,
		"method":  protoReq.Method,
		"payload": protoReq.Payload,
	})
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
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoReply(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*RPCProxyResponse), nil
}

// Authenticate implements Proxy.Authenticate.
func (p *HTTPProxy) Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"username":    protoReq.Username,
		"password":    protoReq.Password,
		"client_type": protoReq.ClientType,
		"client_id":   protoReq.ClientId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "Authenticate", req.Username, "",
		func(respBody []byte) (any, error) {
			var protoResp proxypb.AuthenticateResponse
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
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
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"channel": protoReq.Channel,
		"token":   protoReq.Token,
	})
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
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
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

// OnConnected implements Proxy.OnConnected.
func (p *HTTPProxy) OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"session_id": protoReq.SessionId,
		"username":   protoReq.Username,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnConnected", req.SessionID, "",
		func(respBody []byte) (any, error) {
			var protoResp proxypb.OnConnectedResponse
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoOnConnectedResponse(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnConnectedProxyResponse), nil
}

// OnSubscribed implements Proxy.OnSubscribed.
func (p *HTTPProxy) OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"session_id": protoReq.SessionId,
		"channel":    protoReq.Channel,
		"username":   protoReq.Username,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnSubscribed", req.SessionID, req.Channel,
		func(respBody []byte) (any, error) {
			var protoResp proxypb.OnSubscribedResponse
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoOnSubscribedResponse(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnSubscribedProxyResponse), nil
}

// OnUnsubscribed implements Proxy.OnUnsubscribed.
func (p *HTTPProxy) OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"session_id": protoReq.SessionId,
		"channel":    protoReq.Channel,
		"username":   protoReq.Username,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnUnsubscribed", req.SessionID, req.Channel,
		func(respBody []byte) (any, error) {
			var protoResp proxypb.OnUnsubscribedResponse
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoOnUnsubscribedResponse(&protoResp), nil
		},
	)
	if err != nil {
		return nil, err
	}
	return result.(*OnUnsubscribedProxyResponse), nil
}

// OnDisconnected implements Proxy.OnDisconnected.
func (p *HTTPProxy) OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error) {
	ctx = p.withTimeout(ctx)

	protoReq := req.ToProtoRequest()
	body, err := json.Marshal(map[string]any{
		"session_id": protoReq.SessionId,
		"username":   protoReq.Username,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	result, err := p.doRequest(ctx, httpReq, "OnDisconnected", req.SessionID, "",
		func(respBody []byte) (any, error) {
			var protoResp proxypb.OnDisconnectedResponse
			if err := json.Unmarshal(respBody, &protoResp); err != nil {
				return nil, fmt.Errorf("failed to unmarshal response: %w", err)
			}
			return FromProtoOnDisconnectedResponse(&protoResp), nil
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
	defer resp.Body.Close()

	// Read response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy returned status %d: %s", resp.StatusCode, string(respBody))
	}

	return parseFunc(respBody)
}

// withTimeout applies the proxy timeout if not already set in context.
func (p *HTTPProxy) withTimeout(ctx context.Context) context.Context {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		// Use context.WithTimeout but discard the cancel function.
		// The cancel is called automatically when the context expires or is canceled.
		// We don't need to track it separately since the HTTP request will complete
		// before the timeout (or be canceled by it).
		ctx, _ = context.WithTimeout(ctx, p.timeout)
	}
	return ctx
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
