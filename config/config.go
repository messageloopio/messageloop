package config

import (
	"fmt"
	"time"

	"github.com/messageloopio/messageloop/proxy"
)

type Config struct {
	Server    Server        `yaml:"server" json:"server" mapstructure:"server"`
	Transport Transport     `yaml:"transport" json:"transport" mapstructure:"transport"`
	Broker    BrokerConfig  `yaml:"broker" json:"broker" mapstructure:"broker"`
	Cluster   ClusterConfig `yaml:"cluster" json:"cluster" mapstructure:"cluster"`
	Proxy     []ProxyConfig `yaml:"proxy" json:"proxy" mapstructure:"proxy"`
}

// ClusterConfig configures distributed control-plane wiring.
type ClusterConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled" mapstructure:"enabled"`
	NodeID  string `yaml:"node_id" json:"node_id" mapstructure:"node_id"`
	Backend string `yaml:"backend" json:"backend" mapstructure:"backend"`
}

type Server struct {
	Http        HttpServer `yaml:"http" json:"http" mapstructure:"http"`
	GRPCAdmin   GRPCAdmin  `yaml:"grpc_admin" json:"grpc_admin" mapstructure:"grpc_admin"`
	Heartbeat   Heartbeat  `yaml:"heartbeat" json:"heartbeat" mapstructure:"heartbeat"`
	RPCTimeout  string     `yaml:"rpc_timeout" json:"rpc_timeout" mapstructure:"rpc_timeout"` // default: "30s"
	Limits      Limits     `yaml:"limits" json:"limits" mapstructure:"limits"`
	ACL         ACLConfig  `yaml:"acl" json:"acl" mapstructure:"acl"`
	RequireAuth bool       `yaml:"require_auth" json:"require_auth" mapstructure:"require_auth"` // Reject connections with empty token
}

// ACLConfig defines built-in channel access control rules.
// These rules are evaluated only when no proxy ACL is configured for a channel.
type ACLConfig struct {
	Rules []ACLRule `yaml:"rules" json:"rules" mapstructure:"rules"`
}

// ACLRule defines a single access control rule.
type ACLRule struct {
	ChannelPattern string   `yaml:"channel_pattern" json:"channel_pattern" mapstructure:"channel_pattern"`
	AllowSubscribe []string `yaml:"allow_subscribe" json:"allow_subscribe" mapstructure:"allow_subscribe"`
	AllowPublish   []string `yaml:"allow_publish" json:"allow_publish" mapstructure:"allow_publish"`
	DenyAll        bool     `yaml:"deny_all" json:"deny_all" mapstructure:"deny_all"`
}

type Limits struct {
	MaxConnectionsPerUser     int `yaml:"max_connections_per_user" json:"max_connections_per_user" mapstructure:"max_connections_per_user"`         // 0 = unlimited
	MaxSubscriptionsPerClient int `yaml:"max_subscriptions_per_client" json:"max_subscriptions_per_client" mapstructure:"max_subscriptions_per_client"` // 0 = unlimited
	MaxPublishesPerSecond     int `yaml:"max_publishes_per_second" json:"max_publishes_per_second" mapstructure:"max_publishes_per_second"`         // 0 = unlimited
	MaxMessageSize            int `yaml:"max_message_size" json:"max_message_size" mapstructure:"max_message_size"`                                 // bytes, 0 = default (64KB), applies uniformly to WebSocket and gRPC transports
}

type HttpServer struct {
	Addr string `yaml:"addr" json:"addr" mapstructure:"addr"`
}

type GRPCAdmin struct {
	Addr      string    `yaml:"addr" json:"addr" mapstructure:"addr"`
	TLS       TLSConfig `yaml:"tls" json:"tls" mapstructure:"tls"`
	AuthToken string    `yaml:"auth_token" json:"auth_token" mapstructure:"auth_token"` // Required bearer token for admin API calls
	// AllowInsecure explicitly opts out of the mandatory auth_token: the
	// admin API is served without authentication and a WARN is logged at
	// startup. Only for controlled environments.
	AllowInsecure bool `yaml:"allow_insecure" json:"allow_insecure" mapstructure:"allow_insecure"`
}

type Heartbeat struct {
	IdleTimeout string `yaml:"idle_timeout" json:"idle_timeout" mapstructure:"idle_timeout"` // default: "300s"
}

type Transport struct {
	WebSocket WebSocketTransport `yaml:"websocket" json:"websocket" mapstructure:"websocket"`
	GRPC      GRPCTransport      `yaml:"grpc" json:"grpc" mapstructure:"grpc"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file" json:"cert_file" mapstructure:"cert_file"`
	KeyFile  string `yaml:"key_file" json:"key_file" mapstructure:"key_file"`
}

type WebSocketTransport struct {
	Addr            string    `yaml:"addr" json:"addr" mapstructure:"addr"`
	Path            string    `yaml:"path" json:"path" mapstructure:"path"`
	ReadTimeout     string    `yaml:"read_timeout" json:"read_timeout" mapstructure:"read_timeout"`           // duration string
	WriteTimeout    string    `yaml:"write_timeout" json:"write_timeout" mapstructure:"write_timeout"`         // duration string, e.g. "10s"
	AllowAllOrigins bool      `yaml:"allow_all_origins" json:"allow_all_origins" mapstructure:"allow_all_origins"` // Allow any origin (development only)
	AllowedOrigins  []string  `yaml:"allowed_origins" json:"allowed_origins" mapstructure:"allowed_origins"`   // Whitelist of allowed origins
	TLS             TLSConfig `yaml:"tls" json:"tls" mapstructure:"tls"`
	Compression     bool      `yaml:"compression" json:"compression" mapstructure:"compression"` // Enable permessage-deflate

	// Deprecated: Use AllowAllOrigins instead.
	CheckOrigin bool `yaml:"check_origin" json:"check_origin" mapstructure:"check_origin"`
}

type GRPCTransport struct {
	Addr         string    `yaml:"addr" json:"addr" mapstructure:"addr"`
	WriteTimeout string    `yaml:"write_timeout" json:"write_timeout" mapstructure:"write_timeout"` // duration string, e.g. "10s"
	TLS          TLSConfig `yaml:"tls" json:"tls" mapstructure:"tls"`
}

// ProxyConfig wraps the proxy.ProxyConfig for YAML unmarshaling.
type ProxyConfig struct {
	Name     string                 `yaml:"name" json:"name" mapstructure:"name"`
	Endpoint string                 `yaml:"endpoint" json:"endpoint" mapstructure:"endpoint"`
	Timeout  string                 `yaml:"timeout" json:"timeout" mapstructure:"timeout"` // duration string
	HTTP     *proxy.HTTPProxyConfig `yaml:"http" json:"http" mapstructure:"http"`
	GRPC     *proxy.GRPCProxyConfig `yaml:"grpc" json:"grpc" mapstructure:"grpc"`
	Routes   []proxy.RouteConfig    `yaml:"routes" json:"routes" mapstructure:"routes"`
}

// ToProxyConfig converts the config YAML struct to proxy.ProxyConfig.
// The timeout duration string is parsed here so callers do not need to
// re-parse it.
func (c *ProxyConfig) ToProxyConfig() (*proxy.ProxyConfig, error) {
	pc := &proxy.ProxyConfig{
		Name:     c.Name,
		Endpoint: c.Endpoint,
		HTTP:     c.HTTP,
		GRPC:     c.GRPC,
		Routes:   c.Routes,
	}
	if c.Timeout != "" {
		timeout, err := time.ParseDuration(c.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout: %w", err)
		}
		pc.Timeout = timeout
	}
	return pc, nil
}

type BrokerConfig struct {
	Type  string      `yaml:"type" json:"type" mapstructure:"type"` // "memory" or "redis"
	Redis RedisConfig `yaml:"redis" json:"redis" mapstructure:"redis"`
}

type RedisConfig struct {
	Addr              string `yaml:"addr" json:"addr" mapstructure:"addr"`
	Password          string `yaml:"password" json:"password" mapstructure:"password"`
	DB                int    `yaml:"db" json:"db" mapstructure:"db"`
	PoolSize          int    `yaml:"pool_size" json:"pool_size" mapstructure:"pool_size"`
	MinIdleConns      int    `yaml:"min_idle_conns" json:"min_idle_conns" mapstructure:"min_idle_conns"`
	MaxRetries        int    `yaml:"max_retries" json:"max_retries" mapstructure:"max_retries"`
	DialTimeout       string `yaml:"dial_timeout" json:"dial_timeout" mapstructure:"dial_timeout"`
	ReadTimeout       string `yaml:"read_timeout" json:"read_timeout" mapstructure:"read_timeout"`
	WriteTimeout      string `yaml:"write_timeout" json:"write_timeout" mapstructure:"write_timeout"`
	StreamMaxLength   int64  `yaml:"stream_max_length" json:"stream_max_length" mapstructure:"stream_max_length"`
	StreamApproximate bool   `yaml:"stream_approximate" json:"stream_approximate" mapstructure:"stream_approximate"`
	HistoryTTL        string `yaml:"history_ttl" json:"history_ttl" mapstructure:"history_ttl"`
	ConsumerGroup     string `yaml:"consumer_group" json:"consumer_group" mapstructure:"consumer_group"`
}

// Validate checks the configuration for common errors and returns a descriptive error if any are found.
func (c *Config) Validate() error {
	// The startup wiring always constructs both transports: a WebSocket
	// listener (newWebSocketServer) and a client gRPC listener
	// (prepareGRPCServers). Validate the addresses here so a configuration
	// that would mis-bind or panic at startup is rejected up front.
	if c.Transport.WebSocket.Addr == "" {
		return fmt.Errorf("transport.websocket.addr is required")
	}
	if c.Transport.WebSocket.Path == "" {
		return fmt.Errorf("transport.websocket.path is required when websocket transport is enabled")
	}
	if c.Transport.GRPC.Addr == "" {
		return fmt.Errorf("transport.grpc.addr is required")
	}

	// Validate duration fields.
	for _, entry := range []struct {
		name  string
		value string
	}{
		{"server.heartbeat.idle_timeout", c.Server.Heartbeat.IdleTimeout},
		{"server.rpc_timeout", c.Server.RPCTimeout},
		{"transport.websocket.read_timeout", c.Transport.WebSocket.ReadTimeout},
		{"transport.websocket.write_timeout", c.Transport.WebSocket.WriteTimeout},
		{"transport.grpc.write_timeout", c.Transport.GRPC.WriteTimeout},
	} {
		if entry.value != "" {
			if _, err := time.ParseDuration(entry.value); err != nil {
				return fmt.Errorf("invalid duration for %s: %w", entry.name, err)
			}
		}
	}

	// Validate TLS pair completeness.
	for _, entry := range []struct {
		name string
		tls  TLSConfig
	}{
		{"server.grpc_admin.tls", c.Server.GRPCAdmin.TLS},
		{"transport.websocket.tls", c.Transport.WebSocket.TLS},
		{"transport.grpc.tls", c.Transport.GRPC.TLS},
	} {
		if (entry.tls.CertFile == "") != (entry.tls.KeyFile == "") {
			return fmt.Errorf("%s: cert_file and key_file must both be set or both be empty", entry.name)
		}
	}

	// Admin gRPC must be authenticated unless allow_insecure is explicit:
	// serving the admin API without any credential would expose session
	// takeover, publish, and disconnect capabilities to anyone on the wire.
	if c.Server.GRPCAdmin.Addr != "" && c.Server.GRPCAdmin.AuthToken == "" && !c.Server.GRPCAdmin.AllowInsecure {
		return fmt.Errorf("server.grpc_admin requires auth_token, or set allow_insecure: true to explicitly run without authentication")
	}

	// Validate broker config.
	switch c.Broker.Type {
	case "", "memory":
		// ok
	case "redis":
		if c.Broker.Redis.Addr == "" {
			return fmt.Errorf("broker.redis.addr is required when broker.type is redis")
		}
		// consumer_group is declared but never consumed by the Redis broker;
		// reject it instead of silently accepting a configuration that
		// appears to do something.
		if c.Broker.Redis.ConsumerGroup != "" {
			return fmt.Errorf("broker.redis.consumer_group is not implemented; remove it from the configuration")
		}
		// stream_approximate=false is silently ignored by the broker (only
		// approximate trimming is implemented, so it always behaves as true).
		// An unset field is indistinguishable from an explicit false after
		// parsing, so require the field to be explicitly true.
		if !c.Broker.Redis.StreamApproximate {
			return fmt.Errorf("broker.redis.stream_approximate: false is not supported (only approximate trimming is implemented); remove the field or set it to true")
		}
	default:
		return fmt.Errorf("unknown broker.type: %q (expected \"memory\" or \"redis\")", c.Broker.Type)
	}

	// Validate cluster requires redis broker.
	if c.Cluster.Enabled && c.Broker.Type != "redis" {
		return fmt.Errorf("cluster requires broker.type=redis")
	}

	return nil
}
