package messageloopgo

import (
	"context"
	"strings"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// sentCount returns the number of messages recorded by the fake transport.
func sentCount(trans *fakeTransport) int {
	trans.mu.Lock()
	defer trans.mu.Unlock()
	return len(trans.sent)
}

// sentAt returns the i-th recorded message.
func sentAt(trans *fakeTransport, i int) *clientpb.InboundMessage {
	trans.mu.Lock()
	defer trans.mu.Unlock()
	return trans.sent[i]
}

// awaitSubscribeAck drives one waiting Subscribe call against the fake
// transport: call runs in a goroutine, the helper waits for the Subscribe
// message issued by this call (index-based, so back-to-back calls are safe),
// pushes the matching SubscribeAck echoing the request id, and fails the test
// if the call does not complete with the ack. receiveLoop must be running.
func awaitSubscribeAck(t *testing.T, trans *fakeTransport, call func() error) *clientpb.InboundMessage {
	t.Helper()
	n := sentCount(trans)
	done := make(chan error, 1)
	go func() { done <- call() }()

	deadline := time.Now().Add(5 * time.Second)
	for sentCount(trans) <= n {
		if time.Now().After(deadline) {
			t.Fatal("Subscribe message was not sent")
		}
		time.Sleep(time.Millisecond)
	}
	sent := sentAt(trans, n)

	trans.push(&clientpb.OutboundMessage{
		Id: sent.GetId(),
		Envelope: &clientpb.OutboundMessage_SubscribeAck{
			SubscribeAck: &clientpb.SubscribeAck{
				Subscriptions: sent.GetSubscribe().GetSubscriptions(),
			},
		},
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe hung waiting for the SubscribeAck")
	}
	return sent
}

// awaitUnsubscribeAck is awaitSubscribeAck for Unsubscribe.
func awaitUnsubscribeAck(t *testing.T, trans *fakeTransport, call func() error) *clientpb.InboundMessage {
	t.Helper()
	n := sentCount(trans)
	done := make(chan error, 1)
	go func() { done <- call() }()

	deadline := time.Now().Add(5 * time.Second)
	for sentCount(trans) <= n {
		if time.Now().After(deadline) {
			t.Fatal("Unsubscribe message was not sent")
		}
		time.Sleep(time.Millisecond)
	}
	sent := sentAt(trans, n)

	trans.push(&clientpb.OutboundMessage{
		Id: sent.GetId(),
		Envelope: &clientpb.OutboundMessage_UnsubscribeAck{
			UnsubscribeAck: &clientpb.UnsubscribeAck{
				Subscriptions: sent.GetUnsubscribe().GetSubscriptions(),
			},
		},
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Unsubscribe failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Unsubscribe hung waiting for the UnsubscribeAck")
	}
	return sent
}

// pendingSubAckCount returns the number of pending subscribe acks.
func pendingSubAckCount(c *client) int {
	c.pendingSubAckMu.RLock()
	defer c.pendingSubAckMu.RUnlock()
	return len(c.pendingSubAck)
}

// pendingUnsubAckCount returns the number of pending unsubscribe acks.
func pendingUnsubAckCount(c *client) int {
	c.pendingUnsubAckMu.RLock()
	defer c.pendingUnsubAckMu.RUnlock()
	return len(c.pendingUnsubAck)
}

// TestClientSubscribeWaitsForSubscribeAck verifies the subscription contract:
// Subscribe does not return when the Subscribe message has merely been
// written to the transport — it returns only after the server's SubscribeAck
// (echoing the request id) has been delivered, so a publish issued after
// Subscribe returns can no longer race the subscription registration.
func TestClientSubscribeWaitsForSubscribeAck(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() { done <- c.Subscribe("chat.room1") }()

	// Wait for the Subscribe message, then deliberately delay the ack: the
	// call must still be in flight (and registered as pending).
	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if sent := trans.lastSent(); sent != nil && sent.GetSubscribe() != nil && sent.GetId() != "" {
			id = sent.GetId()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("Subscribe message was not sent")
	}
	if n := pendingSubAckCount(c); n != 1 {
		t.Fatalf("pending subscribes = %d, want 1", n)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Subscribe returned before the SubscribeAck: %v", err)
	default:
	}

	// Late ack: only now may the call complete.
	trans.push(&clientpb.OutboundMessage{
		Id:       id,
		Envelope: &clientpb.OutboundMessage_SubscribeAck{SubscribeAck: &clientpb.SubscribeAck{}},
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe hung after the SubscribeAck arrived")
	}
	if n := pendingSubAckCount(c); n != 0 {
		t.Fatalf("pending subscribe not cleaned after ack: %d entries", n)
	}
}

// TestClientSubscribeWithWaitsForSubscribeAck verifies SubscribeWith shares
// the acked Subscribe contract.
func TestClientSubscribeWithWaitsForSubscribeAck(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	awaitSubscribeAck(t, trans, func() error {
		return c.SubscribeWith("chat.room1", WithEphemeral(true))
	})
}

// TestClientSubscribeAckTimeout verifies that a SubscribeAck which never
// arrives rejects the call with the ack timeout error and cleans up the
// pending (small RPCTimeout to keep the test fast).
func TestClientSubscribeAckTimeout(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := defaultOptions()
	opts.RPCTimeout = 80 * time.Millisecond
	c := newClient(ctx, cancel, trans, opts)
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	start := time.Now()
	err := c.Subscribe("chat.room1")
	if err == nil {
		t.Fatal("Subscribe succeeded without a SubscribeAck, want timeout")
	}
	if !strings.Contains(err.Error(), "subscribe ack timeout") {
		t.Fatalf("error = %v, want subscribe ack timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Subscribe hung past the ack timeout")
	}
	if n := pendingSubAckCount(c); n != 0 {
		t.Fatalf("pending subscribe not cleaned after timeout: %d entries", n)
	}
}

// TestClientSubscribeAsyncDoesNotWait verifies SubscribeAsync keeps the
// fire-and-forget semantics: it returns as soon as the Subscribe message is
// written, registers no pending, and a missing ack is not an error.
func TestClientSubscribeAsyncDoesNotWait(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	start := time.Now()
	if err := c.SubscribeAsync("chat.room1"); err != nil {
		t.Fatalf("SubscribeAsync failed: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("SubscribeAsync waited instead of returning immediately")
	}
	// No ack is ever pushed; no pending may have been registered.
	if n := pendingSubAckCount(c); n != 0 {
		t.Fatalf("SubscribeAsync registered a pending: %d entries", n)
	}
	sent := trans.lastSent()
	if sent == nil || sent.GetSubscribe() == nil {
		t.Fatal("SubscribeAsync did not send a Subscribe message")
	}
}

// TestClientUnsubscribeWaitsForUnsubscribeAck verifies the unsubscribe side
// of the contract: Unsubscribe returns only after the UnsubscribeAck, and the
// acked channel state write-back has removed the subscription.
func TestClientUnsubscribeWaitsForUnsubscribeAck(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	// Seed the subscription state (what the server would have acked earlier).
	c.handleSubscribeAck(&clientpb.SubscribeAck{
		Subscriptions: []*clientpb.Subscription{{Channel: "chat.room1"}},
	})

	done := make(chan error, 1)
	go func() { done <- c.Unsubscribe("chat.room1") }()

	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if sent := trans.lastSent(); sent != nil && sent.GetUnsubscribe() != nil && sent.GetId() != "" {
			id = sent.GetId()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("Unsubscribe message was not sent")
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Unsubscribe returned before the UnsubscribeAck: %v", err)
	default:
	}

	trans.push(&clientpb.OutboundMessage{
		Id:       id,
		Envelope: &clientpb.OutboundMessage_UnsubscribeAck{UnsubscribeAck: &clientpb.UnsubscribeAck{}},
	})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Unsubscribe failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Unsubscribe hung after the UnsubscribeAck arrived")
	}
	if n := pendingUnsubAckCount(c); n != 0 {
		t.Fatalf("pending unsubscribe not cleaned after ack: %d entries", n)
	}
}

// TestClientUnsubscribeAckTimeout verifies the ack timeout for Unsubscribe.
func TestClientUnsubscribeAckTimeout(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := defaultOptions()
	opts.RPCTimeout = 80 * time.Millisecond
	c := newClient(ctx, cancel, trans, opts)
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	err := c.Unsubscribe("chat.room1")
	if err == nil {
		t.Fatal("Unsubscribe succeeded without an UnsubscribeAck, want timeout")
	}
	if !strings.Contains(err.Error(), "unsubscribe ack timeout") {
		t.Fatalf("error = %v, want unsubscribe ack timeout", err)
	}
	if n := pendingUnsubAckCount(c); n != 0 {
		t.Fatalf("pending unsubscribe not cleaned after timeout: %d entries", n)
	}
}

// TestClientUnsubscribeAsyncDoesNotWait verifies UnsubscribeAsync keeps the
// fire-and-forget semantics.
func TestClientUnsubscribeAsyncDoesNotWait(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	start := time.Now()
	if err := c.UnsubscribeAsync("chat.room1"); err != nil {
		t.Fatalf("UnsubscribeAsync failed: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("UnsubscribeAsync waited instead of returning immediately")
	}
	if n := pendingUnsubAckCount(c); n != 0 {
		t.Fatalf("UnsubscribeAsync registered a pending: %d entries", n)
	}
	sent := trans.lastSent()
	if sent == nil || sent.GetUnsubscribe() == nil {
		t.Fatal("UnsubscribeAsync did not send an Unsubscribe message")
	}
}

// TestClientSubscribeDisconnectRejectsPending verifies that a lost connection
// fails all in-flight subscribes so callers can retry instead of hanging
// until the ack timeout (mirrors the pendingPublish disconnect cleanup).
func TestClientSubscribeDisconnectRejectsPending(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	c.opts.AutoReconnect = false
	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() { done <- c.Subscribe("chat.room1") }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if pendingSubAckCount(c) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Subscribe did not register as pending")
		}
		time.Sleep(time.Millisecond)
	}

	// Kill the transport: the receive loop must observe the failure and
	// reject the pending subscribe.
	trans.mu.Lock()
	trans.closed = true
	close(trans.closeCh)
	trans.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Subscribe succeeded after disconnect, want error")
		}
		if !strings.Contains(err.Error(), "connection lost before subscribe ack") {
			t.Fatalf("error = %v, want connection-lost rejection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe hung after disconnect")
	}
	if n := pendingSubAckCount(c); n != 0 {
		t.Fatalf("pending subscribe not cleaned after disconnect: %d entries", n)
	}
}

// TestClientUnsubscribeDisconnectRejectsPending verifies the same
// disconnect cleanup for in-flight unsubscribes.
func TestClientUnsubscribeDisconnectRejectsPending(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	c.opts.AutoReconnect = false
	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() { done <- c.Unsubscribe("chat.room1") }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if pendingUnsubAckCount(c) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Unsubscribe did not register as pending")
		}
		time.Sleep(time.Millisecond)
	}

	trans.mu.Lock()
	trans.closed = true
	close(trans.closeCh)
	trans.mu.Unlock()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Unsubscribe succeeded after disconnect, want error")
		}
		if !strings.Contains(err.Error(), "connection lost before unsubscribe ack") {
			t.Fatalf("error = %v, want connection-lost rejection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Unsubscribe hung after disconnect")
	}
	if n := pendingUnsubAckCount(c); n != 0 {
		t.Fatalf("pending unsubscribe not cleaned after disconnect: %d entries", n)
	}
}

// TestClientSubscribeCloseRejectsPending verifies Close() rejects in-flight
// subscribes, mirroring the pendingRPC/pendingAck cleanup.
func TestClientSubscribeCloseRejectsPending(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() { done <- c.Subscribe("chat.room1") }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if pendingSubAckCount(c) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Subscribe did not register as pending")
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
			t.Fatal("Subscribe succeeded after Close, want error")
		}
		if !strings.Contains(err.Error(), "client closed before subscribe ack") {
			t.Fatalf("error = %v, want client-closed rejection", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Subscribe hung after Close")
	}
	cancel()
}

// TestClientSubscribeErrorEnvelopeFailsFast verifies a top-level Error
// envelope referencing the subscribe request id fails the call fast with the
// server error instead of waiting out the ack timeout.
func TestClientSubscribeErrorEnvelopeFailsFast(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() { done <- c.Subscribe("chat.room1") }()

	deadline := time.Now().Add(5 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if sent := trans.lastSent(); sent != nil && sent.GetSubscribe() != nil && sent.GetId() != "" {
			id = sent.GetId()
			break
		}
		time.Sleep(time.Millisecond)
	}
	if id == "" {
		t.Fatal("Subscribe message was not sent")
	}

	trans.push(&clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "PERMISSION_DENIED",
				Type:    "acl_error",
				Message: "subscribe boom",
			},
		},
	})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Subscribe succeeded, want server error")
		}
		if !strings.Contains(err.Error(), "subscribe boom") {
			t.Fatalf("error = %v, want the server error message", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe hung, error envelope not routed to the pending subscribe")
	}
	if n := pendingSubAckCount(c); n != 0 {
		t.Fatalf("pending subscribe not cleaned after error: %d entries", n)
	}
}
