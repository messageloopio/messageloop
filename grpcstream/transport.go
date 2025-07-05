package grpcstream

import (
	"github.com/deeploopdev/messageloop"
	clientv1 "github.com/deeploopdev/messageloop-protocol/gen/proto/go/client/v1"
	"google.golang.org/grpc"
	"sync"
)

type Transport struct {
	stream  grpc.BidiStreamingServer[clientv1.ClientMessage, clientv1.ServerMessage]
	mu      sync.RWMutex
	closed  bool
	closeCh chan struct{}
}

func (t *Transport) Write(message []byte) error {
	return t.WriteMany(message)
}

func (t *Transport) WriteMany(messages ...[]byte) error {
	t.mu.RLock()
	if t.closed {
		t.mu.RUnlock()
		return nil
	}
	t.mu.RUnlock()
	for i := 0; i < len(messages); i++ {
		if err := t.stream.SendMsg(rawFrame(messages[i])); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	close(t.closeCh)
	return nil
}

var _ messageloop.Transport = new(Transport)

func newGRPCTransport(
	stream grpc.BidiStreamingServer[clientv1.ClientMessage, clientv1.ServerMessage],
) *Transport {
	return &Transport{
		stream:  stream,
		closeCh: make(chan struct{}),
	}
}
