package proxy

import (
	"context"
	"time"

	proxypb "github.com/deeplooplabs/messageloop/genproto/proxy/v1"
	sharedpb "github.com/deeplooplabs/messageloop/genproto/shared/v1"
	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
)

// RPCProxy defines the interface for proxying RPC requests to backend services.
type RPCProxy interface {
	// ProxyRPC forwards an RPC request to the backend service.
	ProxyRPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error)

	// Name returns the name of this proxy instance.
	Name() string

	// Close closes any resources associated with this proxy.
	Close() error
}

// RPCProxyRequest represents a request to be proxied.
type RPCProxyRequest struct {
	ID        string
	ClientID  string
	SessionID string
	UserID    string
	Channel   string
	Method    string
	Event     *cloudevents.CloudEvent
	Meta      map[string]string
}

// RPCProxyResponse represents a response from the proxy backend.
type RPCProxyResponse struct {
	Event *cloudevents.CloudEvent
	Error *sharedpb.Error
}

// ToProtoRequest converts an RPCProxyRequest to the protobuf RPCRequest.
func (r *RPCProxyRequest) ToProtoRequest() *proxypb.RPCRequest {
	return &proxypb.RPCRequest{
		Id:      r.ID,
		Channel: r.Channel,
		Method:  r.Method,
		Event:   r.Event,
	}
}

// FromProtoResponse creates an RPCProxyResponse from the protobuf RPCReply.
func FromProtoReply(reply *proxypb.RPCReply) *RPCProxyResponse {
	if reply == nil {
		return &RPCProxyResponse{}
	}
	return &RPCProxyResponse{
		Event: reply.Event,
		Error: reply.Error,
	}
}

// ProxyConfig is the configuration for a single proxy instance.
type ProxyConfig struct {
	// Name is a unique identifier for this proxy.
	Name string `yaml:"name" json:"name"`

	// Endpoint is the backend service endpoint URL.
	// For HTTP: "http://localhost:8001/rpc" or "https://service.example.com/api"
	// For gRPC: "localhost:9001" or "service.example.com:9001"
	Endpoint string `yaml:"endpoint" json:"endpoint"`

	// Timeout is the request timeout for this proxy.
	// If zero, the default timeout is used.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`

	// HTTP configures HTTP-specific settings.
	HTTP *HTTPProxyConfig `yaml:"http" json:"http"`

	// GRPC configures gRPC-specific settings.
	GRPC *GRPCProxyConfig `yaml:"grpc" json:"grpc"`

	// Routes defines which requests should be routed to this proxy.
	Routes []RouteConfig `yaml:"routes" json:"routes"`
}

// HTTPProxyConfig contains HTTP-specific proxy settings.
type HTTPProxyConfig struct {
	// TLS configures HTTPS settings.
	TLS *TLSConfig `yaml:"tls" json:"tls"`

	// Headers are additional headers to include in requests.
	Headers map[string]string `yaml:"headers" json:"headers"`
}

// TLSConfig contains TLS configuration for HTTP proxies.
type TLSConfig struct {
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" json:"insecure_skip_verify"`

	// ServerName is the expected server name.
	ServerName string `yaml:"server_name" json:"server_name"`
}

// GRPCProxyConfig contains gRPC-specific proxy settings.
type GRPCProxyConfig struct {
	// Insecure disables TLS for gRPC connections.
	Insecure bool `yaml:"insecure" json:"insecure"`
}

// RouteConfig defines a routing rule for the proxy.
type RouteConfig struct {
	// Channel is the channel pattern to match. Supports glob wildcards.
	// Example: "chat.*" matches "chat.messages", "chat.presence", etc.
	Channel string `yaml:"channel" json:"channel"`

	// Method is the method pattern to match. Supports glob wildcards.
	// Example: "user.*" matches "user.get", "user.set", etc.
	// Use "*" to match all methods.
	Method string `yaml:"method" json:"method"`
}

// DefaultRPCTimeout is the default timeout for RPC proxy requests.
const DefaultRPCTimeout = 30 * time.Second
