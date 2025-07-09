package config

type Config struct {
	Server    Server    `yaml:"server" json:"server"`
	Transport Transport `yaml:"transport" json:"transport"`
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
