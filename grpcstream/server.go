package grpcstream

import (
	"context"
	"github.com/deeplooplabs/messageloop"
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	"github.com/lynx-go/lynx"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"net"
)

func NewServer(node *messageloop.Node) (*Server, error) {
	encoding.RegisterCodec(&RawCodec{})
	grpcOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(grpcOpts...)
	handler := NewGRPCHandler(node)
	clientv1.RegisterMessageLoopServiceServer(grpcServer, handler)
	return newServer(grpcServer, ":9090")
}

func newServer(grpcServer *grpc.Server, addr string) (*Server, error) {
	conn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return &Server{
		grpc: grpcServer,
		conn: conn,
	}, nil
}

type Server struct {
	grpc *grpc.Server
	lx   lynx.Lynx
	conn net.Listener
}

func (s *Server) Name() string {
	return "grpc-stream-server"
}

func (s *Server) Init(lx lynx.Lynx) error {
	s.lx = lx
	return nil
}

func (s *Server) Start(ctx context.Context) error {
	log.InfoContext(ctx, "starting gRPC streaming server", "addr", s.conn.Addr())
	return s.grpc.Serve(s.conn)
}

func (s *Server) Stop(ctx context.Context) {
	log.InfoContext(ctx, "stopping gRPC streaming server")
	s.grpc.GracefulStop()
}

var _ lynx.Component = new(Server)
