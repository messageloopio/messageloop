package config

import "github.com/messageloopio/messageloop/proxy"

type Config struct {
	Server    Server        `yaml:"server" json:"server"`
	Transport Transport     `yaml:"transport" json:"transport"`
	Broker    BrokerConfig  `yaml:"broker" json:"broker"`
	Proxy     []ProxyConfig `yaml:"proxy" json:"proxy"`
}

type Server struct {
	Http       HttpServer `yaml:"http" json:"http"`
	Heartbeat  Heartbeat  `yaml:"heartbeat" json:"heartbeat"`
	RPCTimeout string     `yaml:"rpc_timeout" json:"rpc_timeout"` // default: "30s"
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

type WebSocketTransport struct {
	Addr        string `yaml:"addr" json:"addr"`
	Path        string `yaml:"path" json:"path"`
	ReadTimeout string `yaml:"read_timeout" json:"read_timeout"` // duration string
}

type GRPCTransport struct {
	Addr string `yaml:"addr" json:"addr"`
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
