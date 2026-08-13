package grpcstream

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
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
	// sendDelay is applied to every SendMsg after the block check. It models
	// a slow transport so tests can control how long each delivery takes.
	sendDelay time.Duration
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
	delay := s.sendDelay
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
	if delay > 0 {
		time.Sleep(delay)
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

// TestTransport_SlowEnqueueGetsFreshAckBudget is the regression test for
// P1-B3: when the send queue is backed up, a write whose enqueue phase
// consumes most of the budget must still get a full budget for the
// delivery-ack phase. Before the fix the shared timer fired right after the
// slow enqueue, falsely reporting a write timeout on a healthy connection.
//
// Setup: the worker is blocked on the first frame while 64 more frames fill
// the send queue, so the 66th write's enqueue stalls for ~460ms (of a 1s
// budget). After the worker is released, each of the 65 queued frames takes
// 12ms to deliver, so the 66th write is acked at ~+1.24s. The stale shared
// deadline would have fired at +1.0s (false timeout); the fresh ack budget
// runs until +1.46s, so the write must succeed.
func TestTransport_SlowEnqueueGetsFreshAckBudget(t *testing.T) {
	stream := newFakeBidiStream()
	stream.setBlock()
	stream.sendDelay = 12 * time.Millisecond
	transport := newGRPCTransport(stream, "fake-addr", time.Second)

	const fills = 64
	results := make([]chan error, fills+1)
	// The fake stream decodes rawFrame payloads as protobuf, so the queued
	// messages must be valid protobuf bytes.
	queuedFrame, err := proto.Marshal(&clientpb.OutboundMessage{Id: "queued"})
	require.NoError(t, err)
	for i := 0; i < fills+1; i++ {
		results[i] = make(chan error, 1)
		go func(ch chan<- error) { ch <- transport.WriteMany(queuedFrame) }(results[i])
	}

	// Fill the send channel (one write is held by the blocked worker, the
	// remaining fills sit in the channel) so the enqueue phase of the last
	// write stalls.
	require.Eventually(t, func() bool {
		return len(transport.sendCh) == fills
	}, 2*time.Second, 10*time.Millisecond)

	// The next write: its enqueue must wait for a slot to free up.
	slowStart := time.Now()
	slowResult := make(chan error, 1)
	go func() { slowResult <- transport.WriteMany(queuedFrame) }()

	// Make sure the enqueue is actually stalled before releasing the worker.
	time.Sleep(450 * time.Millisecond)
	stream.release()

	var slowErr error
	select {
	case slowErr = <-slowResult:
	case <-time.After(3 * time.Second):
		t.Fatal("the stalled write never completed")
	}
	require.NoError(t, slowErr, "write with slow enqueue must not falsely report a timeout")

	// The write must have succeeded only after the stale deadline would have
	// fired: i.e. it waited for the ack, it did not return at the 1s mark.
	elapsed := time.Since(slowStart)
	require.GreaterOrEqual(t, elapsed, 950*time.Millisecond,
		"write returned too early; the ack phase must get its own budget")

	// Drain the remaining queued writers; they legitimately exceed their
	// budgets (1s enqueue + long queue), so they may return timeouts.
	for i := 0; i < fills+1; i++ {
		select {
		case <-results[i]:
		case <-time.After(2 * time.Second):
			t.Fatal("queued writer never completed")
		}
	}

	// The transport must still close cleanly: the queue is drained by now.
	require.NoError(t, transport.Close(messageloop.Disconnect{Code: 3500, Reason: "test disconnect"}))
	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
	require.False(t, stream.hasConcurrentSend(), "detected concurrent SendMsg on the gRPC stream")
}

// TestTransport_CloseWithFullQueueReturnsPromptly is the regression test for
// P1-B4: Close must not block for the full write timeout when the send queue
// is backed up, and the worker must still drain the queue (backfilling errors)
// once it unblocks.
func TestTransport_CloseWithFullQueueReturnsPromptly(t *testing.T) {
	stream := newFakeBidiStream()
	stream.setBlock()
	transport := newGRPCTransport(stream, "fake-addr", 10*time.Second)

	const fills = 64
	results := make([]chan error, fills+1)
	queuedFrame, err := proto.Marshal(&clientpb.OutboundMessage{Id: "queued"})
	require.NoError(t, err)
	for i := 0; i < fills+1; i++ {
		results[i] = make(chan error, 1)
		go func(ch chan<- error) { ch <- transport.WriteMany(queuedFrame) }(results[i])
	}
	require.Eventually(t, func() bool {
		return len(transport.sendCh) == fills
	}, 2*time.Second, 10*time.Millisecond)

	// Close with the worker still blocked and the queue full: the disconnect
	// frame cannot be enqueued, so Close must degrade to a direct close and
	// return promptly (disconnectFrameTimeout, not the 10s write timeout).
	closeStart := time.Now()
	closeErr := transport.Close(messageloop.Disconnect{Code: 3512, Reason: "slow consumer"})
	require.Less(t, time.Since(closeStart), 5*time.Second, "Close must not block for a full write timeout")
	require.Error(t, closeErr, "disconnect frame could not be enqueued with a full queue")

	// Unblock the worker: it must drain the queue and backfill errors so the
	// queued writers finish promptly instead of hanging for their 10s budget.
	stream.release()
	for i := 0; i < fills+1; i++ {
		select {
		case wErr := <-results[i]:
			require.ErrorIs(t, wErr, ErrTransportClosed, "queued writer %d must be failed during drain", i)
		case <-time.After(3 * time.Second):
			t.Fatalf("queued writer %d never completed after close+drain", i)
		}
	}

	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
	require.False(t, stream.hasConcurrentSend(), "detected concurrent SendMsg on the gRPC stream")
}

// TestTransport_CloseCarriesDisconnectCode verifies that the numeric
// disconnect code (3500-3512) is encoded into the DISCONNECT_ERROR envelope
// metadata, so gRPC clients can distinguish disconnect reasons the way WS
// clients do via the close frame.
func TestTransport_CloseCarriesDisconnectCode(t *testing.T) {
	stream := newFakeBidiStream()
	transport := newGRPCTransport(stream, "fake-addr", 5*time.Second)

	require.NoError(t, transport.Close(messageloop.Disconnect{Code: 3512, Reason: "slow consumer"}))

	sent := stream.sentMessages()
	var errMsg *sharedpb.Error
	for _, msg := range sent {
		if e := msg.GetError(); e != nil {
			errMsg = e
		}
	}
	require.NotNil(t, errMsg, "no error envelope delivered")
	require.Equal(t, "DISCONNECT_ERROR", errMsg.GetCode())
	require.Equal(t, "slow consumer", errMsg.GetMessage())
	require.NotNil(t, errMsg.GetMetadata(), "disconnect code must be carried in the envelope metadata")
	code, ok := errMsg.GetMetadata().GetFields()["disconnect_code"]
	require.True(t, ok, "metadata missing disconnect_code")
	require.Equal(t, float64(3512), code.GetNumberValue())
}
