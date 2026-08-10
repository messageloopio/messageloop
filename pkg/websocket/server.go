package websocket

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
)

type Server struct {
	lx   lynx.AppContext
	mux  *http.ServeMux
	opts *Options
	s    *http.Server
}

type Options struct {
	Addr         string
	WsPath       string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	CheckOrigin  func(r *http.Request) bool
	TLSCertFile  string
	TLSKeyFile   string
	Compression  bool
}

// DefaultWSWriteTimeout bounds each WebSocket write so a slow consumer
// cannot block a broadcast indefinitely (aligned with the gRPC transport's
// default write timeout). Set WriteTimeout to 0 to disable explicitly.
const DefaultWSWriteTimeout = 10 * time.Second

func DefaultOptions() Options {
	return Options{
		Addr:         ":9080",
		WsPath:       "/ws",
		WriteTimeout: DefaultWSWriteTimeout,
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

func (s *Server) Init(ctx lynx.AppContext) error {
	s.lx = ctx
	s.s = &http.Server{
		Addr:    s.opts.Addr,
		Handler: s.mux,
		// ReadHeaderTimeout mitigates slowloris-style header attacks.
		// IdleTimeout is intentionally not set: WebSocket connections are
		// long-lived and the timeout does not apply after upgrade anyway,
		// but keeping it unset avoids any surprise on pre-upgrade keep-alive
		// connections.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	if s.opts.TLSCertFile != "" && s.opts.TLSKeyFile != "" {
		log.InfoContext(ctx, "starting websocket server with TLS", "addr", s.opts.Addr)
		return s.s.ListenAndServeTLS(s.opts.TLSCertFile, s.opts.TLSKeyFile)
	}
	log.InfoContext(ctx, "starting websocket server", "addr", s.opts.Addr)
	return s.s.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "stopping websocket server", "addr", s.opts.Addr)
	if err := s.s.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.ErrorContext(ctx, "shutting down websocket server failed", err)
		return err
	}
	return nil
}

var _ lynx.Service = new(Server)
