package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validTransport() Transport {
	return Transport{
		WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
		GRPC:      GRPCTransport{Addr: ":9090"},
	}
}

func TestValidate_MinimalValid(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_NoTransport(t *testing.T) {
	cfg := &Config{}
	assert.ErrorContains(t, cfg.Validate(), "transport.websocket.addr is required")
}

func TestValidate_GRPCAddrRequired(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.grpc.addr is required")
}

func TestValidate_WebSocketPathRequired(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080"},
			GRPC:      GRPCTransport{Addr: ":9090"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.websocket.path is required")
}

func TestValidate_InvalidDuration(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
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
				Path: "/ws",
				TLS:  TLSConfig{CertFile: "cert.pem"},
			},
			GRPC: GRPCTransport{Addr: ":9090"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "cert_file and key_file must both be set")
}

func TestValidate_UnknownBrokerType(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Broker:    BrokerConfig{Type: "kafka"},
	}
	assert.ErrorContains(t, cfg.Validate(), "unknown broker.type")
}

func TestValidate_RedisBrokerNoAddr(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Broker:    BrokerConfig{Type: "redis"},
	}
	assert.ErrorContains(t, cfg.Validate(), "broker.redis.addr is required")
}

func TestValidate_RedisConsumerGroupRejected(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Broker: BrokerConfig{
			Type:  "redis",
			Redis: RedisConfig{Addr: "localhost:6379", ConsumerGroup: "my-group"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "broker.redis.consumer_group is not implemented")
}

func TestValidate_RedisStreamApproximateFalseRejected(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Broker: BrokerConfig{
			Type:  "redis",
			Redis: RedisConfig{Addr: "localhost:6379", StreamApproximate: false},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "broker.redis.stream_approximate: false is not supported")
}

func TestValidate_ClusterRequiresRedis(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Cluster:   ClusterConfig{Enabled: true},
		Broker:    BrokerConfig{Type: "memory"},
	}
	assert.ErrorContains(t, cfg.Validate(), "cluster requires broker.type=redis")
}

func TestValidate_ValidRedisCluster(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Broker: BrokerConfig{
			Type:  "redis",
			Redis: RedisConfig{Addr: "localhost:6379", StreamApproximate: true},
		},
		Cluster: ClusterConfig{Enabled: true, NodeID: "node-a", Backend: "redis"},
	}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_AdminRequiresAuthToken(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			GRPCAdmin: GRPCAdmin{Addr: "127.0.0.1:9091"},
		},
	}
	assert.Error(t, cfg.Validate(), "empty admin auth token must fail validation")

	cfg.Server.GRPCAdmin.AllowInsecure = true
	assert.NoError(t, cfg.Validate(), "allow_insecure must bypass the auth token check")

	cfg.Server.GRPCAdmin.AllowInsecure = false
	cfg.Server.GRPCAdmin.AuthToken = "secret"
	assert.NoError(t, cfg.Validate(), "a configured auth token must pass validation")
}

func TestProxyConfig_ToProxyConfig_ParsesTimeout(t *testing.T) {
	pc := &ProxyConfig{Name: "p", Endpoint: "127.0.0.1:1", Timeout: "30s"}
	got, err := pc.ToProxyConfig()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, got.Timeout)
}

func TestProxyConfig_ToProxyConfig_EmptyTimeoutKeepsZero(t *testing.T) {
	pc := &ProxyConfig{Name: "p", Endpoint: "127.0.0.1:1"}
	got, err := pc.ToProxyConfig()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), got.Timeout)
}

func TestProxyConfig_ToProxyConfig_InvalidTimeout(t *testing.T) {
	pc := &ProxyConfig{Name: "p", Endpoint: "127.0.0.1:1", Timeout: "not-a-duration"}
	_, err := pc.ToProxyConfig()
	assert.ErrorContains(t, err, "invalid timeout")
}

func boolPtr(v bool) *bool    { return &v }
func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

// TestValidate_ChannelHistoryTTL verifies PR-02: an unparsable history_ttl
// on a channel policy rule fails Validate().
func TestValidate_ChannelHistoryTTL(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Policies: []ChannelPolicyRule{
					{Pattern: "im.**", ChannelPolicySpec: ChannelPolicySpec{HistoryTTL: "not-a-duration"}},
				},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels.policies[0].history_ttl")
}

// TestChannelPolicy_ValidateEmptyPattern verifies that a policy rule without
// a pattern fails Validate().
func TestChannelPolicy_ValidateEmptyPattern(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Policies: []ChannelPolicyRule{{Pattern: ""}},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels.policies[0].pattern is required")
}

// TestValidate_ChannelPolicyMiddleDoubleStarRejected pins the policy pattern
// contract: policy patterns are aligned with the topic matcher, so "**" is
// only allowed as the final segment ("a.**.b" is invalid) — unlike ACL
// patterns which additionally allow middle "**".
func TestValidate_ChannelPolicyMiddleDoubleStarRejected(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Policies: []ChannelPolicyRule{{Pattern: "a.**.b"}},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels.policies[0].pattern")
}

// TestValidate_ChannelPolicyNegativeHistorySize verifies history_size < 0 is
// rejected for both the default spec and policy rules.
func TestValidate_ChannelPolicyNegativeHistorySize(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Default: ChannelPolicySpec{HistorySize: intPtr(-1)},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels.default.history_size must be >= 0")

	cfg = &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Policies: []ChannelPolicyRule{
					{Pattern: "im.**", ChannelPolicySpec: ChannelPolicySpec{HistorySize: intPtr(-5)}},
				},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels.policies[0].history_size must be >= 0")
}

// TestValidate_ChannelPolicyMaxSurveyTimeout verifies an unparsable
// max_survey_timeout fails Validate().
func TestValidate_ChannelPolicyMaxSurveyTimeout(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Default: ChannelPolicySpec{MaxSurveyTimeout: "soon"},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels.default.max_survey_timeout")
}

// TestValidate_ChannelPolicyValid verifies a full, valid server.channels
// block passes validation.
func TestValidate_ChannelPolicyValid(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Default: ChannelPolicySpec{
					History:          boolPtr(true),
					HistorySize:      intPtr(0),
					HistoryTTL:       "24h",
					Presence:         boolPtr(true),
					Recover:          boolPtr(true),
					Survey:           boolPtr(false),
					MaxSurveyTimeout: "5s",
				},
				Policies: []ChannelPolicyRule{
					{Pattern: "game.tick.**", ChannelPolicySpec: ChannelPolicySpec{
						History:       boolPtr(false),
						Presence:      boolPtr(false),
						TransientOnly: boolPtr(true),
					}},
					{Pattern: "im.**", ChannelPolicySpec: ChannelPolicySpec{
						History:     boolPtr(true),
						HistorySize: intPtr(5000),
					}},
				},
			},
		},
	}
	assert.NoError(t, cfg.Validate())
}
