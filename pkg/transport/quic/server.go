package quic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"github.com/quic-go/quic-go"

	"github.com/messageloopio/messageloop/internal/runtime"
)

// Options configures the QUIC client listener.
type Options struct {
	Addr            string
	WriteTimeout    time.Duration
	ReadTimeout     time.Duration
	TLSCertFile     string
	TLSKeyFile      string
	Insecure        bool
	MaxIdleTimeout  time.Duration
	KeepAlivePeriod time.Duration
}

// DefaultWriteTimeout bounds each QUIC stream write so a slow consumer
// cannot block a broadcast indefinitely (aligned with WebSocket/gRPC).
const DefaultWriteTimeout = 10 * time.Second

// DefaultMaxIdleTimeout is the QUIC connection idle timeout when the
// application heartbeat idle timeout is unset. quic-go's own default is 30s,
// which is too short for a long-lived messaging session.
const DefaultMaxIdleTimeout = 5 * time.Minute

// DefaultKeepAlivePeriod is the interval at which quic-go sends transport
// PINGs to keep NATs and the peer idle timeout from firing.
const DefaultKeepAlivePeriod = 15 * time.Second

// Server is a lynx.Service that accepts QUIC client connections.
type Server struct {
	node *runtime.Node
	opts Options
	ln   *quic.Listener

	stopped atomic.Bool
	mu      sync.Mutex
}

// NewServer pre-binds a UDP/QUIC listener so startup fails before the
// accept loop if the address or TLS material is invalid.
func NewServer(opts Options, node *runtime.Node) (*Server, error) {
	if opts.Addr == "" {
		return nil, fmt.Errorf("quic-server addr is required")
	}
	if (opts.TLSCertFile == "") != (opts.TLSKeyFile == "") {
		return nil, fmt.Errorf("quic-server tls cert_file and key_file must both be set")
	}
	tlsConf, err := loadTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	maxIdle := opts.MaxIdleTimeout
	if maxIdle <= 0 {
		maxIdle = DefaultMaxIdleTimeout
	}
	keepAlive := opts.KeepAlivePeriod
	if keepAlive <= 0 {
		keepAlive = DefaultKeepAlivePeriod
	}
	ln, err := quic.ListenAddr(opts.Addr, tlsConf, &quic.Config{
		MaxIdleTimeout:        maxIdle,
		KeepAlivePeriod:       keepAlive,
		MaxIncomingStreams:    8,
		MaxIncomingUniStreams: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("listen quic: %w", err)
	}
	return &Server{node: node, opts: opts, ln: ln}, nil
}

func (s *Server) Name() string {
	return "quic"
}

func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Init(lynx.AppContext) error {
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	if s.opts.Insecure && s.opts.TLSCertFile == "" {
		log.InfoContext(ctx, "starting quic server with self-signed certificate (insecure)", "addr", s.Addr())
	} else {
		log.InfoContext(ctx, "starting quic server", "addr", s.Addr())
	}
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if s.stopped.Load() || errors.Is(err, quic.ErrServerClosed) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "stopping quic server", "addr", s.Addr())
	return s.Close()
}

// Close releases the pre-bound QUIC listener.
func (s *Server) Close() error {
	s.stopped.Store(true)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	err := s.ln.Close()
	s.ln = nil
	return err
}

var _ lynx.Service = (*Server)(nil)
