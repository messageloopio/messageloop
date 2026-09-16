package grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip"

	"github.com/messageloopio/messageloop/proxy"
)

type Options struct {
	Addr         string        `yaml:"addr" json:"addr"`
	WriteTimeout time.Duration `yaml:"write_timeout" json:"write_timeout"`
	TLSCertFile  string
	TLSKeyFile   string
	// AuthTokens is the static Server API bearer token list: any constant-time
	// match grants the superadmin identity (design D28). Server API listener only.
	AuthTokens []string
	// APIAllowInsecure serves the Server API without authentication
	// (requires config server.api.allow_insecure: true).
	APIAllowInsecure bool
	// APIAuthCacheTTL is the positive cache TTL for Server API keys verified
	// through the api_auth-assigned proxy (0 = the 30s resolver default).
	APIAuthCacheTTL time.Duration
	// APIFindProxy returns the api_auth-assigned proxy (nil = no
	// assignment; key verification then does not exist).
	APIFindProxy func() proxy.Proxy
	// APICapabilityCeiling is the node capability upper bound as closed-set
	// capability names (nil → the default capability ceiling at the Server API
	// layer). Server API listener only.
	APICapabilityCeiling []string
	MaxRecvMsgSize       int // Max inbound message size in bytes (0 = gRPC default)
}

func validateOptions(name string, opts Options) error {
	if opts.Addr == "" {
		return fmt.Errorf("%s addr is required", name)
	}
	if (opts.TLSCertFile == "") != (opts.TLSKeyFile == "") {
		return fmt.Errorf("%s tls cert_file and key_file must both be set", name)
	}
	return nil
}

// PrepareServer validates opts, pre-binds the listener, builds a gRPC server
// wired with the package RawCodec, and runs register on it.
func PrepareServer(name string, opts Options, register func(*googlegrpc.Server), extraOpts ...googlegrpc.ServerOption) (*Server, error) {
	if err := validateOptions(name, opts); err != nil {
		return nil, err
	}

	grpcOpts := append([]googlegrpc.ServerOption{}, extraOpts...)
	// Wire the package RawCodec per-server instead of registering it globally:
	// a global registration under the default "proto" name would override the
	// standard codec for every gRPC connection in the process. RawCodec also
	// handles regular proto messages, so non-streaming services on this server
	// (e.g. the Server API) are unaffected.
	grpcOpts = append(grpcOpts, googlegrpc.ForceServerCodec(&RawCodec{}))
	if opts.MaxRecvMsgSize > 0 {
		grpcOpts = append(grpcOpts, googlegrpc.MaxRecvMsgSize(opts.MaxRecvMsgSize))
	}
	if opts.TLSCertFile != "" {
		creds, err := credentials.NewServerTLSFromFile(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load %s tls credentials: %w", name, err)
		}
		grpcOpts = append(grpcOpts, googlegrpc.Creds(creds))
	}

	conn, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", name, err)
	}

	grpcServer := googlegrpc.NewServer(grpcOpts...)
	register(grpcServer)

	return &Server{
		name: name,
		grpc: grpcServer,
		conn: conn,
		opts: &opts,
	}, nil
}

// The former single-token admin interceptor was removed (design D18′/KD-K31
// no-compat): Server API authentication now lives in internal/serverapi/auth.go — a
// resolver-backed interceptor supporting the auth_tokens list,
// allow_insecure, and api_auth proxy assignment, wired by
// serverapi.PrepareServer.

type Server struct {
	name string
	grpc *googlegrpc.Server
	conn net.Listener
	opts *Options

	closeOnce sync.Once
	closeErr  error
}

func (s *Server) Name() string {
	return s.name
}

func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	if s.conn != nil {
		return s.conn.Addr().String()
	}
	if s.opts != nil {
		return s.opts.Addr
	}
	return ""
}

func (s *Server) Init(lynx.AppContext) error {
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting gRPC server", "name", s.name, "addr", s.Addr())
	return s.grpc.Serve(s.conn)
}

func (s *Server) Stop(ctx context.Context) error {
	log.InfoContext(ctx, "stopping gRPC server", "name", s.name, "addr", s.Addr())
	return s.close(true)
}

func (s *Server) Close() error {
	return s.close(false)
}

func (s *Server) close(graceful bool) error {
	s.closeOnce.Do(func() {
		if graceful {
			s.grpc.GracefulStop()
		} else {
			s.grpc.Stop()
		}
		if s.conn != nil {
			if err := s.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}

var _ lynx.Service = new(Server)
