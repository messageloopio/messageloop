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

// validServer supplies the always-required admin gRPC address and token plus
// the static namespace (require_auth is off, so server.namespace is mandatory).
func validServer() Server {
	return Server{
		GRPCAdmin: GRPCAdmin{Addr: "127.0.0.1:9091", AuthTokens: []string{"test-admin-token-0123456789"}},
		Namespace: "dev",
	}
}

func TestValidate_MinimalValid(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server:    validServer(),
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
	assert.ErrorContains(t, cfg.Validate(), "broker.redis.stream_approximate must be set to true explicitly")
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
		Server:    validServer(),
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
			Server:    validServer(),
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
	disabled := &Config{Transport: validTransport(), Server: validServer()}
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

// TestValidate_AdminAuthThreeWay pins the relaxed three-way requirement
// (design §2.7): the admin listener needs auth_tokens, an admin_auth proxy
// assignment, or an explicit allow_insecure — anything less fails Validate.
func TestValidate_AdminAuthThreeWay(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			GRPCAdmin: GRPCAdmin{Addr: "127.0.0.1:9091"},
			Namespace: "dev",
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.grpc_admin requires auth_tokens, a proxy entry with admin_auth: true, or allow_insecure: true")

	// allow_insecure satisfies the requirement (loopback-only, G5).
	cfg.Server.GRPCAdmin.AllowInsecure = true
	assert.NoError(t, cfg.Validate())

	// The static token list satisfies it (D28 list form).
	cfg.Server.GRPCAdmin.AllowInsecure = false
	cfg.Server.GRPCAdmin.AuthTokens = []string{"test-admin-token-0123456789"}
	assert.NoError(t, cfg.Validate())

	// An admin_auth-assigned proxy satisfies it (G3 explicit assignment).
	cfg.Server.GRPCAdmin.AuthTokens = nil
	cfg.Proxy = []ProxyConfig{{Name: "mlbridge", AdminAuth: true}}
	assert.NoError(t, cfg.Validate())
}

// TestValidate_AdminTokenLength pins the ≥20-characters-per-token gate
// (design D28).
func TestValidate_AdminTokenLength(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server:    validServer(),
	}
	cfg.Server.GRPCAdmin.AuthTokens = []string{"only-eighteen-chars"}
	assert.ErrorContains(t, cfg.Validate(), "server.grpc_admin.auth_tokens[0] must be at least 20 characters (got 19)")

	cfg.Server.GRPCAdmin.AuthTokens = []string{"this-one-is-long-enough-1234567890", "still-too-short"}
	assert.ErrorContains(t, cfg.Validate(), "server.grpc_admin.auth_tokens[1] must be at least 20 characters")
}

// TestValidate_AdminAuthCacheTTL pins the admin_auth_cache_ttl validation:
// an unparsable or non-positive value is rejected; an empty value (default)
// and a valid duration pass.
func TestValidate_AdminAuthCacheTTL(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server:    validServer(),
	}
	assert.NoError(t, cfg.Validate(), "an omitted admin_auth_cache_ttl uses the default")

	for _, ttl := range []string{"not-a-duration", "0s", "-5s"} {
		cfg.Server.GRPCAdmin.AdminAuthCacheTTL = ttl
		err := cfg.Validate()
		assert.Error(t, err, "ttl %q must be rejected", ttl)
		if ttl == "not-a-duration" {
			assert.ErrorContains(t, err, "invalid duration for server.grpc_admin.admin_auth_cache_ttl")
		} else {
			assert.ErrorContains(t, err, "admin_auth_cache_ttl must be positive")
		}
	}

	cfg.Server.GRPCAdmin.AdminAuthCacheTTL = "10s"
	assert.NoError(t, cfg.Validate())
}

// TestValidate_AdminAuthAssignmentUniqueness pins G3: at most one proxy
// entry may claim admin_auth, and the entry must carry a name.
func TestValidate_AdminAuthAssignmentUniqueness(t *testing.T) {
	base := func() *Config {
		return &Config{
			Transport: validTransport(),
			Server:    validServer(),
			Proxy:     []ProxyConfig{{Name: "mlbridge", AdminAuth: true}},
		}
	}
	assert.NoError(t, base().Validate(), "a single named admin_auth assignment is valid")

	cfg := base()
	cfg.Proxy[0].Name = ""
	assert.ErrorContains(t, cfg.Validate(), "proxy[0].admin_auth requires proxy[0].name")

	cfg = base()
	cfg.Proxy = append(cfg.Proxy, ProxyConfig{Name: "other", AdminAuth: true})
	assert.ErrorContains(t, cfg.Validate(), "proxy.admin_auth must be assigned to exactly one proxy entry (got 2)")
}

// TestValidate_G5FailClosedStartupGate pins the G5 fail-closed startup gate:
// allow_insecure on a non-loopback admin bind is a Validate error (formerly
// a WARN), and the admin HTTP listener follows the same rule with its
// auth_token standing in for allow_insecure.
func TestValidate_G5FailClosedStartupGate(t *testing.T) {
	cfg := &Config{
		Transport: validTransport(),
		Server: Server{
			GRPCAdmin: GRPCAdmin{Addr: "0.0.0.0:9091", AllowInsecure: true},
			Namespace: "dev",
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "server.grpc_admin.allow_insecure requires a loopback server.grpc_admin.addr")

	// Static tokens make the non-loopback bind acceptable without insecure.
	cfg.Server.GRPCAdmin.AllowInsecure = false
	cfg.Server.GRPCAdmin.AuthTokens = []string{"test-admin-token-0123456789"}
	assert.NoError(t, cfg.Validate())

	// allow_insecure on a loopback bind stays valid.
	cfg.Server.GRPCAdmin.AuthTokens = nil
	cfg.Server.GRPCAdmin.AllowInsecure = true
	cfg.Server.GRPCAdmin.Addr = "127.0.0.1:9091"
	assert.NoError(t, cfg.Validate())

	// Admin HTTP alignment: a non-loopback server.http bind without a token
	// is rejected; with a token it passes; loopback without a token passes.
	httpBase := func() *Config {
		return &Config{
			Transport: validTransport(),
			Server:    validServer(),
		}
	}
	cfg = httpBase()
	cfg.Server.Http.Addr = "0.0.0.0:8080"
	assert.ErrorContains(t, cfg.Validate(), "server.http.addr \"0.0.0.0:8080\" is a non-loopback bind and requires server.http.auth_token")

	cfg = httpBase()
	cfg.Server.Http.Addr = "0.0.0.0:8080"
	cfg.Server.Http.AuthToken = "http-metrics-token-0123456789"
	assert.NoError(t, cfg.Validate())

	cfg = httpBase()
	cfg.Server.Http.Addr = "127.0.0.1:8080"
	assert.NoError(t, cfg.Validate())
}

func TestValidate_QUICOptionalWhenEmpty(t *testing.T) {
	cfg := &Config{Transport: validTransport(), Server: validServer()}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_QUICRequiresTLSOrInsecure(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			QUIC:      QUICTransport{Addr: ":4433"},
		},
		Server: validServer(),
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

func TestValidate_KCPOptionalWhenEmpty(t *testing.T) {
	cfg := &Config{Transport: validTransport(), Server: validServer()}
	assert.NoError(t, cfg.Validate())
}

func TestValidate_KCPRequiresTLSOrInsecure(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			KCP:       KCPTransport{Addr: ":29900"},
		},
		Server: validServer(),
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.kcp requires tls")

	cfg.Transport.KCP.Insecure = true
	assert.NoError(t, cfg.Validate())
}

func TestValidate_KCPShards(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			KCP:       KCPTransport{Addr: ":29900", Insecure: true, ParityShards: 3},
		},
		Server: validServer(),
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.kcp.parity_shards requires transport.kcp.data_shards > 0")

	cfg.Transport.KCP.DataShards = 10
	assert.NoError(t, cfg.Validate())

	cfg.Transport.KCP.DataShards = -1
	assert.ErrorContains(t, cfg.Validate(), "must be >= 0")
}

func TestValidate_KCPInvalidDuration(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			KCP:       KCPTransport{Addr: ":29900", Insecure: true, WriteTimeout: "nope"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.kcp.write_timeout")
}

func TestValidate_KCPNonPositiveWriteTimeout(t *testing.T) {
	cfg := &Config{
		Transport: Transport{
			WebSocket: WebSocketTransport{Addr: ":9080", Path: "/ws"},
			GRPC:      GRPCTransport{Addr: ":9090"},
			KCP:       KCPTransport{Addr: ":29900", Insecure: true, WriteTimeout: "0s"},
		},
	}
	assert.ErrorContains(t, cfg.Validate(), "transport.kcp.write_timeout must be positive")
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

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

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
			GRPCAdmin: GRPCAdmin{Addr: "127.0.0.1:9091", AuthTokens: []string{"test-admin-token-0123456789"}, Capabilities: []string{
				"history.read", "presence.read", "channels.list", "session.act",
				"user.fanout", "subscribe.any", "presence.large_snapshot",
				"survey.bypass_gate", "pattern.global",
			}},
			Namespace: "dev",
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
			GRPCAdmin: validServer().GRPCAdmin,
			Namespace: "dev",
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
			GRPCAdmin: GRPCAdmin{Addr: "127.0.0.1:9091", AuthTokens: []string{"test-admin-token-0123456789"}, Capabilities: []string{}},
			Namespace: "dev",
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
		Server:    validServer(),
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
			Server:    Server{GRPCAdmin: validServer().GRPCAdmin, Presence: tc},
		}
		err := cfg.Validate()
		require.Error(t, err)
		require.Contains(t, err.Error(), "cluster_emit is removed")
	}
}
