package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
	lynxhttp "github.com/lynx-go/lynx/server/http"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/pkg/grpcstream"
	"github.com/messageloopio/messageloop/pkg/redisbroker"
	"github.com/messageloopio/messageloop/pkg/websocket"
	proxyproxy "github.com/messageloopio/messageloop/proxy"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
)

var (
	version string
)

func main() {
	opts := lynx.NewOptions(
		lynx.WithName("MessageLoop"),
		lynx.WithVersion(version),
		lynx.WithSetFlagsFunc(func(f *pflag.FlagSet) {
			f.String("config", "./config.yaml", "config file path")
			f.String("log-level", "info", "log level, default info")
		}),
		lynx.WithBindConfigFunc(lynx.DefaultBindConfigFunc),
		lynx.WithShutdownTimeout(30*time.Second),
	)
	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.MustNewLogger(app))
		cfg := &config.Config{}
		if err := app.Config().Unmarshal(cfg, lynx.TagNameJSON); err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}

		node := messageloop.NewNode(&cfg.Server)
		reg := prometheus.NewRegistry()
		metrics := messageloop.NewMetrics(reg)
		node.SetMetrics(metrics)

		cluster, err := setupCluster(cfg, node, metrics)
		if err != nil {
			return err
		}
		node.SetCluster(cluster)

		broker, err := newBroker(cfg)
		if err != nil {
			return err
		}
		node.SetBroker(broker)

		if err = setupProxy(cfg, node); err != nil {
			return err
		}

		var grpcClientServer, grpcAdminServer *grpcstream.Server
		grpcClientServer, err = newGRPCClientServer(cfg, node)
		if err != nil {
			return err
		}
		grpcAdminServer, err = newGRPCAdminServer(cfg, node)
		if err != nil {
			return err
		}

		wsServer := newWebSocketServer(cfg, node, app.Logger())
		adminServer := newAdminServer(cfg, node, reg)

		if err := app.Hooks(lynx.OnStart(node.Run)); err != nil {
			return err
		}
		components := []lynx.Component{wsServer, adminServer, grpcClientServer, grpcAdminServer}
		if err := app.Hooks(lynx.Components(components...), lynx.OnStop(func(ctx context.Context) error {
			// Drain all client connections before shutting down.
			node.Shutdown()
			return nil
		})); err != nil {
			return err
		}

		return nil
	})

	app.Run()
}

// setupCluster creates and wires the cluster based on the provided config.
// For Redis-backed clusters it also configures the session directory, command bus,
// query store, node lease manager, projection repairer, and presence store.
func setupCluster(cfg *config.Config, node *messageloop.Node, metrics *messageloop.Metrics) (*messageloop.Cluster, error) {
	cluster, err := messageloop.NewCluster(messageloop.ClusterOptions{
		Enabled: cfg.Cluster.Enabled,
		NodeID:  cfg.Cluster.NodeID,
		Backend: cfg.Cluster.Backend,
	}, messageloop.ClusterDependencies{})
	if err != nil {
		return nil, fmt.Errorf("invalid cluster config: %w", err)
	}

	if !cluster.Enabled() || cluster.Backend() != "redis" {
		return cluster, nil
	}

	deps := messageloop.ClusterDependencies{}
	deps.SessionDirectory = redisbroker.NewSessionDirectory(cfg.Broker.Redis)
	deps.CommandBus = redisbroker.NewClusterCommandBus(cfg.Broker.Redis, cluster.NodeID(), cluster.IncarnationID())
	deps.QueryStore = redisbroker.NewClusterQueryStore(cfg.Broker.Redis, cluster.NodeID(), cluster.IncarnationID())
	deps.NodeLeaseManager = messageloop.NewClusterNodeLeaseManager(
		deps.SessionDirectory,
		messageloop.ClusterNodeLeaseManagerConfig{
			NodeID:        cluster.NodeID(),
			IncarnationID: cluster.IncarnationID(),
		},
	)
	deps.ProjectionRepairer = messageloop.NewClusterProjectionRepairer(node, deps.QueryStore, messageloop.ClusterProjectionRepairerConfig{})
	deps.CommandBus.SetHandler(node.ClusterCommandHandler())
	if metricsAware, ok := deps.CommandBus.(interface{ SetMetrics(*messageloop.Metrics) }); ok {
		metricsAware.SetMetrics(metrics)
	}
	node.SetPresenceStore(redisbroker.NewPresenceStore(cfg.Broker.Redis))

	cluster, err = messageloop.NewCluster(messageloop.ClusterOptions{
		Enabled:       true,
		NodeID:        cluster.NodeID(),
		Backend:       cluster.Backend(),
		IncarnationID: cluster.IncarnationID(),
	}, deps)
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
		if p.Timeout != "" {
			timeout, err := time.ParseDuration(p.Timeout)
			if err != nil {
				return fmt.Errorf("invalid proxy timeout %s: %w", p.Name, err)
			}
			pc.Timeout = timeout
		}
		proxyConfigs = append(proxyConfigs, pc)
	}
	if err := node.SetupProxy(proxyConfigs); err != nil {
		return fmt.Errorf("failed to setup proxies: %w", err)
	}
	return nil
}

// newWebSocketServer builds the WebSocket server component from config.
func newWebSocketServer(cfg *config.Config, node *messageloop.Node, logger *slog.Logger) *websocket.Server {
	wsOpts := websocket.Options{
		Addr:        cfg.Transport.WebSocket.Addr,
		WsPath:      cfg.Transport.WebSocket.Path,
		TLSCertFile: cfg.Transport.WebSocket.TLS.CertFile,
		TLSKeyFile:  cfg.Transport.WebSocket.TLS.KeyFile,
		Compression: cfg.Transport.WebSocket.Compression,
	}
	if cfg.Transport.WebSocket.WriteTimeout != "" {
		if d, err := time.ParseDuration(cfg.Transport.WebSocket.WriteTimeout); err == nil {
			wsOpts.WriteTimeout = d
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
	return websocket.NewServer(wsOpts, node)
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
