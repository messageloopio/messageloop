package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/quicstream"
	"github.com/messageloopio/messageloop/pkg/redisbroker"
	"github.com/messageloopio/messageloop/pkg/websocket"
	proxyproxy "github.com/messageloopio/messageloop/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
)

var (
	version string
)

func main() {
	runner := lynx.NewRunner(func(app lynx.App) error {
		app.SetLogger(zap.MustNewLogger(app))
		cfg := &config.Config{}
		if err := app.Config().Unmarshal(cfg); err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}

		node := messageloop.NewNode(&cfg.Server)
		reg := prometheus.NewRegistry()
		reg.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
		metricsRegisterer := prometheus.Registerer(reg)
		if cfg.Cluster.Enabled && cfg.Cluster.NodeID != "" {
			// Tag every messageloop metric with the configured node_id so
			// multi-node deployments can be aggregated per node.
			metricsRegisterer = prometheus.WrapRegistererWith(prometheus.Labels{"node_id": cfg.Cluster.NodeID}, reg)
		}
		metrics := messageloop.NewMetrics(metricsRegisterer)
		node.SetMetrics(metrics)

		cluster, err := setupCluster(cfg, node, metrics)
		if err != nil {
			return err
		}
		node.SetCluster(cluster)
		if cluster.Enabled() && cluster.Backend() == "redis" {
			// Presence state lives in Redis; wire the store explicitly at
			// assembly time so the setupCluster side effect is visible here.
			node.SetPresenceStore(redisbroker.NewPresenceStore(cfg.Broker.Redis))
		}

		broker, err := newBroker(cfg)
		if err != nil {
			return err
		}
		node.SetBroker(broker)

		// In cluster mode the health endpoint probes Redis connectivity;
		// wire the broker's ping as that probe when the broker supports it.
		if pinger, ok := broker.(interface{ Ping(context.Context) error }); ok {
			node.SetHealthCheck(pinger.Ping)
		}

		if err = setupProxy(cfg, node); err != nil {
			return err
		}

		grpcServers, err := prepareGRPCServers(cfg, node)
		if err != nil {
			return err
		}

		wsServer := newWebSocketServer(cfg, node, app.Logger())
		adminServer := newAdminServer(cfg, node, reg)
		quicServer, err := newQUICServer(cfg, node)
		if err != nil {
			return err
		}

		app.OnStart(node.Run)
		components := []lynx.Service{wsServer, adminServer}
		components = append(components, grpcServers.Components()...)
		if quicServer != nil {
			components = append(components, quicServer)
		}
		app.Register(components...)
		app.OnStop(func(ctx context.Context) error {
			// Drain all client connections before shutting down.
			node.Shutdown()
			// Release the pre-bound gRPC / QUIC listeners as a defensive measure.
			grpcServers.Close()
			if quicServer != nil {
				_ = quicServer.Close()
			}
			return nil
		})

		return nil
	},
		lynx.WithName("MessageLoop"),
		lynx.WithVersion(version),
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config", "./config.yaml", "config file path")
			f.String("log-level", "info", "log level, default info")
		}),
		lynx.WithBindConfigFunc(lynx.DefaultBindConfigFunc),
		lynx.WithShutdownTimeout(30*time.Second),
	)

	runner.Run()
}

// normalizeClusterOptions validates and normalizes cluster options from the
// config, mirroring messageloop.ClusterOptions.normalize() so the
// control-plane dependencies can be wired before the single NewCluster
// construction (the normalization result is passed back into NewCluster).
func normalizeClusterOptions(cfg *config.Config) (messageloop.ClusterOptions, error) {
	if !cfg.Cluster.Enabled {
		return messageloop.ClusterOptions{}, nil
	}
	nodeID := strings.TrimSpace(cfg.Cluster.NodeID)
	if nodeID == "" {
		return messageloop.ClusterOptions{}, errors.New("cluster node_id is required when cluster is enabled")
	}
	backend := strings.TrimSpace(cfg.Cluster.Backend)
	if backend == "" {
		backend = "redis"
	}
	return messageloop.ClusterOptions{
		Enabled:       true,
		NodeID:        nodeID,
		Backend:       backend,
		IncarnationID: uuid.NewString(),
	}, nil
}

// setupCluster creates and wires the cluster based on the provided config.
// For Redis-backed clusters it also configures the session directory, command bus,
// query store, node lease manager, projection repairer, and presence store.
func setupCluster(cfg *config.Config, node *messageloop.Node, metrics *messageloop.Metrics) (*messageloop.Cluster, error) {
	opts, err := normalizeClusterOptions(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid cluster config: %w", err)
	}

	if !opts.Enabled || opts.Backend != "redis" {
		cluster, err := messageloop.NewCluster(opts, messageloop.ClusterDependencies{})
		if err != nil {
			return nil, fmt.Errorf("invalid cluster config: %w", err)
		}
		return cluster, nil
	}

	deps := messageloop.ClusterDependencies{}
	deps.SessionDirectory = redisbroker.NewSessionDirectory(cfg.Broker.Redis)

	// The command-bus HMAC key comes only from node configuration; a
	// misconfigured or unreadable key must refuse startup rather than run an
	// unprotected bus.
	hmacKey, err := cfg.Cluster.ResolveHMACKey()
	if err != nil {
		return nil, fmt.Errorf("invalid cluster config: %w", err)
	}
	deps.CommandBus = redisbroker.NewClusterCommandBus(cfg.Broker.Redis, opts.NodeID, opts.IncarnationID, hmacKey)
	deps.QueryStore = redisbroker.NewClusterQueryStore(cfg.Broker.Redis, opts.NodeID, opts.IncarnationID)
	deps.NodeLeaseManager = messageloop.NewClusterNodeLeaseManager(
		deps.SessionDirectory,
		messageloop.ClusterNodeLeaseManagerConfig{
			NodeID:        opts.NodeID,
			IncarnationID: opts.IncarnationID,
		},
	)
	// One repairer drives projection republish, dead-projection reaping,
	// user-index rebuild, and membership OnLeave (PR-KA-B4).
	deps.Repairer = messageloop.NewClusterRepairer(node, deps.SessionDirectory, deps.QueryStore, messageloop.ClusterRepairerConfig{})
	deps.CommandBus.SetHandler(node.ClusterCommandHandler())
	if metricsAware, ok := deps.CommandBus.(interface{ SetMetrics(*messageloop.Metrics) }); ok {
		metricsAware.SetMetrics(metrics)
	}

	cluster, err := messageloop.NewCluster(opts, deps)
	if err != nil {
		return nil, fmt.Errorf("wire cluster: %w", err)
	}
	return cluster, nil
}

// newBroker creates a Broker instance based on the broker type in config.
func newBroker(cfg *config.Config) (messageloop.Broker, error) {
	brokerType := cfg.Broker.Type
	if brokerType == "" {
		brokerType = "memory" // default
	}
	switch brokerType {
	case "redis":
		return redisbroker.New(cfg.Broker.Redis), nil
	case "memory":
		return messageloop.NewMemoryBroker(messageloop.MemoryBrokerOptions{}), nil
	default:
		return nil, fmt.Errorf("unknown broker type: %s", brokerType)
	}
}

// setupProxy configures and registers backend proxy routes on node from the given config.
func setupProxy(cfg *config.Config, node *messageloop.Node) error {
	if len(cfg.Proxy) == 0 {
		return nil
	}
	proxyConfigs := make([]*proxyproxy.ProxyConfig, 0, len(cfg.Proxy))
	for _, p := range cfg.Proxy {
		pc, err := p.ToProxyConfig()
		if err != nil {
			return fmt.Errorf("invalid proxy config %s: %w", p.Name, err)
		}
		proxyConfigs = append(proxyConfigs, pc)
	}
	if err := node.SetupProxy(proxyConfigs); err != nil {
		return fmt.Errorf("failed to setup proxies: %w", err)
	}
	return nil
}

// buildWebSocketOptions translates the WebSocket transport config into
// websocket.Options.
func buildWebSocketOptions(cfg *config.Config, logger *slog.Logger) websocket.Options {
	wsOpts := websocket.Options{
		Addr:        cfg.Transport.WebSocket.Addr,
		WsPath:      cfg.Transport.WebSocket.Path,
		TLSCertFile: cfg.Transport.WebSocket.TLS.CertFile,
		TLSKeyFile:  cfg.Transport.WebSocket.TLS.KeyFile,
		Compression: cfg.Transport.WebSocket.Compression,
	}
	if cfg.Transport.WebSocket.WriteTimeout == "" {
		// Unconfigured: keep the default 10s write timeout so slow consumers
		// cannot block broadcasts indefinitely.
		wsOpts.WriteTimeout = websocket.DefaultWSWriteTimeout
	} else if d, err := time.ParseDuration(cfg.Transport.WebSocket.WriteTimeout); err == nil {
		// Explicitly configured (including "0" to disable the timeout).
		wsOpts.WriteTimeout = d
	}
	if cfg.Transport.WebSocket.ReadTimeout != "" {
		// Explicitly configured read deadline; otherwise the handler falls
		// back to its heartbeat-based default. Invalid durations are already
		// rejected by config.Validate, so parse errors are ignored here.
		if d, err := time.ParseDuration(cfg.Transport.WebSocket.ReadTimeout); err == nil {
			wsOpts.ReadTimeout = d
		}
	}
	if cfg.Transport.WebSocket.AllowAllOrigins || cfg.Transport.WebSocket.CheckOrigin { //nolint:staticcheck // backward compat
		logger.Info("setting websocket CheckOrigin to allow all origins")
		wsOpts.CheckOrigin = func(r *http.Request) bool { return true }
	} else if len(cfg.Transport.WebSocket.AllowedOrigins) > 0 {
		allowed := make(map[string]bool, len(cfg.Transport.WebSocket.AllowedOrigins))
		for _, o := range cfg.Transport.WebSocket.AllowedOrigins {
			allowed[o] = true
		}
		wsOpts.CheckOrigin = func(r *http.Request) bool {
			return allowed[r.Header.Get("Origin")]
		}
	}
	return wsOpts
}

// newWebSocketServer builds the WebSocket server component from config.
func newWebSocketServer(cfg *config.Config, node *messageloop.Node, logger *slog.Logger) *websocket.Server {
	return websocket.NewServer(buildWebSocketOptions(cfg, logger), node)
}

// newQUICServer builds the optional QUIC client listener. A nil server is
// returned (without error) when transport.quic.addr is empty.
func newQUICServer(cfg *config.Config, node *messageloop.Node) (*quicstream.Server, error) {
	if cfg.Transport.QUIC.Addr == "" {
		return nil, nil
	}
	opts := quicstream.Options{
		Addr:        cfg.Transport.QUIC.Addr,
		TLSCertFile: cfg.Transport.QUIC.TLS.CertFile,
		TLSKeyFile:  cfg.Transport.QUIC.TLS.KeyFile,
		Insecure:    cfg.Transport.QUIC.Insecure,
	}
	if cfg.Transport.QUIC.WriteTimeout == "" {
		opts.WriteTimeout = quicstream.DefaultWriteTimeout
	} else if d, err := time.ParseDuration(cfg.Transport.QUIC.WriteTimeout); err == nil {
		opts.WriteTimeout = d
	}
	if cfg.Transport.QUIC.ReadTimeout != "" {
		if d, err := time.ParseDuration(cfg.Transport.QUIC.ReadTimeout); err == nil {
			opts.ReadTimeout = d
		}
	}
	hb := node.GetHeartbeatConfig()
	if hb.IdleTimeout > 0 {
		// Keep the QUIC idle timeout above the application heartbeat so the
		// protocol-level idle check (3511) fires first.
		opts.MaxIdleTimeout = 2 * hb.IdleTimeout
		if opts.MaxIdleTimeout < quicstream.DefaultMaxIdleTimeout {
			opts.MaxIdleTimeout = quicstream.DefaultMaxIdleTimeout
		}
	}
	if hb.PingInterval > 0 {
		opts.KeepAlivePeriod = hb.PingInterval
	}
	return quicstream.NewServer(opts, node)
}

// newAdminServer builds the HTTP admin server component (health + metrics).
func newAdminServer(cfg *config.Config, node *messageloop.Node, reg *prometheus.Registry) *lynxhttp.Server {
	adminAddr := cfg.Server.Http.Addr
	if adminAddr == "" {
		adminAddr = "127.0.0.1:8080"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", node.HealthHandler())
	return lynxhttp.NewServer(mux, lynxhttp.WithAddr(adminAddr))
}
