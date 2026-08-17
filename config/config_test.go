package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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
		Cluster: ClusterConfig{Enabled: true, NodeID: "node-a", Backend: "redis", HMACKey: "0123456789abcdef0123456789abcdef"},
	}
	assert.NoError(t, cfg.Validate())
}

// PR-KA-B4: an enabled cluster must carry an HMAC key for the command bus.
func TestValidate_ClusterHMACKey(t *testing.T) {
	validBase := func() *Config {
		return &Config{
			Transport: validTransport(),
			Broker: BrokerConfig{
				Type:  "redis",
				Redis: RedisConfig{Addr: "localhost:6379", StreamApproximate: true},
			},
			Cluster: ClusterConfig{Enabled: true, NodeID: "node-a", Backend: "redis"},
		}
	}

	cfg := validBase()
	assert.ErrorContains(t, cfg.Validate(), "cluster.hmac_key is required",
		"enabled cluster without any key must fail validation")

	cfg = validBase()
	cfg.Cluster.HMACKey = "short"
	assert.ErrorContains(t, cfg.Validate(), "at least 32 bytes")

	cfg = validBase()
	cfg.Cluster.HMACKey = "0123456789abcdef0123456789abcdef"
	cfg.Cluster.HMACKeyFile = "/tmp/key"
	assert.ErrorContains(t, cfg.Validate(), "only one of hmac_key or hmac_key_file")

	cfg = validBase()
	cfg.Cluster.HMACKeyFile = "/run/secrets/cluster-hmac-key"
	assert.NoError(t, cfg.Validate(), "hmac_key_file alone is acceptable (the file is read at startup)")

	// A disabled cluster needs no key.
	disabled := &Config{Transport: validTransport()}
	assert.NoError(t, disabled.Validate(), "enabled: false must not require a key")
}

func TestClusterConfig_ResolveHMACKey(t *testing.T) {
	key32 := "0123456789abcdef0123456789abcdef"

	key, err := ClusterConfig{HMACKey: key32}.ResolveHMACKey()
	require.NoError(t, err)
	require.Equal(t, []byte(key32), key)

	_, err = ClusterConfig{}.ResolveHMACKey()
	require.ErrorContains(t, err, "cluster.hmac_key is required")

	_, err = ClusterConfig{HMACKey: "short"}.ResolveHMACKey()
	require.ErrorContains(t, err, "at least 32 bytes")

	// Key file: a single trailing newline (LF or CRLF) is trimmed.
	dir := t.TempDir()
	path := filepath.Join(dir, "hmac-key")
	require.NoError(t, os.WriteFile(path, []byte(key32+"\n"), 0o600))
	key, err = ClusterConfig{HMACKeyFile: path}.ResolveHMACKey()
	require.NoError(t, err)
	require.Equal(t, []byte(key32), key)

	require.NoError(t, os.WriteFile(path, []byte(key32+"\r\n"), 0o600))
	key, err = ClusterConfig{HMACKeyFile: path}.ResolveHMACKey()
	require.NoError(t, err)
	require.Equal(t, []byte(key32), key)

	// A too-short file content is rejected.
	require.NoError(t, os.WriteFile(path, []byte("short\n"), 0o600))
	_, err = ClusterConfig{HMACKeyFile: path}.ResolveHMACKey()
	require.ErrorContains(t, err, "at least 32 bytes")

	// An unreadable file fails startup wiring.
	_, err = ClusterConfig{HMACKeyFile: filepath.Join(dir, "missing")}.ResolveHMACKey()
	require.ErrorContains(t, err, "read cluster.hmac_key_file")

	// Both set is rejected.
	_, err = ClusterConfig{HMACKey: key32, HMACKeyFile: path}.ResolveHMACKey()
	require.ErrorContains(t, err, "only one of hmac_key or hmac_key_file")
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

func TestValidate_QUICOptionalWhenEmpty(t *testing.T) {
	cfg := &Config{Transport: validTransport()}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_QUICRequiresTLSOrInsecure(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			QUIC:      QUICTransport{Addr: ":4433"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.quic requires tls")

	cfg.Transport.QUIC.Insecure = true
	assert.NoError(t, cfg.Validate())
}

func TestValidate_QUICTLSPair(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			QUIC: QUICTransport{
				Addr: ":4433",
				TLS:  TLSConfig{CertFile: "cert.pem"},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "cert_file and key_file must both be set")
}

func TestValidate_QUICInvalidDuration(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			QUIC:      QUICTransport{Addr: ":4433", Insecure: true, WriteTimeout: "nope"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.quic.write_timeout")
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

// TestValidate_AuthorizerHistoryTTL verifies PR-KA-A4: an unparsable
// history_ttl on an authorizer rule fails Validate().
func TestValidate_AuthorizerHistoryTTL(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Rules: []AuthorizerRule{
					{Pattern: "im.**", ChannelPolicySpec: ChannelPolicySpec{HistoryTTL: "not-a-duration"}},
				},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.authorizer.rules[0].history_ttl")
}

// TestAuthorizer_ValidateEmptyPattern verifies that an authorizer rule
// without a pattern fails Validate().
func TestAuthorizer_ValidateEmptyPattern(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Rules: []AuthorizerRule{{Pattern: ""}},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.authorizer.rules[0].pattern is required")
}

// TestValidate_AuthorizerMiddleDoubleStarRejected pins the rule pattern
// contract: rule patterns are part of the subscription key language, so "**"
// is only allowed as the final segment ("a.**.b" is invalid) — the old ACL
// middle-"**" dialect is gone (PR-KA-A4 §5.1).
func TestValidate_AuthorizerMiddleDoubleStarRejected(t *testing.T) {
	for _, pattern := range []string{"a.**.b", "*.room", "im.*.tick", "*", "**"} {
		cfg := &Config{
			Transport: validTransport(),
			Server: Server{
				Authorizer: AuthorizerConfig{
					Rules: []AuthorizerRule{{Pattern: pattern}},
				},
			},
		}
		assert.ErrorContains(t, cfg.Validate(), "server.authorizer.rules[0].pattern", "pattern %q must be rejected", pattern)
	}
}

// TestValidate_AuthorizerNegativeHistorySize verifies history_size < 0 is
// rejected for both the default spec and rule specs.
func TestValidate_AuthorizerNegativeHistorySize(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Default: ChannelPolicySpec{HistorySize: intPtr(-1)},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.authorizer.default.history_size must be >= 0")

	cfg = &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Rules: []AuthorizerRule{
					{Pattern: "im.**", ChannelPolicySpec: ChannelPolicySpec{HistorySize: intPtr(-5)}},
				},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.authorizer.rules[0].history_size must be >= 0")
}

// TestValidate_AuthorizerMaxSurveyTimeout verifies an unparsable
// max_survey_timeout fails Validate().
func TestValidate_AuthorizerMaxSurveyTimeout(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Default: ChannelPolicySpec{MaxSurveyTimeout: "soon"},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.authorizer.default.max_survey_timeout")
}

// TestValidate_AuthorizerValid verifies a full, valid server.authorizer
// block passes validation.
func TestValidate_AuthorizerValid(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Default: ChannelPolicySpec{
					History:          boolPtr(true),
					HistorySize:      intPtr(0),
					HistoryTTL:       "24h",
					Presence:         boolPtr(true),
					Recover:          boolPtr(true),
					Survey:           boolPtr(false),
					MaxSurveyTimeout: "5s",
				},
				Rules: []AuthorizerRule{
					{Pattern: "game.tick.**", ChannelPolicySpec: ChannelPolicySpec{
						History:       boolPtr(false),
						Presence:      boolPtr(false),
						TransientOnly: boolPtr(true),
					}},
					{
						Pattern:           "im.**",
						DenyAll:           true,
						AllowSubscribe:    []string{"*"},
						AllowPublish:      []string{"alice"},
						ChannelPolicySpec: ChannelPolicySpec{History: boolPtr(true), HistorySize: intPtr(5000)},
					},
				},
			},
			GRPCAdmin: GRPCAdmin{Capabilities: []string{
				"history.read", "presence.read", "channels.list", "session.act",
				"user.fanout", "subscribe.any", "presence.large_snapshot",
				"survey.bypass_gate", "pattern.global",
			}},
		},
	}
	assert.NoError(t, cfg.Validate())
}

// TestValidate_RejectsServerACL verifies the removed server.acl block is
// rejected (KD-K31: no compatibility period), even when parsed from YAML.
func TestValidate_RejectsServerACL(t *testing.T) {
	var cfg Config
	require.NoError(t, yaml.Unmarshal([]byte("server:\n  acl:\n    rules:\n      - channel_pattern: chat.**\n        allow_survey: [\"*\"]\n"), &cfg))
	require.Len(t, cfg.Server.ACL.Rules, 1)
	require.Equal(t, []string{"*"}, cfg.Server.ACL.Rules[0].AllowSurvey)
	cfg.Transport = validTransport()
	assert.ErrorContains(t, cfg.Validate(), "server.acl is removed")

	// The same rules expressed under server.authorizer pass.
	cfg2 := &Config{
		Transport: validTransport(),
		Server: Server{
			Authorizer: AuthorizerConfig{
				Rules: []AuthorizerRule{
					{Pattern: "chat.**", AllowSurvey: []string{"*"}, ChannelPolicySpec: ChannelPolicySpec{Survey: boolPtr(true)}},
				},
			},
		},
	}
	assert.NoError(t, cfg2.Validate())
}

// TestValidate_RejectsServerChannels verifies the removed server.channels
// block is rejected, for both the default spec and rule lists.
func TestValidate_RejectsServerChannels(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{
				Policies: []ChannelPolicyRule{
					{Pattern: "im.**", ChannelPolicySpec: ChannelPolicySpec{History: boolPtr(false)}},
				},
			},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.channels is removed")

	cfg2 := &Config{
		Transport: validTransport(),
		Server: Server{
			Channels: ChannelConfig{Default: ChannelPolicySpec{History: boolPtr(false)}},
		},
	}
	assert.ErrorContains(t, cfg2.Validate(), "server.channels is removed")
}

// TestValidate_UnknownCapability verifies unknown capability names fail
// Validate() (the set is closed).
func TestValidate_UnknownCapability(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			GRPCAdmin: GRPCAdmin{Capabilities: []string{"history.read", "presence.write"}},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.grpc_admin.capabilities[1]: unknown capability \"presence.write\"")
}

// TestValidate_CapabilitiesEmptyAllowed verifies an explicit empty
// capabilities list is valid (it locks the admin data plane at runtime).
func TestValidate_CapabilitiesEmptyAllowed(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			GRPCAdmin: GRPCAdmin{Capabilities: []string{}},
		},
	}
	assert.NoError(t, cfg.Validate())
}

// TestValidate_PresenceClusterEmitRemoved verifies server.presence.cluster_emit
// is removed (PR-KA-B2): an absent field parses to nil and validates, while
// a YAML that spells the key (true or false) must fail Validate with a
// "cluster_emit is removed" message.
func TestValidate_PresenceClusterEmitRemoved(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
	}
	require.Nil(t, cfg.Server.Presence.ClusterEmit,
		"cluster_emit must parse to nil when absent")

	require.NoError(t, cfg.Validate())

	for _, tc := range []Presence{
		{ClusterEmit: boolPtr(true)},
		{ClusterEmit: boolPtr(false)},
	} {
		cfg = &Config{
			Transport: validTransport(),
			Server:    Server{Presence: tc},
		}
		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "cluster_emit is removed")
	}
}
