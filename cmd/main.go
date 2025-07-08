package main

import (
	"context"
	"github.com/deeplooplabs/messageloop"
	"github.com/deeplooplabs/messageloop/grpcstream"
	"github.com/deeplooplabs/messageloop/websocket"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/contrib/log/zap"
)

func main() {
	o := lynx.BindOptions()
	if o.Name == "" {
		o.Name = "MessageLoop"
	}
	app := lynx.New(o, func(ctx context.Context, lx lynx.Lynx) error {
		slogger := zap.NewLogger(lx, o.LogLevel)
		lx.SetLogger(slogger)

		node := messageloop.NewNode()
		if err := node.Run(); err != nil {
			return err
		}
		grpcServer, err := grpcstream.NewServer(node)
		if err != nil {
			return err
		}
		if err := lx.Load(grpcServer); err != nil {
			return err
		}
		wsServer := websocket.NewServer(websocket.DefaultOptions(), node)
		if err := lx.Load(wsServer); err != nil {
			return err
		}
		return nil
	})

	app.Run()
}
