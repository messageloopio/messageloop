package grpcstream

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/gzip"
)

type Options struct {
	Addr         string        `yaml:"addr" json:"addr"`
	WriteTimeout time.Duration `yaml:"write_timeout" json:"write_timeout"`
	TLSCertFile  string
	TLSKeyFile   string
}

func NewServer(opts Options, node *messageloop.Node) (*Server, error) {
	encoding.RegisterCodec(&RawCodec{})
	grpcOpts := []grpc.ServerOption{}

	if opts.TLSCertFile != "" && opts.TLSKeyFile != "" {
		creds, err := credentials.NewServerTLSFromFile(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS credentials: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
	}

	grpcServer := grpc.NewServer(grpcOpts...)

	// Register client streaming service
	var handlerOpts []GRPCHandlerOption
	if opts.WriteTimeout > 0 {
		handlerOpts = append(handlerOpts, WithWriteTimeout(opts.WriteTimeout))
	}
	clientHandler := NewGRPCHandler(node, handlerOpts...)
	clientpb.RegisterMessageLoopServiceServer(grpcServer, clientHandler)

	// Register server-side API service
	apiHandler := NewAPIServiceHandler(node)
	serverpb.RegisterAPIServiceServer(grpcServer, apiHandler)

	return newServer(grpcServer, opts)
}

func newServer(grpcServer *grpc.Server, opts Options) (*Server, error) {
	conn, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, err
	}
	return &Server{
		grpc: grpcServer,
		conn: conn,
		opts: &opts,
	}, nil
}

type Server struct {
	grpc *grpc.Server
	lx   lynx.Lynx
	conn net.Listener
	opts *Options
}

func (s *Server) Name() string {
	return "grpc-stream-server"
}

func (s *Server) Init(lx lynx.Lynx) error {
	s.lx = lx
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting gRPC streaming server", "addr", s.opts.Addr)
	return s.grpc.Serve(s.conn)
}

func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping gRPC streaming server", "addr", s.opts.Addr)
	s.grpc.GracefulStop()
}

var _ lynx.Component = new(Server)
