package config

import "github.com/messageloopio/messageloop/proxy"

type Config struct {
	Server    Server        `yaml:"server" json:"server"`
	Transport Transport     `yaml:"transport" json:"transport"`
	Broker    BrokerConfig  `yaml:"broker" json:"broker"`
	Cluster   ClusterConfig `yaml:"cluster" json:"cluster"`
	Proxy     []ProxyConfig `yaml:"proxy" json:"proxy"`
}

// ClusterConfig configures distributed control-plane wiring.
type ClusterConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	NodeID  string `yaml:"node_id" json:"node_id"`
	Backend string `yaml:"backend" json:"backend"`
}

type Server struct {
	Http       HttpServer `yaml:"http" json:"http"`
	Heartbeat  Heartbeat  `yaml:"heartbeat" json:"heartbeat"`
	RPCTimeout string     `yaml:"rpc_timeout" json:"rpc_timeout"` // default: "30s"
	Limits     Limits     `yaml:"limits" json:"limits"`
	ACL        ACLConfig  `yaml:"acl" json:"acl"`
}

// ACLConfig defines built-in channel access control rules.
// These rules are evaluated only when no proxy ACL is configured for a channel.
type ACLConfig struct {
	Rules []ACLRule `yaml:"rules" json:"rules"`
}

// ACLRule defines a single access control rule.
type ACLRule struct {
	ChannelPattern string   `yaml:"channel_pattern" json:"channel_pattern"`
	AllowSubscribe []string `yaml:"allow_subscribe" json:"allow_subscribe"`
	AllowPublish   []string `yaml:"allow_publish" json:"allow_publish"`
	DenyAll        bool     `yaml:"deny_all" json:"deny_all"`
}

type Limits struct {
	MaxConnectionsPerUser     int `yaml:"max_connections_per_user" json:"max_connections_per_user"`         // 0 = unlimited
	MaxSubscriptionsPerClient int `yaml:"max_subscriptions_per_client" json:"max_subscriptions_per_client"` // 0 = unlimited
	MaxPublishesPerSecond     int `yaml:"max_publishes_per_second" json:"max_publishes_per_second"`         // 0 = unlimited
}

type HttpServer struct {
	Addr string `yaml:"addr" json:"addr"`
}

type Heartbeat struct {
	IdleTimeout string `yaml:"idle_timeout" json:"idle_timeout"` // default: "300s"
}

type Transport struct {
	WebSocket WebSocketTransport `yaml:"websocket" json:"websocket"`
	GRPC      GRPCTransport      `yaml:"grpc" json:"grpc"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file" json:"cert_file"`
	KeyFile  string `yaml:"key_file" json:"key_file"`
}

type WebSocketTransport struct {
	Addr         string    `yaml:"addr" json:"addr"`
	Path         string    `yaml:"path" json:"path"`
	ReadTimeout  string    `yaml:"read_timeout" json:"read_timeout"`   // duration string
	WriteTimeout string    `yaml:"write_timeout" json:"write_timeout"` // duration string, e.g. "10s"
	CheckOrigin  bool      `yaml:"check_origin" json:"check_origin"`   // Allow any origin when true
	TLS          TLSConfig `yaml:"tls" json:"tls"`
	Compression  bool      `yaml:"compression" json:"compression"` // Enable permessage-deflate
}

type GRPCTransport struct {
	Addr         string    `yaml:"addr" json:"addr"`
	WriteTimeout string    `yaml:"write_timeout" json:"write_timeout"` // duration string, e.g. "10s"
	TLS          TLSConfig `yaml:"tls" json:"tls"`
}

// ProxyConfig wraps the proxy.ProxyConfig for YAML unmarshaling.
type ProxyConfig struct {
	Name     string                 `yaml:"name" json:"name"`
	Endpoint string                 `yaml:"endpoint" json:"endpoint"`
	Timeout  string                 `yaml:"timeout" json:"timeout"` // duration string
	HTTP     *proxy.HTTPProxyConfig `yaml:"http" json:"http"`
	GRPC     *proxy.GRPCProxyConfig `yaml:"grpc" json:"grpc"`
	Routes   []proxy.RouteConfig    `yaml:"routes" json:"routes"`
}

// ToProxyConfig converts the config YAML struct to proxy.ProxyConfig.
func (c *ProxyConfig) ToProxyConfig() (*proxy.ProxyConfig, error) {
	return &proxy.ProxyConfig{
		Name:     c.Name,
		Endpoint: c.Endpoint,
		HTTP:     c.HTTP,
		GRPC:     c.GRPC,
		Routes:   c.Routes,
	}, nil
}

type BrokerConfig struct {
	Type  string      `yaml:"type" json:"type"` // "memory" or "redis"
	Redis RedisConfig `yaml:"redis" json:"redis"`
}

type RedisConfig struct {
	Addr              string `yaml:"addr" json:"addr"`
	Password          string `yaml:"password" json:"password"`
	DB                int    `yaml:"db" json:"db"`
	PoolSize          int    `yaml:"pool_size" json:"pool_size"`
	MinIdleConns      int    `yaml:"min_idle_conns" json:"min_idle_conns"`
	MaxRetries        int    `yaml:"max_retries" json:"max_retries"`
	DialTimeout       string `yaml:"dial_timeout" json:"dial_timeout"`
	ReadTimeout       string `yaml:"read_timeout" json:"read_timeout"`
	WriteTimeout      string `yaml:"write_timeout" json:"write_timeout"`
	StreamMaxLength   int64  `yaml:"stream_max_length" json:"stream_max_length"`
	StreamApproximate bool   `yaml:"stream_approximate" json:"stream_approximate"`
	HistoryTTL        string `yaml:"history_ttl" json:"history_ttl"`
	ConsumerGroup     string `yaml:"consumer_group" json:"consumer_group"`
}
