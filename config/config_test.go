package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidate_MinimalValid(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
		},
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_NoTransport(t *testing.T) {
	cfg := &Config{}
	assert.ErrorContains(t, cfg.Validate(), "at least one transport address")
}

func TestValidate_InvalidDuration(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
		},
		Server: Server{
			RPCTimeout: "not-a-duration",
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "invalid duration for server.rpc_timeout")
}

func TestValidate_TLSMismatch(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{
				Addr: ":9080",
				TLS:  TLSConfig{CertFile: "cert.pem"},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "cert_file and key_file must both be set")
}

func TestValidate_UnknownBrokerType(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
		},
		Broker: BrokerConfig{Type: "kafka"},
	}
	assert.ErrorContains(t, cfg.Validate(), "unknown broker.type")
}

func TestValidate_RedisBrokerNoAddr(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
		},
		Broker: BrokerConfig{Type: "redis"},
	}
	assert.ErrorContains(t, cfg.Validate(), "broker.redis.addr is required")
}

func TestValidate_ClusterRequiresRedis(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
		},
		Cluster: ClusterConfig{Enabled: true},
		Broker:  BrokerConfig{Type: "memory"},
	}
	assert.ErrorContains(t, cfg.Validate(), "cluster requires broker.type=redis")
}

func TestValidate_ValidRedisCluster(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
		},
		Broker: BrokerConfig{
			Type:  "redis",
			Redis: RedisConfig{Addr: "localhost:6379"},
		},
		Cluster: ClusterConfig{Enabled: true, NodeID: "node-a", Backend: "redis"},
	}
	assert.NoError(t, cfg.Validate())
}
