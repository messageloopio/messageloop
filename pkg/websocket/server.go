package websocket

import (
	"context"
	"net/http"

	"github.com/fleetlit/messageloop"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
)

type Server struct {
	lx   lynx.Lynx
	mux  *http.ServeMux
	opts *Options
	s    *http.Server
}

type Options struct {
	Addr   string
	WsPath string
}

func DefaultOptions() Options {
	return Options{
		Addr:   ":9080",
		WsPath: "/ws",
	}
}

func NewServer(
	opts Options,
	node *messageloop.Node,
) *Server {
	mux := http.NewServeMux()
	handler := NewHandler(node, opts)
	mux.HandleFunc(opts.WsPath, handler.ServeHTTP)

	return &Server{
		mux:  mux,
		opts: &opts,
	}
}

func (s *Server) Name() string {
	return "websocket"
}

func (s *Server) Init(lx lynx.Lynx) error {
	s.lx = lx
	s.s = &http.Server{
		Addr:    s.opts.Addr,
		Handler: s.mux,
	}
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting websocket server", "addr", s.opts.Addr)
	return s.s.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping websocket server", "addr", s.opts.Addr)
	if err := s.s.Shutdown(ctx); err != nil {
		log.ErrorContext(ctx, "shutting down websocket server failed", err)
	}
}

var _ lynx.Component = new(Server)
