package grpc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Options struct {
	Addr           string        `yaml:"addr" json:"addr"`
	WriteTimeout   time.Duration `yaml:"write_timeout" json:"write_timeout"`
	TLSCertFile    string
	TLSKeyFile     string
	AdminAuthToken string // Bearer token for admin API authentication
	// AdminAllowInsecure serves the admin API without authentication
	// (requires config server.grpc_admin.allow_insecure: true).
	AdminAllowInsecure bool
	MaxRecvMsgSize     int // Max inbound message size in bytes (0 = gRPC default)
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
	// (e.g. the admin API) are unaffected.
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

// AdminAuthInterceptor returns a gRPC unary interceptor that validates bearer tokens.
func AdminAuthInterceptor(token string) googlegrpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *googlegrpc.UnaryServerInfo, handler googlegrpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		values := md.Get("authorization")
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing authorization token")
		}
		authHeader := values[0]
		const bearerPrefix = "Bearer "
		if len(authHeader) <= len(bearerPrefix) || authHeader[:len(bearerPrefix)] != bearerPrefix {
			return nil, status.Error(codes.Unauthenticated, "invalid authorization format")
		}
		// Constant-time comparison so token timing cannot leak the expected value.
		// ConstantTimeCompare is length-safe: mismatched lengths return 0.
		if subtle.ConstantTimeCompare([]byte(authHeader[len(bearerPrefix):]), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid authorization token")
		}
		return handler(ctx, req)
	}
}

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
