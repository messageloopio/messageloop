package grpcstream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fakeBidiStream is an in-memory gRPC bidi stream that records concurrent
// SendMsg calls so tests can assert single-writer semantics.
type fakeBidiStream struct {
	grpc.ServerStream
	ctx context.Context

	mu             sync.Mutex
	inSend         bool
	concurrentSend bool
	sent           []*clientpb.OutboundMessage
	blockSend      chan struct{}
	blockClosed    bool
}

var _ grpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage] = (*fakeBidiStream)(nil)

func newFakeBidiStream() *fakeBidiStream {
	return &fakeBidiStream{ctx: context.Background()}
}

func (s *fakeBidiStream) Context() context.Context {
	return s.ctx
}

func (s *fakeBidiStream) Send(m *clientpb.OutboundMessage) error {
	return s.SendMsg(m)
}

func (s *fakeBidiStream) Recv() (*clientpb.InboundMessage, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}

func (s *fakeBidiStream) SendMsg(m any) error {
	s.mu.Lock()
	if s.inSend {
		s.concurrentSend = true
	}
	s.inSend = true
	block := s.blockSend
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inSend = false
		s.mu.Unlock()
	}()
	// Widen the critical section so overlapping sends are reliably detected.
	time.Sleep(time.Microsecond)
	if block != nil {
		<-block
	}
	out, ok := m.(*clientpb.OutboundMessage)
	if !ok {
		frame, ok := m.(rawFrame)
		if !ok {
			return fmt.Errorf("unexpected message type %T", m)
		}
		out = &clientpb.OutboundMessage{}
		if err := proto.Unmarshal(frame, out); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.sent = append(s.sent, out)
	s.mu.Unlock()
	return nil
}

func (s *fakeBidiStream) setBlock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockSend == nil {
		s.blockSend = make(chan struct{})
	}
}

func (s *fakeBidiStream) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockSend != nil && !s.blockClosed {
		close(s.blockSend)
		s.blockClosed = true
	}
}

func (s *fakeBidiStream) hasConcurrentSend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.concurrentSend
}

func (s *fakeBidiStream) sentMessages() []*clientpb.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*clientpb.OutboundMessage(nil), s.sent...)
}

// TestTransport_ConcurrentWriteManyAndClose hammers WriteMany from many
// goroutines while Close runs concurrently. Before the P0-2 fix this panicked
// with "send on closed channel" and sent concurrently on the gRPC stream.
func TestTransport_ConcurrentWriteManyAndClose(t *testing.T) {
	stream := newFakeBidiStream()
	transport := newGRPCTransport(stream, "fake-addr", 5*time.Second)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = transport.WriteMany([]byte("payload"))
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, transport.Close(messageloop.Disconnect{Code: 3500, Reason: "test disconnect"}))
	require.NoError(t, transport.Close(messageloop.Disconnect{Code: 3501, Reason: "second close is a no-op"}))
	close(stop)
	wg.Wait()

	require.False(t, stream.hasConcurrentSend(), "detected concurrent SendMsg on the gRPC stream")
	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)

	sent := stream.sentMessages()
	require.NotEmpty(t, sent)
	foundErrorFrame := false
	for _, msg := range sent {
		if errMsg := msg.GetError(); errMsg != nil && errMsg.GetCode() == "DISCONNECT_ERROR" {
			foundErrorFrame = true
		}
	}
	require.True(t, foundErrorFrame, "disconnect error frame was not delivered")
}

// TestTransport_ConcurrentWriteManyAndClose_DefaultWriteTimeout covers the
// default configuration where no write timeout is configured. All sends must
// still be serialized through the worker goroutine.
func TestTransport_ConcurrentWriteManyAndClose_DefaultWriteTimeout(t *testing.T) {
	stream := newFakeBidiStream()
	transport := newGRPCTransport(stream, "fake-addr", 0)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = transport.WriteMany([]byte("payload"))
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, transport.Close(messageloop.Disconnect{Code: 3500, Reason: "test disconnect"}))
	close(stop)
	wg.Wait()

	require.False(t, stream.hasConcurrentSend(), "detected concurrent SendMsg on the gRPC stream")
	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
}

// TestTransport_WriteManyTimesOutWhenWorkerBlocked verifies that a blocked
// worker does not hang or panic WriteMany: it times out, and Close afterwards
// still works.
func TestTransport_WriteManyTimesOutWhenWorkerBlocked(t *testing.T) {
	stream := newFakeBidiStream()
	stream.setBlock()
	transport := newGRPCTransport(stream, "fake-addr", 100*time.Millisecond)

	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- transport.WriteMany([]byte("blocked payload"))
	}()

	select {
	case err := <-writeErrCh:
		require.Error(t, err)
		require.Contains(t, err.Error(), "write timeout")
	case <-time.After(2 * time.Second):
		t.Fatal("WriteMany did not time out while the worker was blocked")
	}

	stream.release()
	require.NoError(t, transport.Close(messageloop.Disconnect{Code: 3500, Reason: "test disconnect"}))
	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
}
