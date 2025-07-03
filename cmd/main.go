package main

import (
	"context"
	"github.com/deeploopdev/messageloop"
	"github.com/deeploopdev/messageloop/grpcstream"
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
		grpcServer, err := grpcstream.NewServer(node)
		if err != nil {
			return err
		}
		if err := lx.Load(grpcServer); err != nil {
			return err
		}

		return nil
	})

	app.Run()
}
