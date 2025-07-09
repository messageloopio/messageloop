package grpcstream

import (
	clientv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/client/v1"
	sharedv1 "github.com/deeplooplabs/messageloop-protocol/gen/proto/go/shared/v1"
	"github.com/deeplooplabs/messageloop/engine"
	"google.golang.org/grpc"
	"sync"
	"time"
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

func (t *Transport) Close(disconnect engine.Disconnect) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.writeError(int32(disconnect.Code), disconnect.Reason)
	time.Sleep(100 * time.Millisecond)
	close(t.closeCh)
	t.closed = true
	return nil
}

func (t *Transport) writeError(code int32, reason string) {
	msg := engine.MakeServerMessage(nil, func(out *clientv1.ServerMessage) {
		out.Envelope = &clientv1.ServerMessage_Error{
			Error: &sharedv1.Error{
				Code:   code,
				Reason: reason,
			},
		}
	})
	_ = t.stream.Send(msg)
}

var _ engine.Transport = new(Transport)

func newGRPCTransport(
	stream grpc.BidiStreamingServer[clientv1.ClientMessage, clientv1.ServerMessage],
) *Transport {
	return &Transport{
		stream:  stream,
		closeCh: make(chan struct{}),
	}
}
