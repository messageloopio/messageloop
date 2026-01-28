package grpcstream

import (
	"context"
	"net"

	"github.com/fleetlit/messageloop"
	clientpb "github.com/fleetlit/messageloop/genproto/v1"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
)

type Options struct {
	Addr string `yaml:"addr" json:"addr"`
}

func NewServer(opts Options, node *messageloop.Node) (*Server, error) {
	encoding.RegisterCodec(&RawCodec{})
	grpcOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(grpcOpts...)
	handler := NewGRPCHandler(node)
	clientpb.RegisterMessagingServiceServer(grpcServer, handler)
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
