package messageloopgo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// fakeTransport is a minimal in-memory transport for SDK tests.
type fakeTransport struct {
	mu      sync.Mutex
	sent    []*clientpb.InboundMessage
	recvBuf []*clientpb.OutboundMessage
	closed  bool
	closeCh chan struct{}
	wakeCh  chan struct{}
	sendErr error // when non-nil, Send returns this error
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		closeCh: make(chan struct{}),
		wakeCh:  make(chan struct{}, 1),
	}
}

func (f *fakeTransport) Send(ctx context.Context, msg *clientpb.InboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("transport closed")
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, msg)
	return nil
}

func (f *fakeTransport) Recv(ctx context.Context) (*clientpb.OutboundMessage, error) {
	for {
		f.mu.Lock()
		if len(f.recvBuf) > 0 {
			msg := f.recvBuf[0]
			f.recvBuf = f.recvBuf[1:]
			f.mu.Unlock()
			return msg, nil
		}
		closed := f.closed
		f.mu.Unlock()
		if closed {
			return nil, fmt.Errorf("transport closed")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.closeCh:
			return nil, fmt.Errorf("transport closed")
		case <-f.wakeCh:
		}
	}
}

func (f *fakeTransport) push(msg *clientpb.OutboundMessage) {
	f.mu.Lock()
	f.recvBuf = append(f.recvBuf, msg)
	f.mu.Unlock()
	select {
	case f.wakeCh <- struct{}{}:
	default:
	}
}

func (f *fakeTransport) lastSent() *clientpb.InboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return nil
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.closeCh)
	}
	return nil
}

func (f *fakeTransport) pendingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recvBuf)
}

// TestClientPublish_Transient verifies that Publish forwards the transient
// flag to the server-side Publish message, and that the flag defaults to
// false when the variadic argument is omitted.
func TestClientPublish_Transient(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	if err := c.Publish("ch", msg, true); err != nil {
		t.Fatalf("Publish with transient failed: %v", err)
	}

	sent := trans.lastSent()
	if sent == nil {
		t.Fatal("no message was sent")
	}
	pub := sent.GetPublish()
	if pub == nil {
		t.Fatal("sent message is not a Publish envelope")
	}
	if !pub.GetTransient() {
		t.Fatal("Transient flag was not forwarded")
	}
	if pub.GetChannel() != "ch" {
		t.Fatalf("wrong channel: %q, want %q", pub.GetChannel(), "ch")
	}

	// Default (no variadic argument) must not set the transient flag.
	if err := c.Publish("ch", msg); err != nil {
		t.Fatalf("Publish without transient failed: %v", err)
	}
	sent = trans.lastSent()
	if sent == nil || sent.GetPublish() == nil {
		t.Fatal("second message was not sent as a Publish envelope")
	}
	if sent.GetPublish().GetTransient() {
		t.Fatal("Transient flag should default to false")
	}
}

// TestClientRPCCloseRace reproduces the P0-4 double close panic: an RPC
// issued in a goroutine while the main goroutine immediately closes the
// client. The RPC must return an error (not hang) and the process must not
// panic from closing the pending channel twice.
func TestClientRPCCloseRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		trans := newFakeTransport()
		ctx, cancel := context.WithCancel(context.Background())
		c := newClient(ctx, cancel, trans, defaultOptions())
		c.connected.Store(true)

		req := NewMessageWithData("messageloop.rpc", NewTextData("ping"))
		done := make(chan error, 1)
		go func() {
			done <- c.RPC(context.Background(), "svc.echo", "echo", req, nil)
		}()

		// Wait until the RPC is registered as pending so that Close's
		// cleanup deterministically races with RPC's deferred cleanup.
		deadline := time.Now().Add(5 * time.Second)
		for {
			c.pendingRPCMu.RLock()
			n := len(c.pendingRPC)
			c.pendingRPCMu.RUnlock()
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("RPC did not register as pending")
			}
			time.Sleep(time.Millisecond)
		}

		if err := c.Close(); err != nil {
			cancel()
			t.Fatalf("Close failed: %v", err)
		}

		select {
		case err := <-done:
			if err == nil {
				cancel()
				t.Fatal("RPC succeeded after Close, want error")
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("RPC hung after Close")
		}
		cancel()
	}
}

// TestClientRPCReceivesReply verifies the receiveLoop delivery path still
// routes an RpcReply to the pending RPC after the rpcPending change.
func TestClientRPCReceivesReply(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	req := NewMessageWithData("messageloop.rpc", NewTextData("ping"))
	done := make(chan error, 1)
	go func() {
		done <- c.RPC(context.Background(), "svc.echo", "echo", req, nil)
	}()

	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if sent := trans.lastSent(); sent != nil && sent.GetId() != "" {
			id = sent.GetId()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("RPC request was not sent")
	}

	trans.push(BuildRPCReplyMessage(id, NewMessageWithData("messageloop.reply", NewTextData("pong"))))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RPC failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RPC timed out waiting for reply")
	}
}

// TestClientRPCErrorEnvelopeByID reproduces P1-10: an Error envelope carrying
// the pending RPC request ID must be routed to the RPC caller so the call
// fails fast with the server error instead of hanging until the context
// deadline.
func TestClientRPCErrorEnvelopeByID(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	req := NewMessageWithData("messageloop.rpc", NewTextData("ping"))
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rpcCancel()
	done := make(chan error, 1)
	go func() {
		done <- c.RPC(rpcCtx, "svc.echo", "echo", req, nil)
	}()

	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if sent := trans.lastSent(); sent != nil && sent.GetId() != "" {
			id = sent.GetId()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("RPC request was not sent")
	}

	trans.push(&clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "RPC_FAILED",
				Type:    "rpc_error",
				Message: "boom",
			},
		},
	})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RPC succeeded, want server error")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("RPC returned wrong error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RPC timed out, error envelope not routed to pending RPC")
	}

	// An Error envelope that does not match a pending RPC must still reach
	// the global error handler.
	handlerErr := make(chan error, 1)
	c.OnError(func(err error) { handlerErr <- err })
	trans.push(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "GENERIC",
				Type:    "server_error",
				Message: "generic failure",
			},
		},
	})
	select {
	case err := <-handlerErr:
		if !strings.Contains(err.Error(), "generic failure") {
			t.Fatalf("error handler received unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("error handler was not called for unmatched error envelope")
	}
}

// TestClientReconnectSendFailureClosesTransport reproduces P2-9: when the
// Connect send fails during a reconnect attempt, the freshly created
// transport must be closed instead of leaking a connection.
func TestClientReconnectSendFailureClosesTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failing := newFakeTransport()
	failing.sendErr = fmt.Errorf("send boom")

	c := newClient(ctx, cancel, newFakeTransport(), defaultOptions())
	c.newTransport = func() (transport, error) { return failing, nil }

	err := c.reconnect()
	if err == nil {
		t.Fatal("reconnect succeeded, want send error")
	}
	if !strings.Contains(err.Error(), "send boom") {
		t.Fatalf("unexpected error: %v", err)
	}

	failing.mu.Lock()
	closed := failing.closed
	failing.mu.Unlock()
	if !closed {
		t.Fatal("failed reconnect transport was not closed")
	}
}

// TestClientReconnectTimeoutClosesTransport reproduces P2-9: when a reconnect
// attempt times out waiting for Connected, the new transport must be closed
// and its receiveLoop must observe the close and exit instead of hanging on
// the stream.
func TestClientReconnectTimeoutClosesTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trans := newFakeTransport()
	c := newClient(ctx, cancel, newFakeTransport(), defaultOptions())
	c.newTransport = func() (transport, error) { return trans, nil }
	c.connectTimeout = 100 * time.Millisecond

	recvErr := make(chan error, 1)
	c.OnError(func(err error) { recvErr <- err })

	err := c.reconnect()
	if err == nil {
		t.Fatal("reconnect succeeded, want timeout error")
	}
	if !strings.Contains(err.Error(), "reconnect timeout") {
		t.Fatalf("unexpected error: %v", err)
	}

	trans.mu.Lock()
	closed := trans.closed
	trans.mu.Unlock()
	if !closed {
		t.Fatal("timed-out reconnect transport was not closed")
	}

	// The receiveLoop spawned for the new transport must observe the close
	// and exit instead of leaking on the stream.
	select {
	case err := <-recvErr:
		if !strings.Contains(err.Error(), "receive error") {
			t.Fatalf("unexpected receive error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiveLoop did not observe the transport close")
	}
}

// TestClientStaleConnectedDropped reproduces P2-9: a Connected response that
// arrives late on a superseded connection (older generation) must be dropped
// so it cannot reset the reconnecting flag or overwrite the session state.
func TestClientStaleConnectedDropped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldTrans := newFakeTransport()
	c := newClient(ctx, cancel, oldTrans, defaultOptions())
	c.reconnecting.Store(true)
	// The current connection is generation 1; oldTrans belongs to the
	// superseded generation 0 and its receiveLoop is still delivering.
	c.generation.Store(1)

	go c.receiveLoop(oldTrans, 0)

	oldTrans.push(BuildConnectedMessage("ghost-session", nil))

	// Wait until the stale Connected has been consumed and processed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if oldTrans.pendingCount() == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if oldTrans.pendingCount() != 0 {
		t.Fatal("stale Connected was never consumed")
	}

	if !c.reconnecting.Load() {
		t.Fatal("stale Connected reset the reconnecting flag")
	}
	if got := c.SessionID(); got != "" {
		t.Fatalf("stale Connected overwrote session ID: %q", got)
	}

	// A Connected from the current generation must still be processed.
	newTrans := newFakeTransport()
	go c.receiveLoop(newTrans, 1)
	newTrans.push(BuildConnectedMessage("current-session", nil))

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.SessionID() == "current-session" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := c.SessionID(); got != "current-session" {
		t.Fatalf("current-generation Connected was not processed, session ID = %q", got)
	}
	if c.reconnecting.Load() {
		t.Fatal("matching Connected did not clear the reconnecting flag")
	}
}

// TestClientTransportSwapRace reproduces P2-9: concurrent Publish calls and
// reconnect attempts must not race on the transport field (validated by
// running with -race).
func TestClientTransportSwapRace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, newFakeTransport(), defaultOptions())
	c.connected.Store(true)
	c.newTransport = func() (transport, error) { return newFakeTransport(), nil }
	c.connectTimeout = time.Millisecond

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := NewMessageWithData("ch", NewTextData("hi"))
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.Publish("ch", msg)
			}
		}()
	}

	var rwg sync.WaitGroup
	for w := 0; w < 2; w++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			for i := 0; i < 150; i++ {
				if err := c.reconnect(); err == nil {
					t.Errorf("reconnect succeeded without a Connected response")
				}
			}
		}()
	}
	rwg.Wait()
	close(stop)
	wg.Wait()
}
