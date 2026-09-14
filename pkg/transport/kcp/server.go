package kcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	kcpgo "github.com/xtaci/kcp-go/v5"

	"github.com/messageloopio/messageloop/internal/runtime"
)

// Options configures the KCP client listener.
type Options struct {
	Addr         string
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
	// DataShards / ParityShards configure forward error correction (Reed
	// Solomon). 0/0 disables FEC (kcp-go's own default). Clients must dial
	// with the same shard counts.
	DataShards   int
	ParityShards int
	TLSCertFile  string
	TLSKeyFile   string
	Insecure     bool
}

// DefaultWriteTimeout bounds each frame write so a slow consumer cannot
// block a broadcast indefinitely (aligned with WebSocket/gRPC/QUIC).
const DefaultWriteTimeout = 10 * time.Second

// DefaultWindowSize is the KCP send/receive window (in packets) applied to
// accepted sessions. kcp-go's own default (32) is tuned for small transfers;
// a messaging session regularly has multiple max-size frames in flight.
const DefaultWindowSize = 256

// Server is a lynx.Service that accepts TLS-secured KCP client sessions.
type Server struct {
	node    *runtime.Node
	opts    Options
	tlsConf *tls.Config
	ln      *kcpgo.Listener

	stopped atomic.Bool
	mu      sync.Mutex
}

// NewServer pre-binds the UDP/KCP listener so startup fails before the
// accept loop if the address or TLS material is invalid.
func NewServer(opts Options, node *runtime.Node) (*Server, error) {
	if opts.Addr == "" {
		return nil, fmt.Errorf("kcp-server addr is required")
	}
	if (opts.TLSCertFile == "") != (opts.TLSKeyFile == "") {
		return nil, fmt.Errorf("kcp-server tls cert_file and key_file must both be set")
	}
	tlsConf, err := loadTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	if opts.DataShards < 0 || opts.ParityShards < 0 {
		return nil, fmt.Errorf("kcp-server data_shards and parity_shards must be >= 0")
	}
	if opts.ParityShards > 0 && opts.DataShards <= 0 {
		return nil, fmt.Errorf("kcp-server parity_shards requires data_shards > 0")
	}
	ln, err := kcpgo.ListenWithOptions(opts.Addr, nil, opts.DataShards, opts.ParityShards)
	if err != nil {
		return nil, fmt.Errorf("listen kcp: %w", err)
	}
	return &Server{node: node, opts: opts, tlsConf: tlsConf, ln: ln}, nil
}

func (s *Server) Name() string {
	return "kcp"
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
		log.InfoContext(ctx, "starting kcp server with self-signed certificate (insecure)", "addr", s.Addr())
	} else {
		log.InfoContext(ctx, "starting kcp server", "addr", s.Addr())
	}
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	for {
		conn, err := ln.AcceptKCP()
		if err != nil {
			if s.stopped.Load() || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

func (s *Server) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "stopping kcp server", "addr", s.Addr())
	return s.Close()
}

// Close releases the pre-bound KCP listener.
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
