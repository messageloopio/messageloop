package main

import (
	"fmt"
	"strings"

	"github.com/lynx-go/lynx"
	"github.com/spf13/pflag"
)

// envPrefix is the environment variable prefix for config overrides:
// MESSAGELOOP_SERVER_NAMESPACE overrides server.namespace, and so on.
const envPrefix = "MESSAGELOOP"

// envConfigKeys lists the deployment-relevant config paths that are
// registered as environment-variable bindings. Keys on this list can be
// overridden via MESSAGELOOP_-prefixed variables even when the loaded config
// file omits them — viper's Unmarshal only decodes keys the config source
// knows, so a binding (not AutomaticEnv alone) is what makes a key visible.
// Complex nested structures (server.authorizer rules, proxy backends) are
// deliberately absent: they stay config-file-only; mount or bake a custom
// YAML for those.
var envConfigKeys = []string{
	// server
	"server.http.addr",
	"server.http.auth_token",
	"server.grpc_admin.addr",
	"server.grpc_admin.auth_tokens",
	"server.grpc_admin.admin_auth_cache_ttl",
	"server.grpc_admin.allow_insecure",
	"server.grpc_admin.tls.cert_file",
	"server.grpc_admin.tls.key_file",
	"server.heartbeat.idle_timeout",
	"server.heartbeat.ping_interval",
	"server.heartbeat.ping_timeout",
	"server.rpc_timeout",
	"server.require_auth",
	"server.namespace",
	"server.limits.max_connections_per_user",
	"server.limits.max_subscriptions_per_client",
	"server.limits.max_publishes_per_second",
	"server.limits.max_message_size",
	// transport: websocket
	"transport.websocket.addr",
	"transport.websocket.path",
	"transport.websocket.allow_all_origins",
	"transport.websocket.allowed_origins",
	"transport.websocket.compression",
	"transport.websocket.read_timeout",
	"transport.websocket.write_timeout",
	"transport.websocket.tls.cert_file",
	"transport.websocket.tls.key_file",
	// transport: grpc
	"transport.grpc.addr",
	"transport.grpc.write_timeout",
	"transport.grpc.tls.cert_file",
	"transport.grpc.tls.key_file",
	// transport: quic
	"transport.quic.addr",
	"transport.quic.insecure",
	"transport.quic.read_timeout",
	"transport.quic.write_timeout",
	"transport.quic.tls.cert_file",
	"transport.quic.tls.key_file",
	// transport: kcp
	"transport.kcp.addr",
	"transport.kcp.insecure",
	"transport.kcp.data_shards",
	"transport.kcp.parity_shards",
	"transport.kcp.read_timeout",
	"transport.kcp.write_timeout",
	"transport.kcp.tls.cert_file",
	"transport.kcp.tls.key_file",
	// broker
	"broker.type",
	"broker.redis.addr",
	"broker.redis.password",
	"broker.redis.db",
	"broker.redis.pool_size",
	"broker.redis.min_idle_conns",
	"broker.redis.max_retries",
	"broker.redis.dial_timeout",
	"broker.redis.read_timeout",
	"broker.redis.write_timeout",
	"broker.redis.stream_max_length",
	"broker.redis.stream_approximate",
	"broker.redis.history_ttl",
	// cluster
	"cluster.enabled",
	"cluster.node_id",
	"cluster.backend",
	"cluster.hmac_key",
	"cluster.hmac_key_file",
}

// bindConfigWithEnv extends lynx.DefaultBindConfigFunc with environment
// variable overrides. It is installed via lynx.WithBindConfigFunc so
// container deployments (Dokploy, plain Docker, Kubernetes) can reconfigure
// the baked-in config file without rebuilding the image.
//
// The mapping is prefix + dotted path with dots replaced by underscores:
// broker.redis.addr -> MESSAGELOOP_BROKER_REDIS_ADDR (viper applies the
// replacer when it looks up the variable). Besides the registered keys,
// AutomaticEnv also overrides any scalar key present in the loaded config
// file; string slices take comma-separated values. Environment values win
// over the file.
func bindConfigWithEnv(f *pflag.FlagSet, c lynx.ConfigSource) error {
	if err := lynx.DefaultBindConfigFunc(f, c); err != nil {
		return err
	}
	c.SetEnvPrefix(envPrefix)
	c.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	c.AutomaticEnv()
	for _, key := range envConfigKeys {
		if err := c.BindEnv(key); err != nil {
			return fmt.Errorf("bind env override for %s: %w", key, err)
		}
	}
	return nil
}
