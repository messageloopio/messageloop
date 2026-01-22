package config

import "github.com/deeplooplabs/messageloop/proxy"

type Config struct {
	Server    Server        `yaml:"server" json:"server"`
	Transport Transport     `yaml:"transport" json:"transport"`
	Proxies   []ProxyConfig `yaml:"proxies" json:"proxies"`
}

type Server struct {
	Http HttpServer `yaml:"http" json:"http"`
}

type HttpServer struct {
	Addr string `yaml:"addr" json:"addr"`
}

type Transport struct {
	WebSocket WebSocketTransport `yaml:"websocket" json:"websocket"`
	GRPC      GRPCTransport      `yaml:"grpc" json:"grpc"`
}

type WebSocketTransport struct {
	Addr string `yaml:"addr" json:"addr"`
	Path string `yaml:"path" json:"path"`
}

type GRPCTransport struct {
	Addr string `yaml:"addr" json:"addr"`
}

// ProxyConfig wraps the proxy.ProxyConfig for YAML unmarshaling.
type ProxyConfig struct {
	Name     string                  `yaml:"name" json:"name"`
	Endpoint string                  `yaml:"endpoint" json:"endpoint"`
	Timeout  string                  `yaml:"timeout" json:"timeout"` // duration string
	HTTP     *proxy.HTTPProxyConfig  `yaml:"http" json:"http"`
	GRPC     *proxy.GRPCProxyConfig  `yaml:"grpc" json:"grpc"`
	Routes   []proxy.RouteConfig     `yaml:"routes" json:"routes"`
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
