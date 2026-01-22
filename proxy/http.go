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

// HTTPProxy implements RPCProxy using HTTP transport.
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

// ProxyRPC implements RPCProxy.ProxyRPC.
func (p *HTTPProxy) ProxyRPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error) {
	// Apply timeout if not already set in context
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

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

	// Set headers
	for k, v := range p.headers {
		httpReq.Header.Set(k, v)
	}

	log.DebugContext(ctx, "proxying HTTP RPC request",
		"proxy", p.name,
		"endpoint", p.endpoint,
		"channel", req.Channel,
		"method", req.Method,
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

	// Parse response
	var protoResp proxypb.RPCResponse
	if err := json.Unmarshal(respBody, &protoResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return FromProtoReply(&protoResp), nil
}

// Name implements RPCProxy.Name.
func (p *HTTPProxy) Name() string {
	return p.name
}

// Close implements RPCProxy.Close.
func (p *HTTPProxy) Close() error {
	p.client.CloseIdleConnections()
	return nil
}
