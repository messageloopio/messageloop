package grpcstream

import (
	"sync"
	"time"

	"github.com/messageloopio/messageloop"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/v1"
	"google.golang.org/grpc"
)

type Transport struct {
	stream    grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage]
	mu        sync.RWMutex
	closed    bool
	closeCh   chan struct{}
	closeOnce sync.Once
}

func (t *Transport) Write(message []byte) error {
	return t.WriteMany(message)
}

func (t *Transport) WriteMany(messages ...[]byte) error {
	// Check if closed using a read lock
	t.mu.RLock()
	if t.closed {
		t.mu.RUnlock()
		return nil
	}
	t.mu.RUnlock()

	for i := 0; i < len(messages); i++ {
		// Double-check after acquiring the write lock for each send
		t.mu.RLock()
		closed := t.closed
		t.mu.RUnlock()
		if closed {
			return nil
		}
		if err := t.stream.SendMsg(rawFrame(messages[i])); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) Close(disconnect messageloop.Disconnect) error {
	var err error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			err = nil
			return
		}
		t.writeError(int32(disconnect.Code), disconnect.Reason)
		time.Sleep(100 * time.Millisecond)
		close(t.closeCh)
		t.closed = true
	})
	return err
}

func (t *Transport) writeError(code int32, reason string) {
	msg := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "DISCONNECT_ERROR",
				Type:    "transport_error",
				Message: reason,
			},
		}
	})
	_ = t.stream.Send(msg)
}

var _ messageloop.Transport = new(Transport)

func newGRPCTransport(
	stream grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage],
) *Transport {
	return &Transport{
		stream:  stream,
		closeCh: make(chan struct{}),
	}
}
