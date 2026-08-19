package grpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/internal/protocol"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/stretchr/testify/require"
	googlegrpc "google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fakeBidiStream is an in-memory gRPC bidi stream that records concurrent
// SendMsg calls so tests can assert single-writer semantics.
type fakeBidiStream struct {
	googlegrpc.ServerStream
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

var _ googlegrpc.BidiStreamingServer[clientpb.InboundMessage, clientpb.OutboundMessage] = (*fakeBidiStream)(nil)

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
	require.NoError(t, transport.Close(protocol.Disconnect{Code: 3500, Reason: "test disconnect"}))
	require.NoError(t, transport.Close(protocol.Disconnect{Code: 3501, Reason: "second close is a no-op"}))
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
	require.NoError(t, transport.Close(protocol.Disconnect{Code: 3500, Reason: "test disconnect"}))
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
	require.NoError(t, transport.Close(protocol.Disconnect{Code: 3500, Reason: "test disconnect"}))
	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
}

// TestTransport_SlowEnqueueGetsFreshAckBudget is the P1-B3 regression test
// adapted to the PR-KA-B1 depth-1 handoff: the send channel is a single-slot
// handoff, not a 64-deep buffer. A write whose enqueue stalls on the
// occupied slot must still get a full budget for the delivery-ack phase —
// it must succeed once the slot frees up, never falsely reporting a timeout
// on a connection that merely queued behind the handoff.
//
// Setup: the worker is blocked on frame A (in flight) while frame B fills
// the single handoff slot, so write C's enqueue stalls. The worker is
// released at ~450ms (well inside the 1s budget): A and B are acked (12ms
// each), C's enqueue succeeds and its ack phase gets a fresh budget, so C
// must succeed.
func TestTransport_SlowEnqueueGetsFreshAckBudget(t *testing.T) {
	stream := newFakeBidiStream()
	stream.setBlock()
	stream.sendDelay = 12 * time.Millisecond
	transport := newGRPCTransport(stream, "fake-addr", time.Second)

	queuedFrame, err := proto.Marshal(&clientpb.OutboundMessage{Id: "queued"})
	require.NoError(t, err)

	// Frame A: dequeued by the worker, blocked in-flight.
	writeA := make(chan error, 1)
	go func() { writeA <- transport.WriteMany(queuedFrame) }()

	// Frame B: fills the single handoff slot (the worker is still blocked on A).
	writeB := make(chan error, 1)
	go func() { writeB <- transport.WriteMany(queuedFrame) }()
	require.Eventually(t, func() bool {
		return len(transport.sendCh) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Frame C: its enqueue must wait for the slot to free up.
	slowStart := time.Now()
	writeC := make(chan error, 1)
	go func() { writeC <- transport.WriteMany(queuedFrame) }()

	// Make sure the enqueue is actually stalled before releasing the worker.
	time.Sleep(450 * time.Millisecond)
	stream.release()

	select {
	case err := <-writeC:
		require.NoError(t, err, "write with slow enqueue must not falsely report a timeout")
	case <-time.After(3 * time.Second):
		t.Fatal("the stalled write never completed")
	}

	// The write must have waited out the handoff backpressure: it succeeded
	// only after the slot freed up.
	elapsed := time.Since(slowStart)
	require.GreaterOrEqual(t, elapsed, 400*time.Millisecond,
		"write returned too early; it must wait for the handoff slot")

	// The earlier writers complete once the worker drains.
	for name, ch := range map[string]chan error{"A": writeA, "B": writeB} {
		select {
		case err := <-ch:
			require.NoError(t, err, "write %s must succeed", name)
		case <-time.After(3 * time.Second):
			t.Fatalf("write %s never completed", name)
		}
	}

	require.NoError(t, transport.Close(protocol.Disconnect{Code: 3500, Reason: "test disconnect"}))
	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
	require.False(t, stream.hasConcurrentSend(), "detected concurrent SendMsg on the gRPC stream")
}

// TestTransport_CloseWithFullQueueReturnsPromptly is the P1-B4 regression
// test adapted to the PR-KA-B1 depth-1 handoff: with the worker blocked and
// the single handoff slot occupied, Close must not block for the full write
// timeout — the disconnect frame enqueue degrades to a direct close after
// disconnectFrameTimeout — and the queued writers must still finish promptly
// once the worker drains.
func TestTransport_CloseWithFullQueueReturnsPromptly(t *testing.T) {
	stream := newFakeBidiStream()
	stream.setBlock()
	transport := newGRPCTransport(stream, "fake-addr", 10*time.Second)

	queuedFrame, err := proto.Marshal(&clientpb.OutboundMessage{Id: "queued"})
	require.NoError(t, err)

	// Frame A is held in-flight by the blocked worker; frame B fills the
	// single handoff slot.
	results := make([]chan error, 2)
	for i := 0; i < 2; i++ {
		results[i] = make(chan error, 1)
		go func(ch chan<- error) { ch <- transport.WriteMany(queuedFrame) }(results[i])
	}
	require.Eventually(t, func() bool {
		return len(transport.sendCh) == 1
	}, 2*time.Second, 10*time.Millisecond)

	// Close with the worker still blocked and the handoff slot occupied: the
	// disconnect frame cannot be enqueued, so Close must degrade to a direct
	// close and return promptly (disconnectFrameTimeout, not the 10s write
	// timeout).
	closeStart := time.Now()
	closeErr := transport.Close(protocol.Disconnect{Code: 3512, Reason: "slow consumer"})
	require.Less(t, time.Since(closeStart), 5*time.Second, "Close must not block for a full write timeout")
	require.Error(t, closeErr, "disconnect frame could not be enqueued with an occupied slot")

	// Unblock the worker: the in-flight write completes and the slot-held
	// write finishes promptly (delivered or failed as closed — the worker
	// select may pick either path once both closeCh and sendCh are ready).
	stream.release()
	for i := 0; i < 2; i++ {
		select {
		case wErr := <-results[i]:
			require.True(t, wErr == nil || errors.Is(wErr, ErrTransportClosed),
				"queued writer %d must complete (nil or ErrTransportClosed), got %v", i, wErr)
		case <-time.After(3 * time.Second):
			t.Fatalf("queued writer %d never completed after close+drain", i)
		}
	}

	require.ErrorIs(t, transport.WriteMany([]byte("after close")), ErrTransportClosed)
	require.False(t, stream.hasConcurrentSend(), "detected concurrent SendMsg on the gRPC stream")
}

// TestTransport_CloseCarriesDisconnectCode verifies that the numeric
// disconnect code (3500-3514) is encoded into the DISCONNECT_ERROR envelope
// metadata, so gRPC clients can distinguish disconnect reasons the way WS
// clients do via the close frame.
func TestTransport_CloseCarriesDisconnectCode(t *testing.T) {
	stream := newFakeBidiStream()
	transport := newGRPCTransport(stream, "fake-addr", 5*time.Second)

	require.NoError(t, transport.Close(protocol.Disconnect{Code: 3512, Reason: "slow consumer"}))

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
