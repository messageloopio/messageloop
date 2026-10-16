package main

import (
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/messageloopio/messageloop/internal/config"
	"github.com/messageloopio/messageloop/internal/runtime"
)

// TestRepositoryConfigsValidateAndPrebind parses every YAML config shipped in
// the repository and asserts that (a) Validate passes and (b) both gRPC
// listeners (client + admin) can be pre-bound on ephemeral ports. This guards
// against a regression where the default config cannot start the server
// (e.g. a missing server.api section).
func TestRepositoryConfigsValidateAndPrebind(t *testing.T) {
	files := []string{
		"../../config.yaml",
		"../../config-example.yaml",
		"../../config-node1.yaml",
		"../../config-node2.yaml",
		"../../configs/test.yaml",
		"../../configs/docker.yaml",
	}
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if os.IsNotExist(err) {
				// Local-only configs (e.g. a developer's own config.yaml or
				// cluster node files) are not shipped in the repository; the
				// guard applies to tracked configs only.
				t.Skipf("%s not present in this checkout (untracked local config)", file)
			}
			require.NoError(t, err)

			cfg := &config.Config{}
			require.NoError(t, yaml.Unmarshal(data, cfg), "config must parse")
			require.NoError(t, cfg.Validate(), "config must pass Validate")

			// Rewrite fixed addrs to ephemeral ports so pre-binding cannot
			// collide with a local service, then verify both gRPC listeners
			// can be prepared exactly like startup does.
			cfg.Transport.GRPC.Addr = "127.0.0.1:0"
			cfg.Server.API.Addr = "127.0.0.1:0"

			servers, err := prepareGRPCServers(cfg, runtime.NewNode(nil), nil, nil)
			require.NoError(t, err, "gRPC servers must pre-bind with the config's addresses")
			servers.Close()
		})
	}
}

func TestNewQUICServer_DisabledWhenAddrEmpty(t *testing.T) {
	cfg := &config.Config{}
	server, err := newQUICServer(cfg, runtime.NewNode(nil))
	require.NoError(t, err)
	require.Nil(t, server)
}

func TestNewQUICServer_PrebindsInsecure(t *testing.T) {
	cfg := &config.Config{
		Transport: config.Transport{
			QUIC: config.QUICTransport{Addr: "127.0.0.1:0", Insecure: true},
		},
	}
	server, err := newQUICServer(cfg, runtime.NewNode(nil))
	require.NoError(t, err)
	require.NotNil(t, server)
	require.NotEmpty(t, server.Addr())
	require.NoError(t, server.Close())
}

// TestNewRegistryResolver pins the registry assembly contract (v1.1 PR-A,
// design §4.7): disabled yields nils (no redis client is built), enabled
// yields the node-level Resolver plus the backend and the independent client,
// and all three expose Close. Nothing dials Redis here: redis.NewClient and
// registry.NewResolver are lazy, so no server is needed.
func TestNewRegistryResolver(t *testing.T) {
	disabled := &config.Config{}
	resolver, disc, client, err := newRegistryResolver(disabled)
	require.NoError(t, err)
	require.Nil(t, resolver)
	require.Nil(t, disc)
	require.Nil(t, client)

	enabled := &config.Config{
		Registry: config.RegistryConfig{
			Enabled: true,
			Backend: "redis",
			Redis: config.RegistryRedisConfig{
				Addr:      "127.0.0.1:6379",
				KeyPrefix: config.DefaultRegistryKeyPrefix,
			},
		},
	}
	resolver, disc, client, err = newRegistryResolver(enabled)
	require.NoError(t, err)
	require.NotNil(t, resolver)
	require.NotNil(t, disc)
	require.NotNil(t, client)
	require.NoError(t, resolver.Close())
	require.NoError(t, disc.Close())
	require.NoError(t, client.Close())
}

func TestBuildWebSocketOptions_ReadTimeoutApplied(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Transport: config.Transport{
			WebSocket: config.WebSocketTransport{Addr: ":9080", Path: "/ws", ReadTimeout: "75s"},
		},
	}
	opts := buildWebSocketOptions(cfg, logger)
	require.Equal(t, 75*time.Second, opts.ReadTimeout)
}

func TestBuildWebSocketOptions_ReadTimeoutDisabledWithZero(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Transport: config.Transport{
			WebSocket: config.WebSocketTransport{Addr: ":9080", Path: "/ws", ReadTimeout: "0s"},
		},
	}
	opts := buildWebSocketOptions(cfg, logger)
	require.Equal(t, time.Duration(0), opts.ReadTimeout)
}

func TestBuildWebSocketOptions_EmptyReadTimeoutKeepsZero(t *testing.T) {
	// Unconfigured read_timeout must remain zero so the handler falls back
	// to its heartbeat-based default deadline.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		Transport: config.Transport{
			WebSocket: config.WebSocketTransport{Addr: ":9080", Path: "/ws"},
		},
	}
	opts := buildWebSocketOptions(cfg, logger)
	require.Equal(t, time.Duration(0), opts.ReadTimeout)
}
