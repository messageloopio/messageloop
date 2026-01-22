package main

import (
	"context"
	"fmt"
	"time"

	"github.com/deeplooplabs/messageloop"
	"github.com/deeplooplabs/messageloop/config"
	"github.com/deeplooplabs/messageloop/grpcstream"
	redisbroker "github.com/deeplooplabs/messageloop/pkg/redisbroker"
	"github.com/deeplooplabs/messageloop/websocket"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/zap"
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

		node := messageloop.NewNode()

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

		if err := node.Run(); err != nil {
			return err
		}
		grpcServer, err := grpcstream.NewServer(grpcstream.Options{
			Addr: cfg.Transport.GRPC.Addr,
		}, node)
		if err != nil {
			return err
		}

		wsServer := websocket.NewServer(websocket.Options{
			Addr:   cfg.Transport.WebSocket.Addr,
			WsPath: cfg.Transport.WebSocket.Path,
		}, node)
		if err := app.Hooks(lynx.Components(wsServer, grpcServer)); err != nil {
			return err
		}
		return nil
	})

	app.Run()
}
