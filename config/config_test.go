package config

import (
	"encoding/json"
	"testing"

	"github.com/fleetlit/messageloop/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfig_Structure(t *testing.T) {
	cfg := Config{
		Server: Server{
			Http: HttpServer{Addr: ":8080"},
		},
		Transport: Transport{
			WebSocket: WebSocketTransport{
				Addr: ":9080",
				Path: "/ws",
			},
			GRPC: GRPCTransport{
				Addr: ":9090",
			},
		},
		Broker: BrokerConfig{
			Type: "memory",
		},
	}

	assert.Equal(t, ":8080", cfg.Server.Http.Addr)
	assert.Equal(t, ":9080", cfg.Transport.WebSocket.Addr)
	assert.Equal(t, "/ws", cfg.Transport.WebSocket.Path)
	assert.Equal(t, ":9090", cfg.Transport.GRPC.Addr)
	assert.Equal(t, "memory", cfg.Broker.Type)
}

func TestConfig_JSONMarshal(t *testing.T) {
	cfg := Config{
		Server: Server{
			Http: HttpServer{Addr: ":8080"},
		},
		Transport: Transport{
			WebSocket: WebSocketTransport{
				Addr: ":9080",
				Path: "/ws",
			},
			GRPC: GRPCTransport{
				Addr: ":9090",
			},
		},
		Broker: BrokerConfig{
			Type:  "redis",
			Redis: RedisConfig{Addr: "localhost:6379"},
		},
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"addr":":8080"`)
	assert.Contains(t, string(data), `"type":"redis"`)
}

func TestConfig_JSONUnmarshal(t *testing.T) {
	jsonData := `{
		"server": {"http": {"addr": ":8080"}},
		"transport": {
			"websocket": {"addr": ":9080", "path": "/ws"},
			"grpc": {"addr": ":9090"}
		},
		"broker": {"type": "memory"},
		"proxy": []
	}`

	var cfg Config
	err := json.Unmarshal([]byte(jsonData), &cfg)
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.Server.Http.Addr)
	assert.Equal(t, ":9080", cfg.Transport.WebSocket.Addr)
	assert.Equal(t, "/ws", cfg.Transport.WebSocket.Path)
	assert.Equal(t, ":9090", cfg.Transport.GRPC.Addr)
	assert.Equal(t, "memory", cfg.Broker.Type)
}

func TestRedisConfig_Structure(t *testing.T) {
	cfg := RedisConfig{
		Addr:              "localhost:6379",
		Password:          "secret",
		DB:                1,
		PoolSize:          100,
		MinIdleConns:      10,
		MaxRetries:        5,
		DialTimeout:       "5s",
		ReadTimeout:       "3s",
		WriteTimeout:      "3s",
		StreamMaxLength:   10000,
		StreamApproximate: true,
		HistoryTTL:        "24h",
		ConsumerGroup:     "messageloop",
	}

	assert.Equal(t, "localhost:6379", cfg.Addr)
	assert.Equal(t, "secret", cfg.Password)
	assert.Equal(t, 1, cfg.DB)
	assert.Equal(t, 100, cfg.PoolSize)
	assert.Equal(t, 10, cfg.MinIdleConns)
	assert.Equal(t, 5, cfg.MaxRetries)
	assert.Equal(t, "5s", cfg.DialTimeout)
	assert.Equal(t, "3s", cfg.ReadTimeout)
	assert.Equal(t, "3s", cfg.WriteTimeout)
	assert.Equal(t, int64(10000), cfg.StreamMaxLength)
	assert.True(t, cfg.StreamApproximate)
	assert.Equal(t, "24h", cfg.HistoryTTL)
	assert.Equal(t, "messageloop", cfg.ConsumerGroup)
}

func TestProxyConfig_Structure(t *testing.T) {
	cfg := ProxyConfig{
		Name:     "test-proxy",
		Endpoint: "http://localhost:8080",
		Timeout:  "30s",
		HTTP: &proxy.HTTPProxyConfig{
			TLS: &proxy.TLSConfig{
				InsecureSkipVerify: true,
				ServerName:         "example.com",
			},
			Headers: map[string]string{
				"X-Custom": "value",
			},
		},
		GRPC: &proxy.GRPCProxyConfig{
			Insecure: true,
		},
		Routes: []proxy.RouteConfig{
			{Channel: "user.*", Method: "*"},
			{Channel: "chat.*", Method: "send"},
		},
	}

	assert.Equal(t, "test-proxy", cfg.Name)
	assert.Equal(t, "http://localhost:8080", cfg.Endpoint)
	assert.Equal(t, "30s", cfg.Timeout)
	assert.NotNil(t, cfg.HTTP)
	assert.True(t, cfg.HTTP.TLS.InsecureSkipVerify)
	assert.Equal(t, "example.com", cfg.HTTP.TLS.ServerName)
	assert.Equal(t, "value", cfg.HTTP.Headers["X-Custom"])
	assert.NotNil(t, cfg.GRPC)
	assert.True(t, cfg.GRPC.Insecure)
	assert.Len(t, cfg.Routes, 2)
}

func TestProxyConfig_ToProxyConfig(t *testing.T) {
	cfg := ProxyConfig{
		Name:     "test-proxy",
		Endpoint: "http://localhost:8080",
		Timeout:  "30s",
		HTTP: &proxy.HTTPProxyConfig{
			Headers: map[string]string{"X-Test": "value"},
		},
		Routes: []proxy.RouteConfig{
			{Channel: "user.*", Method: "*"},
		},
	}

	proxyCfg, err := cfg.ToProxyConfig()
	require.NoError(t, err)

	assert.Equal(t, "test-proxy", proxyCfg.Name)
	assert.Equal(t, "http://localhost:8080", proxyCfg.Endpoint)
	assert.Equal(t, "value", proxyCfg.HTTP.Headers["X-Test"])
	assert.Len(t, proxyCfg.Routes, 1)
}

func TestTransport_Structure(t *testing.T) {
	transport := Transport{
		WebSocket: WebSocketTransport{
			Addr: ":9080",
			Path: "/ws",
		},
		GRPC: GRPCTransport{
			Addr: ":9090",
		},
	}

	assert.Equal(t, ":9080", transport.WebSocket.Addr)
	assert.Equal(t, "/ws", transport.WebSocket.Path)
	assert.Equal(t, ":9090", transport.GRPC.Addr)
}

func TestBrokerConfig_Structure(t *testing.T) {
	tests := []struct {
		name string
		cfg  BrokerConfig
	}{
		{
			name: "memory broker",
			cfg: BrokerConfig{
				Type: "memory",
			},
		},
		{
			name: "redis broker",
			cfg: BrokerConfig{
				Type: "redis",
				Redis: RedisConfig{
					Addr:     "localhost:6379",
					Password: "secret",
					DB:       0,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.cfg.Type, tt.cfg.Type)
			if tt.cfg.Type == "redis" {
				assert.Equal(t, "localhost:6379", tt.cfg.Redis.Addr)
			}
		})
	}
}

func TestServer_Structure(t *testing.T) {
	server := Server{
		Http: HttpServer{Addr: ":8080"},
	}

	assert.Equal(t, ":8080", server.Http.Addr)
}

func TestHttpServer_Structure(t *testing.T) {
	server := HttpServer{Addr: ":8080"}
	assert.Equal(t, ":8080", server.Addr)
}

func TestWebSocketTransport_Structure(t *testing.T) {
	ws := WebSocketTransport{
		Addr: ":9080",
		Path: "/ws",
	}

	assert.Equal(t, ":9080", ws.Addr)
	assert.Equal(t, "/ws", ws.Path)
}

func TestGRPCTransport_Structure(t *testing.T) {
	grpc := GRPCTransport{
		Addr: ":9090",
	}

	assert.Equal(t, ":9090", grpc.Addr)
}

// Test config with empty proxy list
func TestConfig_EmptyProxy(t *testing.T) {
	cfg := Config{
		Broker: BrokerConfig{Type: "memory"},
		Proxy:  []ProxyConfig{},
	}

	assert.Empty(t, cfg.Proxy)
}

// Test config with multiple proxies
func TestConfig_MultipleProxies(t *testing.T) {
	cfg := Config{
		Broker: BrokerConfig{Type: "memory"},
		Proxy: []ProxyConfig{
			{Name: "user-service", Endpoint: "http://user:8080"},
			{Name: "chat-service", Endpoint: "http://chat:8080"},
			{Name: "presence-service", Endpoint: "http://presence:8080"},
		},
	}

	assert.Len(t, cfg.Proxy, 3)
	assert.Equal(t, "user-service", cfg.Proxy[0].Name)
	assert.Equal(t, "chat-service", cfg.Proxy[1].Name)
	assert.Equal(t, "presence-service", cfg.Proxy[2].Name)
}

// Test JSON round-trip
func TestConfig_JSONRoundTrip(t *testing.T) {
	original := Config{
		Server: Server{
			Http: HttpServer{Addr: ":8080"},
		},
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
		},
		Broker: BrokerConfig{Type: "memory"},
	}

	// Marshal
	data, err := json.Marshal(original)
	require.NoError(t, err)

	// Unmarshal
	var restored Config
	err = json.Unmarshal(data, &restored)
	require.NoError(t, err)

	assert.Equal(t, original.Server.Http.Addr, restored.Server.Http.Addr)
	assert.Equal(t, original.Transport.WebSocket.Addr, restored.Transport.WebSocket.Addr)
	assert.Equal(t, original.Broker.Type, restored.Broker.Type)
}

// Test RedisConfig with zero values
func TestRedisConfig_ZeroValues(t *testing.T) {
	cfg := RedisConfig{
		Addr: "localhost:6379",
	}

	assert.Equal(t, "localhost:6379", cfg.Addr)
	assert.Equal(t, "", cfg.Password)
	assert.Equal(t, 0, cfg.DB)
	assert.Equal(t, 0, cfg.PoolSize)
	assert.Equal(t, 0, cfg.MinIdleConns)
	assert.Equal(t, 0, cfg.MaxRetries)
	assert.Equal(t, "", cfg.DialTimeout)
	assert.Equal(t, "", cfg.ReadTimeout)
	assert.Equal(t, "", cfg.WriteTimeout)
	assert.Equal(t, int64(0), cfg.StreamMaxLength)
	assert.False(t, cfg.StreamApproximate)
	assert.Equal(t, "", cfg.HistoryTTL)
	assert.Equal(t, "", cfg.ConsumerGroup)
}

// Test ProxyConfig without HTTP/GRPC
func TestProxyConfig_NoTransport(t *testing.T) {
	cfg := ProxyConfig{
		Name:     "test",
		Endpoint: "http://localhost:8080",
	}

	assert.Nil(t, cfg.HTTP)
	assert.Nil(t, cfg.GRPC)
}

// Benchmark JSON marshal
func BenchmarkConfig_JSONMarshal(b *testing.B) {
	cfg := Config{
		Server: Server{
			Http: HttpServer{Addr: ":8080"},
		},
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
		},
		Broker: BrokerConfig{Type: "memory"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		json.Marshal(cfg)
	}
}

// Benchmark JSON unmarshal
func BenchmarkConfig_JSONUnmarshal(b *testing.B) {
	jsonData := `{
		"server": {"http": {"addr": ":8080"}},
		"transport": {
			"websocket": {"addr": ":9080", "path": "/ws"},
			"grpc": {"addr": ":9090"}
		},
		"broker": {"type": "memory"},
		"proxy": []
	}`

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var cfg Config
		json.Unmarshal([]byte(jsonData), &cfg)
	}
}
