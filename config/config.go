package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/messageloopio/messageloop/proxy"
)

// CapabilityNames is the closed set of admin capability names accepted under
// server.grpc_admin.capabilities. It mirrors internal/authz's
// ClosedCapabilityNames (internal/authz cannot import config, so the two
// lists are kept in sync manually).
var CapabilityNames = map[string]struct{}{
	"presence.large_snapshot": {},
	"survey.bypass_gate":      {},
	"history.read":            {},
	"presence.read":           {},
	"channels.list":           {},
	"session.act":             {},
	"user.fanout":             {},
	"subscribe.any":           {},
	"pattern.global":          {},
}

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
	// HMACKey is the inline HMAC-SHA256 key for the cluster command bus
	// (at least 32 bytes). Exactly one of HMACKey / HMACKeyFile must be set
	// when the cluster is enabled. The key never enters Redis, logs, or
	// metrics labels.
	HMACKey string `yaml:"hmac_key" json:"hmac_key" mapstructure:"hmac_key"`
	// HMACKeyFile points to a file holding the HMAC key; a single trailing
	// newline is trimmed when the file is read at startup.
	HMACKeyFile string `yaml:"hmac_key_file" json:"hmac_key_file" mapstructure:"hmac_key_file"`
}

// minClusterHMACKeyBytes is the minimum accepted length of the cluster
// command-bus HMAC key.
const minClusterHMACKeyBytes = 32

// ResolveHMACKey returns the cluster command-bus HMAC key bytes from
// HMACKey or HMACKeyFile. A key file's single trailing newline (LF or CRLF)
// is trimmed. It fails when no key is configured, the file cannot be read,
// or the resolved key is shorter than 32 bytes — the process must refuse to
// start rather than run an unprotected command bus.
func (c ClusterConfig) ResolveHMACKey() ([]byte, error) {
	if c.HMACKey != "" && c.HMACKeyFile != "" {
		return nil, fmt.Errorf("only one of hmac_key or hmac_key_file may be set")
	}
	var key []byte
	switch {
	case c.HMACKey != "":
		key = []byte(c.HMACKey)
	case c.HMACKeyFile != "":
		data, err := os.ReadFile(c.HMACKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read cluster.hmac_key_file: %w", err)
		}
		key = bytes.TrimSuffix(data, []byte("\n"))
		key = bytes.TrimSuffix(key, []byte("\r"))
	default:
		return nil, fmt.Errorf("cluster.hmac_key is required when cluster is enabled (or set cluster.hmac_key_file)")
	}
	if len(key) < minClusterHMACKeyBytes {
		return nil, fmt.Errorf("cluster hmac key must be at least 32 bytes")
	}
	return key, nil
}

type Server struct {
	Http        HttpServer `yaml:"http" json:"http" mapstructure:"http"`
	GRPCAdmin   GRPCAdmin  `yaml:"grpc_admin" json:"grpc_admin" mapstructure:"grpc_admin"`
	Heartbeat   Heartbeat  `yaml:"heartbeat" json:"heartbeat" mapstructure:"heartbeat"`
	RPCTimeout  string     `yaml:"rpc_timeout" json:"rpc_timeout" mapstructure:"rpc_timeout"` // default: "30s"
	Limits      Limits     `yaml:"limits" json:"limits" mapstructure:"limits"`
	RequireAuth bool       `yaml:"require_auth" json:"require_auth" mapstructure:"require_auth"` // Reject connections with empty token
	Presence    Presence   `yaml:"presence" json:"presence" mapstructure:"presence"`
	// Authorizer is the single authorization table (PR-KA-A4 §6): pattern →
	// allow lists / deny_all / Effects. It replaces the old server.acl and
	// server.channels blocks.
	Authorizer AuthorizerConfig `yaml:"authorizer" json:"authorizer" mapstructure:"authorizer"`
	// ACL and Channels are removed in PR-KA-A4 (KD-K31: no compatibility
	// period). The fields stay declared so YAML still parses and Validate can
	// reject them explicitly — nothing else may read them.
	ACL      ACLConfig     `yaml:"acl" json:"acl" mapstructure:"acl"`
	Channels ChannelConfig `yaml:"channels" json:"channels" mapstructure:"channels"`
}

// Presence is the process-wide presence control-plane switch.
// It is not a channel policy (those stay under server.channels).
type Presence struct {
	// ClusterEmit is removed in PR-KA-B2: occupancy always crosses nodes
	// over the LiveBus (exact channels + compiled Interest), so the old
	// dual-path switch is gone. The field stays declared so YAML still
	// parses and Validate can reject it; nothing may read it.
	ClusterEmit *bool `yaml:"cluster_emit" json:"cluster_emit" mapstructure:"cluster_emit"`
}

// ChannelConfig is the removed server.channels block (PR-KA-A4 / KD-K31: no
// compatibility period). It exists only so Validate can reject YAML that
// still spells it; nothing may read it.
type ChannelConfig struct {
	Default  ChannelPolicySpec   `yaml:"default" json:"default" mapstructure:"default"`
	Policies []ChannelPolicyRule `yaml:"policies" json:"policies" mapstructure:"policies"`
}

// AuthorizerConfig is the single authorization table (PR-KA-A4 §6): a default
// Effects spec plus rules in table order. A zero AuthorizerConfig is valid:
// no rules, default Effects = DefaultChannelPolicy() (subscribe/publish open,
// survey off).
type AuthorizerConfig struct {
	Default ChannelPolicySpec `yaml:"default" json:"default" mapstructure:"default"`
	Rules   []AuthorizerRule  `yaml:"rules" json:"rules" mapstructure:"rules"`
}

// AuthorizerRule is one row of the authorizer table. Pattern uses the
// subscription key language (`*` single segment, `**` only as the final
// segment; the literal prefix must not be empty). A nil allow list does not
// constrain the action; an empty list denies it; "*" allows any
// authenticated user. deny_all denies every action on the pattern.
type AuthorizerRule struct {
	Pattern           string   `yaml:"pattern" json:"pattern" mapstructure:"pattern"`
	DenyAll           bool     `yaml:"deny_all" json:"deny_all" mapstructure:"deny_all"`
	AllowSubscribe    []string `yaml:"allow_subscribe" json:"allow_subscribe" mapstructure:"allow_subscribe"`
	AllowPublish      []string `yaml:"allow_publish" json:"allow_publish" mapstructure:"allow_publish"`
	AllowSurvey       []string `yaml:"allow_survey" json:"allow_survey" mapstructure:"allow_survey"`
	ChannelPolicySpec `yaml:",inline" mapstructure:",squash"`
}

// ChannelPolicyRule is one policy rule: every rule whose pattern matches a
// channel contributes its Effects, in table order (later overrides earlier,
// PR-KA-A4 §5.5 — not first-match).
type ChannelPolicyRule struct {
	Pattern           string `yaml:"pattern" json:"pattern" mapstructure:"pattern"`
	ChannelPolicySpec `yaml:",inline" mapstructure:",squash"`
}

// ChannelPolicySpec is one policy overlay. Pointer fields mean "not
// overridden" (nil leaves the compiled default untouched). HistoryTTL and
// MaxSurveyTimeout use strings to distinguish "unset" from an explicit "0s".
type ChannelPolicySpec struct {
	History               *bool  `yaml:"history" json:"history" mapstructure:"history"`
	HistorySize           *int   `yaml:"history_size" json:"history_size" mapstructure:"history_size"`
	HistoryTTL            string `yaml:"history_ttl" json:"history_ttl" mapstructure:"history_ttl"`
	Presence              *bool  `yaml:"presence" json:"presence" mapstructure:"presence"`
	Recover               *bool  `yaml:"recover" json:"recover" mapstructure:"recover"`
	Survey                *bool  `yaml:"survey" json:"survey" mapstructure:"survey"`
	TransientOnly         *bool  `yaml:"transient_only" json:"transient_only" mapstructure:"transient_only"`
	RecoverLimit          *int   `yaml:"recover_limit" json:"recover_limit" mapstructure:"recover_limit"`
	MaxSurveySubscribers  *int   `yaml:"max_survey_subscribers" json:"max_survey_subscribers" mapstructure:"max_survey_subscribers"`
	MaxSurveyTimeout      string `yaml:"max_survey_timeout" json:"max_survey_timeout" mapstructure:"max_survey_timeout"`
	LegacyPresenceChannel *bool  `yaml:"legacy_presence_channel" json:"legacy_presence_channel" mapstructure:"legacy_presence_channel"`
	PresenceSnapshotLimit *int   `yaml:"presence_snapshot_limit" json:"presence_snapshot_limit" mapstructure:"presence_snapshot_limit"`
}

// ACLConfig is the removed server.acl block (PR-KA-A4 / KD-K31: no
// compatibility period). It exists only so Validate can reject YAML that
// still spells it; nothing may read it.
type ACLConfig struct {
	Rules []ACLRule `yaml:"rules" json:"rules" mapstructure:"rules"`
}

// ACLRule is the removed server.acl rule shape (moved to AuthorizerRule
// under server.authorizer).
type ACLRule struct {
	ChannelPattern string   `yaml:"channel_pattern" json:"channel_pattern" mapstructure:"channel_pattern"`
	AllowSubscribe []string `yaml:"allow_subscribe" json:"allow_subscribe" mapstructure:"allow_subscribe"`
	AllowPublish   []string `yaml:"allow_publish" json:"allow_publish" mapstructure:"allow_publish"`
	AllowSurvey    []string `yaml:"allow_survey" json:"allow_survey" mapstructure:"allow_survey"`
	DenyAll        bool     `yaml:"deny_all" json:"deny_all" mapstructure:"deny_all"`
}

type Limits struct {
	MaxConnectionsPerUser     int `yaml:"max_connections_per_user" json:"max_connections_per_user" mapstructure:"max_connections_per_user"`             // 0 = unlimited
	MaxSubscriptionsPerClient int `yaml:"max_subscriptions_per_client" json:"max_subscriptions_per_client" mapstructure:"max_subscriptions_per_client"` // 0 = unlimited
	MaxPublishesPerSecond     int `yaml:"max_publishes_per_second" json:"max_publishes_per_second" mapstructure:"max_publishes_per_second"`             // 0 = unlimited
	MaxMessageSize            int `yaml:"max_message_size" json:"max_message_size" mapstructure:"max_message_size"`                                     // bytes, 0 = default (64KB), applies uniformly to WebSocket, gRPC, and QUIC transports
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
	// Capabilities is the admin capability set (PR-KA-A4 §7). Omitted (nil) →
	// DefaultAdminCapabilities (every closed bit except pattern.global);
	// explicitly empty ([]) → zero bits, locking the admin data plane.
	// Unknown names are a Validate error.
	Capabilities []string `yaml:"capabilities" json:"capabilities" mapstructure:"capabilities"`
}

type Heartbeat struct {
	IdleTimeout  string `yaml:"idle_timeout" json:"idle_timeout" mapstructure:"idle_timeout"`    // default: "300s"
	PingInterval string `yaml:"ping_interval" json:"ping_interval" mapstructure:"ping_interval"` // default: "0s" (no server-initiated ping)
	PingTimeout  string `yaml:"ping_timeout" json:"ping_timeout" mapstructure:"ping_timeout"`    // default: ping_interval
}

type Transport struct {
	WebSocket WebSocketTransport `yaml:"websocket" json:"websocket" mapstructure:"websocket"`
	GRPC      GRPCTransport      `yaml:"grpc" json:"grpc" mapstructure:"grpc"`
	QUIC      QUICTransport      `yaml:"quic" json:"quic" mapstructure:"quic"`
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file" json:"cert_file" mapstructure:"cert_file"`
	KeyFile  string `yaml:"key_file" json:"key_file" mapstructure:"key_file"`
}

type WebSocketTransport struct {
	Addr            string    `yaml:"addr" json:"addr" mapstructure:"addr"`
	Path            string    `yaml:"path" json:"path" mapstructure:"path"`
	ReadTimeout     string    `yaml:"read_timeout" json:"read_timeout" mapstructure:"read_timeout"`                // duration string
	WriteTimeout    string    `yaml:"write_timeout" json:"write_timeout" mapstructure:"write_timeout"`             // duration string, e.g. "10s"
	AllowAllOrigins bool      `yaml:"allow_all_origins" json:"allow_all_origins" mapstructure:"allow_all_origins"` // Allow any origin (development only)
	AllowedOrigins  []string  `yaml:"allowed_origins" json:"allowed_origins" mapstructure:"allowed_origins"`       // Whitelist of allowed origins
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

// QUICTransport configures the optional QUIC client listener. An empty Addr
// disables the listener. QUIC always requires TLS 1.3: provide cert/key or
// set Insecure to generate an ephemeral self-signed certificate (dev only).
type QUICTransport struct {
	Addr         string    `yaml:"addr" json:"addr" mapstructure:"addr"`
	WriteTimeout string    `yaml:"write_timeout" json:"write_timeout" mapstructure:"write_timeout"`
	ReadTimeout  string    `yaml:"read_timeout" json:"read_timeout" mapstructure:"read_timeout"`
	Insecure     bool      `yaml:"insecure" json:"insecure" mapstructure:"insecure"`
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
	// The startup wiring always constructs the WebSocket listener
	// (newWebSocketServer) and the client gRPC listener (prepareGRPCServers).
	// QUIC is optional: an empty transport.quic.addr leaves it disabled.
	// Validate the required addresses here so a configuration that would
	// mis-bind or panic at startup is rejected up front.
	if c.Transport.WebSocket.Addr == "" {
		return fmt.Errorf("transport.websocket.addr is required")
	}
	if c.Transport.WebSocket.Path == "" {
		return fmt.Errorf("transport.websocket.path is required when websocket transport is enabled")
	}
	if c.Transport.GRPC.Addr == "" {
		return fmt.Errorf("transport.grpc.addr is required")
	}
	if c.Transport.QUIC.Addr != "" {
		hasCert := c.Transport.QUIC.TLS.CertFile != "" || c.Transport.QUIC.TLS.KeyFile != ""
		if !c.Transport.QUIC.Insecure && !hasCert {
			return fmt.Errorf("transport.quic requires tls cert_file and key_file, or set insecure: true to use a self-signed certificate")
		}
	}

	// Validate duration fields.
	for _, entry := range []struct {
		name  string
		value string
	}{
		{"server.heartbeat.idle_timeout", c.Server.Heartbeat.IdleTimeout},
		{"server.heartbeat.ping_interval", c.Server.Heartbeat.PingInterval},
		{"server.heartbeat.ping_timeout", c.Server.Heartbeat.PingTimeout},
		{"server.rpc_timeout", c.Server.RPCTimeout},
		{"transport.websocket.read_timeout", c.Transport.WebSocket.ReadTimeout},
		{"transport.websocket.write_timeout", c.Transport.WebSocket.WriteTimeout},
		{"transport.grpc.write_timeout", c.Transport.GRPC.WriteTimeout},
		{"transport.quic.write_timeout", c.Transport.QUIC.WriteTimeout},
		{"transport.quic.read_timeout", c.Transport.QUIC.ReadTimeout},
	} {
		if entry.value != "" {
			if _, err := time.ParseDuration(entry.value); err != nil {
				return fmt.Errorf("invalid duration for %s: %w", entry.name, err)
			}
		}
	}

	// Heartbeat durations must be second-scale when enabled: a non-zero
	// idle_timeout / ping_interval / ping_timeout below 1s is rejected.
	// "0s" keeps its existing meaning (disable that probe).
	hb := c.Server.Heartbeat
	parsed := map[string]time.Duration{}
	for _, entry := range []struct {
		name  string
		value string
	}{
		{"server.heartbeat.idle_timeout", hb.IdleTimeout},
		{"server.heartbeat.ping_interval", hb.PingInterval},
		{"server.heartbeat.ping_timeout", hb.PingTimeout},
	} {
		if entry.value == "" {
			continue
		}
		d, err := time.ParseDuration(entry.value)
		if err != nil {
			return fmt.Errorf("invalid duration for %s: %w", entry.name, err)
		}
		parsed[entry.name] = d
		if d != 0 && d < time.Second {
			return fmt.Errorf("%s must be at least 1s (or 0s to disable), got %q", entry.name, entry.value)
		}
	}

	// ping_timeout=0s is only meaningful when server pings are disabled:
	// enabling ping_interval with an explicit zero timeout would arm a
	// deadline that fires instantly. An empty ping_timeout falls back to
	// ping_interval at NewNode time, so it is not an error here.
	if interval, ok := parsed["server.heartbeat.ping_interval"]; ok && interval > 0 {
		// An empty ping_timeout falls back to ping_interval at NewNode time.
		effectiveTimeout := interval
		if timeout, ok := parsed["server.heartbeat.ping_timeout"]; ok {
			effectiveTimeout = timeout
		}
		if timeout, ok := parsed["server.heartbeat.ping_timeout"]; ok && timeout == 0 {
			return fmt.Errorf("server.heartbeat.ping_timeout: 0s is not allowed when server.heartbeat.ping_interval is enabled")
		}
		if idle, ok := parsed["server.heartbeat.idle_timeout"]; ok && idle > 0 && idle < interval+effectiveTimeout {
			log.WarnContext(context.Background(),
				"server.heartbeat.idle_timeout is shorter than ping_interval+ping_timeout; unresponded pings will disconnect clients before the idle check",
				"idle_timeout", hb.IdleTimeout,
				"ping_interval", hb.PingInterval,
				"ping_timeout", hb.PingTimeout)
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
		{"transport.quic.tls", c.Transport.QUIC.TLS},
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

	// An enabled cluster must carry an HMAC key for the command bus: writing
	// to Redis must not be enough to inject cluster commands (KD-K31).
	if c.Cluster.Enabled {
		key, keyFile := c.Cluster.HMACKey, c.Cluster.HMACKeyFile
		switch {
		case key == "" && keyFile == "":
			return fmt.Errorf("cluster.hmac_key is required when cluster is enabled (or set cluster.hmac_key_file)")
		case key != "" && keyFile != "":
			return fmt.Errorf("only one of hmac_key or hmac_key_file may be set")
		case key != "" && len([]byte(key)) < minClusterHMACKeyBytes:
			return fmt.Errorf("cluster.hmac_key must be at least 32 bytes")
		}
	}

	// Validate the admin capability names: the set is closed, unknown names
	// are rejected up front (PR-KA-A4 §7).
	for i, name := range c.Server.GRPCAdmin.Capabilities {
		if _, ok := CapabilityNames[name]; !ok {
			return fmt.Errorf("server.grpc_admin.capabilities[%d]: unknown capability %q", i, name)
		}
	}

	// The removed authorization blocks must not reappear (KD-K31: no
	// compatibility period). The fields stay declared so YAML still parses
	// and this explicit check can reject them.
	if len(c.Server.ACL.Rules) > 0 {
		return fmt.Errorf("server.acl is removed; move rules to server.authorizer.rules")
	}
	if len(c.Server.Channels.Policies) > 0 || channelPolicySpecSet(c.Server.Channels.Default) {
		return fmt.Errorf("server.channels is removed; move policy to server.authorizer")
	}
	if c.Server.Presence.ClusterEmit != nil {
		return fmt.Errorf("server.presence.cluster_emit is removed; occupancy always crosses nodes over the live bus (exact channel + compiled Interest)")
	}

	// Validate the authorizer table: the default Effects spec and every rule.
	// Rule patterns must be part of the subscription key language (§5.1) and
	// every inline Effects spec follows the channel policy constraints.
	if err := validateChannelPolicySpec("server.authorizer.default", c.Server.Authorizer.Default); err != nil {
		return err
	}
	for i, rule := range c.Server.Authorizer.Rules {
		prefix := fmt.Sprintf("server.authorizer.rules[%d]", i)
		if rule.Pattern == "" {
			return fmt.Errorf("%s.pattern is required", prefix)
		}
		if err := validateAuthorizerPattern(rule.Pattern); err != nil {
			return fmt.Errorf("%s.pattern %q: %w", prefix, rule.Pattern, err)
		}
		if err := validateChannelPolicySpec(prefix, rule.ChannelPolicySpec); err != nil {
			return err
		}
	}

	return nil
}

// validateAuthorizerPattern checks that a rule pattern is part of the
// subscription key language: a valid topic whose wildcard (if any) is a final
// single "*" or "**" preceded by a non-empty literal prefix. "a.**.b",
// "*.room", "im.*.tick" and bare "*" / "**" are all rejected.
func validateAuthorizerPattern(pattern string) error {
	if err := topics.ValidateTopic(pattern); err != nil {
		return err
	}
	if !strings.Contains(pattern, "*") {
		return nil
	}
	segments := strings.Split(pattern, ".")
	last := segments[len(segments)-1]
	if last != "*" && last != "**" {
		return fmt.Errorf("wildcard must be the final segment (last segment must be * or **)")
	}
	for _, seg := range segments[:len(segments)-1] {
		if strings.Contains(seg, "*") {
			return fmt.Errorf("only the final segment may be a wildcard")
		}
	}
	if len(segments) == 1 {
		return fmt.Errorf("empty literal prefix is not allowed")
	}
	return nil
}

// channelPolicySpecSet reports whether the spec carries any explicit value
// (used to detect a non-empty server.channels.default).
func channelPolicySpecSet(spec ChannelPolicySpec) bool {
	return spec.History != nil ||
		spec.HistorySize != nil ||
		spec.HistoryTTL != "" ||
		spec.Presence != nil ||
		spec.Recover != nil ||
		spec.Survey != nil ||
		spec.TransientOnly != nil ||
		spec.RecoverLimit != nil ||
		spec.MaxSurveySubscribers != nil ||
		spec.MaxSurveyTimeout != "" ||
		spec.LegacyPresenceChannel != nil ||
		spec.PresenceSnapshotLimit != nil
}

// validateChannelPolicySpec validates the scalar constraints shared by the
// default spec and each policy rule.
func validateChannelPolicySpec(prefix string, spec ChannelPolicySpec) error {
	if spec.HistorySize != nil && *spec.HistorySize < 0 {
		return fmt.Errorf("%s.history_size must be >= 0", prefix)
	}
	if spec.HistoryTTL != "" {
		if _, err := time.ParseDuration(spec.HistoryTTL); err != nil {
			return fmt.Errorf("invalid duration for %s.history_ttl: %w", prefix, err)
		}
	}
	if spec.MaxSurveyTimeout != "" {
		if _, err := time.ParseDuration(spec.MaxSurveyTimeout); err != nil {
			return fmt.Errorf("invalid duration for %s.max_survey_timeout: %w", prefix, err)
		}
	}
	return nil
}
