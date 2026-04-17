package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
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
		lynx.WithCloseTimeout(30*time.Second),
	)
	app := lynx.New(opts, func(ctx context.Context, app lynx.Lynx) error {
		app.SetLogger(zap.MustNewLogger(app))
		cfg := &config.Config{}
		if err := app.Config().Unmarshal(cfg, lynx.TagNameJSON); err != nil {
			return err
		}

		node := messageloop.NewNode(&cfg.Server)

		// Configure broker based on config
		brokerType := cfg.Broker.Type
		if brokerType == "" {
			brokerType = "memory" // default
		}

		var broker messageloop.Broker
		switch brokerType {
		case "redis":
			broker = redisbroker.New(cfg.Broker.Redis)
		case "memory":
			broker = messageloop.NewMemoryBroker(messageloop.MemoryBrokerOptions{})
		default:
			return fmt.Errorf("unknown broker type: %s", brokerType)
		}
		node.SetBroker(broker)

		// Configure proxies from config
		if len(cfg.Proxy) > 0 {
			proxyConfigs := make([]*proxyproxy.ProxyConfig, 0, len(cfg.Proxy))
			for _, p := range cfg.Proxy {
				pc, err := p.ToProxyConfig()
				if err != nil {
					return fmt.Errorf("invalid proxy config %s: %w", p.Name, err)
				}
				// Parse timeout duration
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
		}

		if err := node.Run(ctx); err != nil {
			return err
		}

		// Set up Prometheus metrics and admin HTTP server
		reg := prometheus.NewRegistry()
		metrics := messageloop.NewMetrics(reg)
		node.SetMetrics(metrics)

		adminAddr := cfg.Server.Http.Addr
		if adminAddr == "" {
			adminAddr = ":8080"
		}
		adminMux := http.NewServeMux()
		adminMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		adminMux.HandleFunc("/health", node.HealthHandler())
		adminServer := &http.Server{Addr: adminAddr, Handler: adminMux}
		go func() {
			if err := adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				app.Logger().Error("admin HTTP server error: " + err.Error())
			}
		}()
		go func() {
			<-ctx.Done()
			// Drain all client connections before shutting down.
			node.Shutdown()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = adminServer.Shutdown(shutdownCtx)
		}()

		grpcOpts := grpcstream.Options{
			Addr: cfg.Transport.GRPC.Addr,
		}
		if cfg.Transport.GRPC.WriteTimeout != "" {
			if d, err := time.ParseDuration(cfg.Transport.GRPC.WriteTimeout); err == nil {
				grpcOpts.WriteTimeout = d
			}
		}
		grpcServer, err := grpcstream.NewServer(grpcOpts, node)
		if err != nil {
			return err
		}

		wsOpts := websocket.Options{
			Addr:   cfg.Transport.WebSocket.Addr,
			WsPath: cfg.Transport.WebSocket.Path,
		}
		if cfg.Transport.WebSocket.WriteTimeout != "" {
			if d, err := time.ParseDuration(cfg.Transport.WebSocket.WriteTimeout); err == nil {
				wsOpts.WriteTimeout = d
			}
		}
		if cfg.Transport.WebSocket.CheckOrigin {
			app.Logger().Info("setting websocket CheckOrigin to allow all origins")
			wsOpts.CheckOrigin = func(r *http.Request) bool { return true }
		}
		wsServer := websocket.NewServer(wsOpts, node)
		if err := app.Hooks(lynx.Components(wsServer, grpcServer)); err != nil {
			return err
		}
		return nil
	})

	app.Run()
}
