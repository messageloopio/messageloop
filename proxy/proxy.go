package proxy

import (
	"context"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	proxypb "github.com/deeplooplabs/messageloop/genproto/proxy/v1"
	sharedpb "github.com/deeplooplabs/messageloop/genproto/shared/v1"
)

// Proxy defines the interface for proxying RPC requests to backend services.
type Proxy interface {
	// RPC forwards an RPC request to the backend service.
	RPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error)

	// Authenticate forwards an authentication request to the backend service.
	Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error)

	// SubscribeAcl forwards a subscription ACL check request to the backend service.
	SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error)

	// OnConnected notifies the backend when a client connects.
	OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error)

	// OnSubscribed notifies the backend when a client subscribes to a channel.
	OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error)

	// OnUnsubscribed notifies the backend when a client unsubscribes from a channel.
	OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error)

	// OnDisconnected notifies the backend when a client disconnects.
	OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error)

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
		Payload: r.Event,
	}
}

// FromProtoResponse creates an RPCProxyResponse from the protobuf RPCResponse.
func FromProtoReply(reply *proxypb.RPCResponse) *RPCProxyResponse {
	if reply == nil {
		return &RPCProxyResponse{}
	}
	return &RPCProxyResponse{
		Event: reply.Payload,
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

// AuthenticateProxyRequest represents an authentication request to be proxied.
type AuthenticateProxyRequest struct {
	Username   string
	Password   string
	ClientType string
	ClientID   string
}

// ToProtoRequest converts an AuthenticateProxyRequest to the protobuf AuthenticateRequest.
func (r *AuthenticateProxyRequest) ToProtoRequest() *proxypb.AuthenticateRequest {
	return &proxypb.AuthenticateRequest{
		Username:   r.Username,
		Password:   r.Password,
		ClientType: r.ClientType,
		ClientId:   r.ClientID,
	}
}

// AuthenticateProxyResponse represents an authentication response from the proxy backend.
type AuthenticateProxyResponse struct {
	Error    *sharedpb.Error
	UserInfo *UserInfo
}

// UserInfo represents user information returned from authentication.
type UserInfo struct {
	ID         string
	Username   string
	Token      string
	ClientType string
	ClientID   string
}

// FromProtoAuthenticateResponse creates an AuthenticateProxyResponse from the protobuf AuthenticateResponse.
func FromProtoAuthenticateResponse(resp *proxypb.AuthenticateResponse) *AuthenticateProxyResponse {
	if resp == nil {
		return &AuthenticateProxyResponse{}
	}
	userInfo := &UserInfo{}
	if resp.UserInfo != nil {
		userInfo = &UserInfo{
			ID:         resp.UserInfo.Id,
			Username:   resp.UserInfo.Username,
			Token:      resp.UserInfo.Token,
			ClientType: resp.UserInfo.ClientType,
			ClientID:   resp.UserInfo.ClientId,
		}
	}
	return &AuthenticateProxyResponse{
		Error:    resp.Error,
		UserInfo: userInfo,
	}
}

// SubscribeAclProxyRequest represents a subscription ACL check request to be proxied.
type SubscribeAclProxyRequest struct {
	Channel string
	Token   string
}

// ToProtoRequest converts a SubscribeAclProxyRequest to the protobuf SubscribeAclRequest.
func (r *SubscribeAclProxyRequest) ToProtoRequest() *proxypb.SubscribeAclRequest {
	return &proxypb.SubscribeAclRequest{
		Channel: r.Channel,
		Token:   r.Token,
	}
}

// SubscribeAclProxyResponse represents a subscription ACL response from the proxy backend.
type SubscribeAclProxyResponse struct {
	Error *sharedpb.Error
}

// FromProtoResponse creates a SubscribeAclProxyResponse from the protobuf SubscribeAclResponse.
func FromProtoSubscribeAclResponse(resp *proxypb.SubscribeAclResponse) *SubscribeAclProxyResponse {
	if resp == nil {
		return &SubscribeAclProxyResponse{}
	}
	return &SubscribeAclProxyResponse{}
}

// OnConnectedProxyRequest represents a client connected notification to be proxied.
type OnConnectedProxyRequest struct {
	SessionID string
	Username  string
}

// ToProtoRequest converts an OnConnectedProxyRequest to the protobuf OnConnectedRequest.
func (r *OnConnectedProxyRequest) ToProtoRequest() *proxypb.OnConnectedRequest {
	return &proxypb.OnConnectedRequest{
		SessionId: r.SessionID,
		Username:  r.Username,
	}
}

// OnConnectedProxyResponse represents a response from the OnConnected notification.
type OnConnectedProxyResponse struct {
	Error *sharedpb.Error
}

// FromProtoResponse creates an OnConnectedProxyResponse from the protobuf OnConnectedResponse.
func FromProtoOnConnectedResponse(resp *proxypb.OnConnectedResponse) *OnConnectedProxyResponse {
	if resp == nil {
		return &OnConnectedProxyResponse{}
	}
	return &OnConnectedProxyResponse{}
}

// OnSubscribedProxyRequest represents a client subscribed notification to be proxied.
type OnSubscribedProxyRequest struct {
	SessionID string
	Channel   string
	Username  string
}

// ToProtoRequest converts an OnSubscribedProxyRequest to the protobuf OnSubscribedRequest.
func (r *OnSubscribedProxyRequest) ToProtoRequest() *proxypb.OnSubscribedRequest {
	return &proxypb.OnSubscribedRequest{
		SessionId: r.SessionID,
		Channel:   r.Channel,
		Username:  r.Username,
	}
}

// OnSubscribedProxyResponse represents a response from the OnSubscribed notification.
type OnSubscribedProxyResponse struct {
	Error *sharedpb.Error
}

// FromProtoResponse creates an OnSubscribedProxyResponse from the protobuf OnSubscribedResponse.
func FromProtoOnSubscribedResponse(resp *proxypb.OnSubscribedResponse) *OnSubscribedProxyResponse {
	if resp == nil {
		return &OnSubscribedProxyResponse{}
	}
	return &OnSubscribedProxyResponse{}
}

// OnUnsubscribedProxyRequest represents a client unsubscribed notification to be proxied.
type OnUnsubscribedProxyRequest struct {
	SessionID string
	Channel   string
	Username  string
}

// ToProtoRequest converts an OnUnsubscribedProxyRequest to the protobuf OnUnsubscribedRequest.
func (r *OnUnsubscribedProxyRequest) ToProtoRequest() *proxypb.OnUnsubscribedRequest {
	return &proxypb.OnUnsubscribedRequest{
		SessionId: r.SessionID,
		Channel:   r.Channel,
		Username:  r.Username,
	}
}

// OnUnsubscribedProxyResponse represents a response from the OnUnsubscribed notification.
type OnUnsubscribedProxyResponse struct {
	Error *sharedpb.Error
}

// FromProtoResponse creates an OnUnsubscribedProxyResponse from the protobuf OnUnsubscribedResponse.
func FromProtoOnUnsubscribedResponse(resp *proxypb.OnUnsubscribedResponse) *OnUnsubscribedProxyResponse {
	if resp == nil {
		return &OnUnsubscribedProxyResponse{}
	}
	return &OnUnsubscribedProxyResponse{}
}

// OnDisconnectedProxyRequest represents a client disconnected notification to be proxied.
type OnDisconnectedProxyRequest struct {
	SessionID string
	Username  string
}

// ToProtoRequest converts an OnDisconnectedProxyRequest to the protobuf OnDisconnectedRequest.
func (r *OnDisconnectedProxyRequest) ToProtoRequest() *proxypb.OnDisconnectedRequest {
	return &proxypb.OnDisconnectedRequest{
		SessionId: r.SessionID,
		Username:  r.Username,
	}
}

// OnDisconnectedProxyResponse represents a response from the OnDisconnected notification.
type OnDisconnectedProxyResponse struct {
	Error *sharedpb.Error
}

// FromProtoResponse creates an OnDisconnectedProxyResponse from the protobuf OnDisconnectedResponse.
func FromProtoOnDisconnectedResponse(resp *proxypb.OnDisconnectedResponse) *OnDisconnectedProxyResponse {
	if resp == nil {
		return &OnDisconnectedProxyResponse{}
	}
	return &OnDisconnectedProxyResponse{}
}
