package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lynx-go/lynx"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"
)

// newTestConfigSource builds a config source the same way lynx does in
// initConfigure and runs bindConfigWithEnv against a parsed --config flag.
// The runner calls ReadInConfig on its internal viper instance after
// BindConfigFunc; the ConfigSource interface does not expose it, so the
// test reads the file through the same underlying viper.
func newTestConfigSource(t *testing.T, yaml string) lynx.ConfigSource {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yaml), 0o600))

	f := pflag.NewFlagSet("test", pflag.ContinueOnError)
	f.String("config", "", "config file path")
	require.NoError(t, f.Parse([]string{"--config", path}))

	v := viper.New()
	require.NoError(t, bindConfigWithEnv(f, lynx.NewViperConfig(v)))
	require.NoError(t, v.ReadInConfig())
	return lynx.NewViperConfig(v)
}

func TestBindConfigWithEnvOverridesFile(t *testing.T) {
	t.Setenv("MESSAGELOOP_SERVER_NAMESPACE", "from-env")
	t.Setenv("MESSAGELOOP_SERVER_REQUIRE_AUTH", "true")
	t.Setenv("MESSAGELOOP_BROKER_REDIS_STREAM_MAX_LENGTH", "5000")
	t.Setenv("MESSAGELOOP_TRANSPORT_WEBSOCKET_ADDR", "0.0.0.0:1234")

	source := newTestConfigSource(t, `
server:
  namespace: from-file
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
broker:
  type: memory
`)
	var cfg config.Config
	require.NoError(t, source.Unmarshal(&cfg))
	assert.Equal(t, "from-env", cfg.Server.Namespace)
	assert.True(t, cfg.Server.RequireAuth)
	assert.Equal(t, int64(5000), cfg.Broker.Redis.StreamMaxLength)
	assert.Equal(t, "0.0.0.0:1234", cfg.Transport.WebSocket.Addr)
	// The untouched file value survives.
	assert.Equal(t, "/ws", cfg.Transport.WebSocket.Path)
	assert.Equal(t, "memory", cfg.Broker.Type)
}

func TestBindConfigWithEnvFileFallback(t *testing.T) {
	source := newTestConfigSource(t, `
server:
  namespace: from-file
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
broker:
  type: memory
`)
	var cfg config.Config
	require.NoError(t, source.Unmarshal(&cfg))
	assert.Equal(t, "from-file", cfg.Server.Namespace)
	assert.Equal(t, ":9080", cfg.Transport.WebSocket.Addr)
	assert.False(t, cfg.Server.RequireAuth)
}

func TestBindConfigWithEnvOnlyKey(t *testing.T) {
	// broker.redis.addr is absent from the file; the BindEnv table is what
	// makes the environment value visible to Unmarshal.
	t.Setenv("MESSAGELOOP_BROKER_REDIS_ADDR", "redis:6379")
	t.Setenv("MESSAGELOOP_BROKER_TYPE", "redis")
	t.Setenv("MESSAGELOOP_BROKER_REDIS_STREAM_APPROXIMATE", "true")

	source := newTestConfigSource(t, `
server:
  namespace: from-file
  grpc_admin:
    addr: "127.0.0.1:9091"
    allow_insecure: true
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
  grpc:
    addr: ":9090"
broker:
  type: memory
`)
	var cfg config.Config
	require.NoError(t, source.Unmarshal(&cfg))
	assert.Equal(t, "redis", cfg.Broker.Type)
	assert.Equal(t, "redis:6379", cfg.Broker.Redis.Addr)
	assert.True(t, cfg.Broker.Redis.StreamApproximate)
	require.NoError(t, cfg.Validate())
}

func TestBindConfigWithEnvStringSlice(t *testing.T) {
	// Registered keys decode comma-separated environment values into string
	// slices (viper's StringToSliceHookFunc).
	t.Setenv("MESSAGELOOP_TRANSPORT_WEBSOCKET_ALLOWED_ORIGINS", "https://a.example.com,https://b.example.com")

	source := newTestConfigSource(t, `
server:
  namespace: from-file
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
broker:
  type: memory
`)
	var cfg config.Config
	require.NoError(t, source.Unmarshal(&cfg))
	assert.Equal(t,
		[]string{"https://a.example.com", "https://b.example.com"},
		cfg.Transport.WebSocket.AllowedOrigins)
}

func TestBindConfigWithEnvAdminAuthTokens(t *testing.T) {
	// The admin token list overrides via the plural env key (design D28):
	// comma-separated values decode into server.grpc_admin.auth_tokens, and
	// the cache TTL binding parses as a plain string.
	t.Setenv("MESSAGELOOP_SERVER_GRPC_ADMIN_AUTH_TOKENS", "token-one-0123456789abcdef,token-two-0123456789abcdef")
	t.Setenv("MESSAGELOOP_SERVER_GRPC_ADMIN_ADMIN_AUTH_CACHE_TTL", "45s")

	source := newTestConfigSource(t, `
server:
  namespace: from-file
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
broker:
  type: memory
`)
	var cfg config.Config
	require.NoError(t, source.Unmarshal(&cfg))
	assert.Equal(t,
		[]string{"token-one-0123456789abcdef", "token-two-0123456789abcdef"},
		cfg.Server.GRPCAdmin.AuthTokens)
	assert.Equal(t, "45s", cfg.Server.GRPCAdmin.AdminAuthCacheTTL)
}

func TestBindConfigWithEnvIgnoresUnregisteredKey(t *testing.T) {
	// server.authorizer is not in the BindEnv table; an env var cannot
	// conjure an authorization rule that the config file does not define.
	t.Setenv("MESSAGELOOP_SERVER_AUTHORIZER_DEFAULT_HISTORY", "false")

	source := newTestConfigSource(t, `
server:
  namespace: from-file
transport:
  websocket:
    addr: ":9080"
    path: "/ws"
broker:
  type: memory
`)
	var cfg config.Config
	require.NoError(t, source.Unmarshal(&cfg))
	assert.Nil(t, cfg.Server.Authorizer.Default.History)
}
