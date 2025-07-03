package main

import (
	"context"
	"github.com/deeploopdev/messageloop"
	"github.com/deeploopdev/messageloop/grpcstream"
	"github.com/lynx-go/lynx"
)

func main() {
	o := lynx.BindOptions()
	app := lynx.New(o, func(ctx context.Context, lx lynx.Lynx) error {
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
