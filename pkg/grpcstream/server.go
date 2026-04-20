package grpcstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
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
	MaxRecvMsgSize int    // Max inbound message size in bytes (0 = gRPC default)
}

var registerRawCodecOnce sync.Once

func registerRawCodec() {
	registerRawCodecOnce.Do(func() {
		encoding.RegisterCodec(&RawCodec{})
	})
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

func prepareServer(name string, opts Options, register func(*grpc.Server), extraOpts ...grpc.ServerOption) (*Server, error) {
	if err := validateOptions(name, opts); err != nil {
		return nil, err
	}

	registerRawCodec()

	grpcOpts := append([]grpc.ServerOption{}, extraOpts...)
	if opts.MaxRecvMsgSize > 0 {
		grpcOpts = append(grpcOpts, grpc.MaxRecvMsgSize(opts.MaxRecvMsgSize))
	}
	if opts.TLSCertFile != "" {
		creds, err := credentials.NewServerTLSFromFile(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load %s tls credentials: %w", name, err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}

	conn, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", name, err)
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	register(grpcServer)

	return &Server{
		name: name,
		grpc: grpcServer,
		conn: conn,
		opts: &opts,
	}, nil
}

// adminAuthInterceptor returns a gRPC unary interceptor that validates bearer tokens.
func adminAuthInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
		if authHeader[len(bearerPrefix):] != token {
			return nil, status.Error(codes.Unauthenticated, "invalid authorization token")
		}
		return handler(ctx, req)
	}
}

type Server struct {
	name string
	grpc *grpc.Server
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

func (s *Server) Init(lynx.Lynx) error {
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting gRPC server", "name", s.name, "addr", s.Addr())
	return s.grpc.Serve(s.conn)
}

func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping gRPC server", "name", s.name, "addr", s.Addr())
	_ = s.close(true)
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

var _ lynx.Component = new(Server)
