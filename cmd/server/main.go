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
	redisbroker "github.com/messageloopio/messageloop/pkg/redisbroker"
	"github.com/messageloopio/messageloop/pkg/websocket"
	proxyproxy "github.com/messageloopio/messageloop/proxy"
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
		if err := app.Config().Unmarshal(cfg); err != nil {
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
			// Generate a unique node ID for this instance
			nodeID := fmt.Sprintf("node-%d", time.Now().UnixNano())
			broker = redisbroker.New(cfg.Broker.Redis, nodeID)
		case "memory":
			broker = messageloop.NewMemoryBroker()
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

		if err := node.Run(); err != nil {
			return err
		}
		grpcServer, err := grpcstream.NewServer(grpcstream.Options{
			Addr: cfg.Transport.GRPC.Addr,
		}, node)
		if err != nil {
			return err
		}

		wsOpts := websocket.Options{
			Addr:   cfg.Transport.WebSocket.Addr,
			WsPath: cfg.Transport.WebSocket.Path,
		}
		if cfg.Transport.WebSocket.CheckOrigin {
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
