package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	proxypb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/protocol/genproto/messageloop/shared/v2"
)

// Proxy defines the interface for proxying RPC requests to backend services.
type Proxy interface {
	// RPC forwards an RPC request to the backend service.
	RPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error)

	// Authenticate forwards an authentication request to the backend service.
	Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error)

	// SubscribeAcl forwards a subscription ACL check request to the backend service.
	SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error)

	// PublishAcl forwards a publish ACL check request to the backend service.
	PublishAcl(ctx context.Context, req *PublishAclProxyRequest) (*PublishAclProxyResponse, error)

	// OnConnected notifies the backend when a client connects.
	OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error)

	// OnSubscribed notifies the backend when a client subscribes to a channel.
	OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error)

	// OnUnsubscribed notifies the backend when a client unsubscribes from a channel.
	OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error)

	// OnDisconnected notifies the backend when a client disconnects.
	OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error)

	// AuthenticateAPIKey forwards a Server API key verification request to the backend service.
	AuthenticateAPIKey(ctx context.Context, req *AuthenticateAPIKeyProxyRequest) (*AuthenticateAPIKeyProxyResponse, error)

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
	Payload   *sharedv2.Payload
	Meta      map[string]string
}

// RPCProxyResponse represents a response from the proxy backend.
type RPCProxyResponse struct {
	Payload *sharedv2.Payload
	Meta    map[string]string
	Error   *sharedv2.Error
}

// ToProtoRequest converts an RPCProxyRequest to the protobuf RPCRequest.
func (r *RPCProxyRequest) ToProtoRequest() (*proxypb.RPCRequest, error) {
	var metadata *sharedv2.Metadata
	if r.Meta != nil {
		metadata = &sharedv2.Metadata{Entries: r.Meta}
	}
	return &proxypb.RPCRequest{
		Id:        r.ID,
		Channel:   r.Channel,
		Method:    r.Method,
		Payload:   r.Payload,
		Metadata:  metadata,
		UserId:    r.UserID,
		SessionId: r.SessionID,
		ClientId:  r.ClientID,
	}, nil
}

// FromProtoResponse creates an RPCProxyResponse from the protobuf RPCResponse.
func FromProtoReply(reply *proxypb.RPCResponse) (*RPCProxyResponse, error) {
	if reply == nil {
		return &RPCProxyResponse{}, nil
	}
	var meta map[string]string
	if reply.Metadata != nil {
		meta = reply.Metadata.Entries
	}
	return &RPCProxyResponse{
		Payload: reply.Payload,
		Meta:    meta,
		Error:   reply.Error,
	}, nil
}

// ProxyConfig is the configuration for a single proxy instance.
type ProxyConfig struct {
	// Name is a unique identifier for this proxy.
	Name string `yaml:"name" json:"name" mapstructure:"name"`

	// Endpoint is the backend service endpoint URL.
	// For HTTP: "http://localhost:8001/rpc" or "https://service.example.com/api"
	// For gRPC: "localhost:9001" or "service.example.com:9001"
	Endpoint string `yaml:"endpoint" json:"endpoint" mapstructure:"endpoint"`

	// Service selects a discovery-based backend (v1.1 PR-B): the registry
	// service name whose healthy instances are resolved at request time.
	// Mutually exclusive with Endpoint — exactly one of the two must be set
	// (Validate enforces it when the proxies are wired).
	Service string `yaml:"service" json:"service" mapstructure:"service"`

	// Routing configures instance selection for a service backend. It is
	// ignored for static Endpoint backends.
	Routing RoutingConfig `yaml:"routing" json:"routing" mapstructure:"routing"`

	// Timeout is the request timeout for this proxy.
	// If zero, the default timeout is used.
	Timeout time.Duration `yaml:"timeout" json:"timeout" mapstructure:"timeout"`

	// HTTP configures HTTP-specific settings.
	HTTP *HTTPProxyConfig `yaml:"http" json:"http" mapstructure:"http"`

	// GRPC configures gRPC-specific settings.
	GRPC *GRPCProxyConfig `yaml:"grpc" json:"grpc" mapstructure:"grpc"`

	// Routes defines which requests should be routed to this proxy.
	Routes []RouteConfig `yaml:"routes" json:"routes" mapstructure:"routes"`
}

// Routing strategy names for service-type proxy backends (v1.1 design §4.3).
const (
	// RoutingStrategyConsistentHash routes each RPC by a routing key through
	// the Rendezvous (HRW) hash: the same key lands on the same instance on
	// every node, with zero coordination. It is the default when
	// routing.strategy is omitted.
	RoutingStrategyConsistentHash = "consistent_hash"
	// RoutingStrategyRoundRobin rotates across the healthy instance snapshot
	// per node; counters are independent per messageloop node.
	RoutingStrategyRoundRobin = "round_robin"
	// RoutingStrategyRandom picks a uniformly random healthy instance per
	// request.
	RoutingStrategyRandom = "random"
	// RoutingStrategySticky is reserved for the phase-2 key lease directory;
	// configuring it is a Validate error in this phase.
	RoutingStrategySticky = "sticky"
)

// DefaultRoutingStrategy is applied when RoutingConfig.Strategy is empty.
const DefaultRoutingStrategy = RoutingStrategyConsistentHash

// Routing key syntax for keyed strategies (v1.1 design §4.3).
const (
	// RoutingKeyChannel routes by the raw channel string, namespace prefix
	// included.
	RoutingKeyChannel = "channel"
	// RoutingKeyMetadataPrefix prefixes a request metadata entry name: the
	// routing key is read from that entry, falling back to the channel when
	// it is missing or empty.
	RoutingKeyMetadataPrefix = "metadata:"
)

// DefaultRoutingKey is applied when RoutingConfig.Key is empty.
const DefaultRoutingKey = RoutingKeyChannel

// RoutingConfig selects how requests to a service-type proxy pick an
// instance (v1.1 design §4.3/§4.4). The zero value routes RPCs by channel
// through the consistent hash.
type RoutingConfig struct {
	// Strategy is one of "consistent_hash" (default), "round_robin",
	// "random", or "sticky" (reserved for phase 2 and rejected by Validate).
	Strategy string `yaml:"strategy" json:"strategy" mapstructure:"strategy"`

	// Key is the routing key source used by keyed strategies: "channel"
	// (default) or "metadata:<name>". It is only valid under a keyed
	// strategy (consistent_hash); configuring it under round_robin or
	// random is a Validate error, so a mistyped strategy fails at startup
	// instead of silently ignoring the key.
	Key string `yaml:"key" json:"key" mapstructure:"key"`
}

// Validate checks the proxy configuration for errors that would otherwise
// surface only at request time (v1.1 design §4.4): exactly one of
// endpoint/service must be set, and the routing block must be coherent. It
// runs when the proxies are wired (Node.SetupProxy), before any backend is
// constructed.
func (c *ProxyConfig) Validate() error {
	switch {
	case c.Endpoint != "" && c.Service != "":
		return fmt.Errorf("proxy %s: endpoint and service are mutually exclusive; configure exactly one", c.Name)
	case c.Endpoint == "" && c.Service == "":
		return fmt.Errorf("proxy %s: either endpoint or service is required", c.Name)
	}
	return c.Routing.validate(c.Name)
}

// validate checks the routing block semantics (v1.1 design §4.4 rule 3):
// the strategy value domain, the sticky reservation, and the rule that a
// routing key only appears under a keyed strategy.
func (r RoutingConfig) validate(name string) error {
	keyed := false
	switch r.Strategy {
	case "", RoutingStrategyConsistentHash:
		keyed = true
	case RoutingStrategyRoundRobin, RoutingStrategyRandom:
		// Unkeyed strategies: key extraction is skipped entirely.
	case RoutingStrategySticky:
		return fmt.Errorf("proxy %s: routing.strategy %q is reserved for a future release; use consistent_hash, round_robin or random", name, r.Strategy)
	default:
		return fmt.Errorf("proxy %s: unknown routing.strategy %q (expected %q, %q or %q)", name, r.Strategy, RoutingStrategyConsistentHash, RoutingStrategyRoundRobin, RoutingStrategyRandom)
	}
	if r.Key == "" {
		return nil
	}
	if !keyed {
		return fmt.Errorf("proxy %s: routing.key is only valid with the %q strategy (round_robin/random are keyless by design)", name, RoutingStrategyConsistentHash)
	}
	if r.Key == RoutingKeyChannel {
		return nil
	}
	if meta, ok := strings.CutPrefix(r.Key, RoutingKeyMetadataPrefix); ok {
		if meta == "" {
			return fmt.Errorf("proxy %s: routing.key %q requires a non-empty metadata field name", name, r.Key)
		}
		return nil
	}
	return fmt.Errorf("proxy %s: unknown routing.key %q (expected %q or %q<name>)", name, r.Key, RoutingKeyChannel, RoutingKeyMetadataPrefix)
}

// HTTPProxyConfig contains HTTP-specific proxy settings.
type HTTPProxyConfig struct {
	// TLS configures HTTPS settings.
	TLS *TLSConfig `yaml:"tls" json:"tls" mapstructure:"tls"`

	// Headers are additional headers to include in requests.
	Headers map[string]string `yaml:"headers" json:"headers" mapstructure:"headers"`
}

// TLSConfig contains TLS configuration for HTTP proxies.
type TLSConfig struct {
	// InsecureSkipVerify disables TLS certificate verification.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" json:"insecure_skip_verify" mapstructure:"insecure_skip_verify"`

	// ServerName is the expected server name.
	ServerName string `yaml:"server_name" json:"server_name" mapstructure:"server_name"`
}

// GRPCProxyConfig contains gRPC-specific proxy settings.
type GRPCProxyConfig struct {
	// Insecure disables TLS for gRPC connections.
	Insecure bool `yaml:"insecure" json:"insecure" mapstructure:"insecure"`

	// ServerName overrides the TLS server name used for certificate
	// verification. When empty, grpc-go derives it from the endpoint
	// authority.
	ServerName string `yaml:"server_name" json:"server_name" mapstructure:"server_name"`

	// InsecureSkipVerify disables TLS certificate verification for secure
	// connections.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify" json:"insecure_skip_verify" mapstructure:"insecure_skip_verify"`
}

// RouteConfig defines a routing rule for the proxy.
type RouteConfig struct {
	// Channel is the channel pattern to match. Supports glob wildcards.
	// Example: "chat.*" matches "chat.messages", "chat.presence", etc.
	Channel string `yaml:"channel" json:"channel" mapstructure:"channel"`

	// Method is the method pattern to match. Supports glob wildcards.
	// Example: "user.*" matches "user.get", "user.set", etc.
	// Use "*" to match all methods.
	Method string `yaml:"method" json:"method" mapstructure:"method"`
}

// DefaultRPCTimeout is the default timeout for RPC proxy requests.
const DefaultRPCTimeout = 30 * time.Second

// AuthenticateProxyRequest represents an authentication request to be proxied.
type AuthenticateProxyRequest struct {
	ClientID   string
	Token      string
	ClientType string
	SessionID  string
	RemoteAddr string
}

// ToProtoRequest converts an AuthenticateProxyRequest to the protobuf AuthenticateRequest.
func (r *AuthenticateProxyRequest) ToProtoRequest() *proxypb.AuthenticateRequest {
	return &proxypb.AuthenticateRequest{
		ClientId:   r.ClientID,
		Token:      r.Token,
		ClientType: r.ClientType,
		SessionId:  r.SessionID,
		RemoteAddr: r.RemoteAddr,
	}
}

// AuthenticateProxyResponse represents an authentication response from the proxy backend.
type AuthenticateProxyResponse struct {
	Error    *sharedv2.Error
	UserInfo *UserInfo
}

// UserInfo represents user information returned from authentication.
type UserInfo struct {
	ID         string
	Username   string
	Token      string
	ClientType string
	ClientID   string
	// Namespace scopes the session to a multi-tenant namespace ("" = server
	// static fallback).
	Namespace string
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
			Namespace:  resp.UserInfo.Namespace,
		}
	}
	return &AuthenticateProxyResponse{
		Error:    resp.Error,
		UserInfo: userInfo,
	}
}

// SubscribeAclProxyRequest represents a subscription ACL check request to be proxied.
type SubscribeAclProxyRequest struct {
	Channel   string
	Token     string
	UserID    string
	SessionID string
}

// ToProtoRequest converts a SubscribeAclProxyRequest to the protobuf SubscribeAclRequest.
func (r *SubscribeAclProxyRequest) ToProtoRequest() *proxypb.SubscribeAclRequest {
	return &proxypb.SubscribeAclRequest{
		Channel:   r.Channel,
		Token:     r.Token,
		UserId:    r.UserID,
		SessionId: r.SessionID,
	}
}

// SubscribeAclProxyResponse represents a subscription ACL response from the proxy backend.
type SubscribeAclProxyResponse struct {
	Error *sharedv2.Error
}

// FromProtoResponse creates a SubscribeAclProxyResponse from the protobuf SubscribeAclResponse.
func FromProtoSubscribeAclResponse(resp *proxypb.SubscribeAclResponse) *SubscribeAclProxyResponse {
	if resp == nil {
		return &SubscribeAclProxyResponse{}
	}
	return &SubscribeAclProxyResponse{Error: resp.Error}
}

// PublishAclProxyRequest represents a publish ACL check request to be proxied.
type PublishAclProxyRequest struct {
	Channel   string
	Token     string
	UserID    string
	SessionID string
}

// ToProtoRequest converts a PublishAclProxyRequest to the protobuf PublishAclRequest.
func (r *PublishAclProxyRequest) ToProtoRequest() *proxypb.PublishAclRequest {
	return &proxypb.PublishAclRequest{
		Channel:   r.Channel,
		Token:     r.Token,
		UserId:    r.UserID,
		SessionId: r.SessionID,
	}
}

// PublishAclProxyResponse represents a publish ACL response from the proxy backend.
type PublishAclProxyResponse struct {
	Error *sharedv2.Error
}

// FromProtoPublishAclResponse creates a PublishAclProxyResponse from the protobuf PublishAclResponse.
func FromProtoPublishAclResponse(resp *proxypb.PublishAclResponse) *PublishAclProxyResponse {
	if resp == nil {
		return &PublishAclProxyResponse{}
	}
	return &PublishAclProxyResponse{Error: resp.Error}
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
	Error *sharedv2.Error
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
	Error *sharedv2.Error
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
	Error *sharedv2.Error
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
	Error *sharedv2.Error
}

// FromProtoResponse creates an OnDisconnectedProxyResponse from the protobuf OnDisconnectedResponse.
func FromProtoOnDisconnectedResponse(resp *proxypb.OnDisconnectedResponse) *OnDisconnectedProxyResponse {
	if resp == nil {
		return &OnDisconnectedProxyResponse{}
	}
	return &OnDisconnectedProxyResponse{}
}

// AuthenticateAPIKeyProxyRequest represents a Server API key verification request to be proxied.
type AuthenticateAPIKeyProxyRequest struct {
	APIKey     string
	RemoteAddr string
}

// ToProtoRequest converts an AuthenticateAPIKeyProxyRequest to the protobuf AuthenticateAPIKeyRequest.
func (r *AuthenticateAPIKeyProxyRequest) ToProtoRequest() *proxypb.AuthenticateAPIKeyRequest {
	return &proxypb.AuthenticateAPIKeyRequest{
		ApiKey:     r.APIKey,
		RemoteAddr: r.RemoteAddr,
	}
}

// AuthenticateAPIKeyProxyResponse represents a Server API key authentication response from the proxy backend.
type AuthenticateAPIKeyProxyResponse struct {
	Error    *sharedv2.Error
	Identity *APIKeyInfo
}

// APIKeyInfo represents the Server API caller identity decided by the proxy backend.
// KeyID is the backend's unique key ID (not the display name: allow lists match
// principals as "key:<id>" and display names are not unique — design D26).
// Empty Namespaces means deny everything (fail-closed); Capabilities is a
// closed set of capability names where empty means no capabilities.
// MaxAgeSeconds is a relative duration (how much longer this verification
// result may be reused, avoiding clock skew between the two systems); 0 means
// the caller's configured TTL.
type APIKeyInfo struct {
	KeyID         string
	Namespaces    []string
	Capabilities  []string
	MaxAgeSeconds int64
}

// FromProtoAuthenticateAPIKeyResponse creates an AuthenticateAPIKeyProxyResponse from the protobuf AuthenticateAPIKeyResponse.
// Identity stays nil when the backend decided no identity (e.g. it answered with an error only).
func FromProtoAuthenticateAPIKeyResponse(resp *proxypb.AuthenticateAPIKeyResponse) *AuthenticateAPIKeyProxyResponse {
	if resp == nil {
		return &AuthenticateAPIKeyProxyResponse{}
	}
	var identity *APIKeyInfo
	if resp.Identity != nil {
		identity = &APIKeyInfo{
			KeyID:         resp.Identity.KeyId,
			Namespaces:    resp.Identity.Namespaces,
			Capabilities:  resp.Identity.Capabilities,
			MaxAgeSeconds: resp.Identity.MaxAgeSeconds,
		}
	}
	return &AuthenticateAPIKeyProxyResponse{
		Error:    resp.Error,
		Identity: identity,
	}
}
