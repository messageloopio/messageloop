package messageloopgo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// TestClientSubscribeWithToken verifies WithSubscriptionToken flows into the
// outbound Subscribe message, plain Subscribe stays token-free, and
// Unsubscribe reads the token back symmetrically with the ephemeral flag.
func TestClientSubscribeWithToken(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	if err := c.SubscribeWith("secure.ch", WithSubscriptionToken("sub-token-1")); err != nil {
		t.Fatalf("SubscribeWith failed: %v", err)
	}
	sub := trans.lastSent().GetSubscribe()
	if sub == nil {
		t.Fatal("no Subscribe message sent")
	}
	if len(sub.GetSubscriptions()) != 1 || sub.GetSubscriptions()[0].GetToken() != "sub-token-1" {
		t.Fatalf("token not forwarded: %v", sub.GetSubscriptions())
	}

	// Plain Subscribe must keep the default (no token).
	if err := c.Subscribe("plain.ch"); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	sub = trans.lastSent().GetSubscribe()
	if sub.GetSubscriptions()[0].GetToken() != "" {
		t.Fatal("plain Subscribe carried a token")
	}

	// Unsubscribe must read the token back like it reads back the ephemeral
	// flag. Record the subscription state first (the server echoes it in the
	// SubscribeAck).
	c.handleSubscribeAck(&clientpb.SubscribeAck{
		Subscriptions: []*clientpb.Subscription{
			{Channel: "secure.ch", Token: "sub-token-1", Ephemeral: true},
		},
	})
	if err := c.Unsubscribe("secure.ch"); err != nil {
		t.Fatalf("Unsubscribe failed: %v", err)
	}
	un := trans.lastSent().GetUnsubscribe()
	if un == nil {
		t.Fatal("no Unsubscribe message sent")
	}
	if len(un.GetSubscriptions()) != 1 {
		t.Fatalf("unsubscribe subscriptions = %v, want 1 entry", un.GetSubscriptions())
	}
	if un.GetSubscriptions()[0].GetToken() != "sub-token-1" {
		t.Fatalf("unsubscribe token = %q, want sub-token-1", un.GetSubscriptions()[0].GetToken())
	}
	if !un.GetSubscriptions()[0].GetEphemeral() {
		t.Fatal("unsubscribe lost the ephemeral read-back")
	}
}

// TestClientResumeSubscriptionsKeepsToken verifies the subscription token
// survives the reconnect resume path, and that the server-authoritative
// Connected list (which does not echo tokens) does not wipe it.
func TestClientResumeSubscriptionsKeepsToken(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	c.handleSubscribeAck(&clientpb.SubscribeAck{
		Subscriptions: []*clientpb.Subscription{
			{Channel: "tok.ch", Token: "tok-1", Ephemeral: true},
			{Channel: "plain.ch"},
		},
	})

	byChannel := func(subs []*clientpb.Subscription) map[string]*clientpb.Subscription {
		m := make(map[string]*clientpb.Subscription, len(subs))
		for _, s := range subs {
			m[s.GetChannel()] = s
		}
		return m
	}

	got := byChannel(c.resumeSubscriptions("ep"))
	if got["tok.ch"] == nil || got["tok.ch"].GetToken() != "tok-1" {
		t.Fatalf("token lost on resume: %v", got["tok.ch"])
	}
	if got["plain.ch"].GetToken() != "" {
		t.Fatalf("plain channel resumed with a token: %v", got["plain.ch"])
	}
	if !got["tok.ch"].GetRecover() {
		t.Fatal("resume subscription missing Recover=true")
	}

	// The server's Connected list is authoritative for the channel set but
	// carries no token: the local token must be preserved, and channels
	// absent from the server list must still be dropped.
	c.handleConnected(&clientpb.Connected{
		SessionId: "s1",
		Subscriptions: []*clientpb.Subscription{
			{Channel: "tok.ch"},
			{Channel: "other.ch"},
		},
	}, 0)

	got = byChannel(c.resumeSubscriptions("ep"))
	if got["tok.ch"] == nil || got["tok.ch"].GetToken() != "tok-1" {
		t.Fatalf("token wiped by Connected write-back: %v", got["tok.ch"])
	}
	if got["plain.ch"] != nil {
		t.Fatal("channel absent from the server list was not dropped")
	}
	if got["other.ch"] == nil {
		t.Fatal("server-listed channel missing from resumed subscriptions")
	}
}

// TestClientPublishWithToken verifies the optional per-publish token flows
// into the outbound Publish message, and that plain Publish stays unchanged.
func TestClientPublishWithToken(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	if err := c.PublishWith("ch", msg, WithPublishToken("pub-token-1")); err != nil {
		t.Fatalf("PublishWith failed: %v", err)
	}
	pub := trans.lastSent().GetPublish()
	if pub == nil {
		t.Fatal("no Publish message sent")
	}
	if pub.GetToken() != "pub-token-1" {
		t.Fatalf("publish token = %q, want pub-token-1", pub.GetToken())
	}
	if pub.GetChannel() != "ch" {
		t.Fatalf("publish channel = %q, want ch", pub.GetChannel())
	}

	// Plain Publish keeps its existing shape (no token).
	if err := c.Publish("ch", msg); err != nil {
		t.Fatalf("Publish failed: %v", err)
	}
	pub = trans.lastSent().GetPublish()
	if pub.GetToken() != "" {
		t.Fatal("plain Publish carried a token")
	}
}

// TestClientPublishWithAckResolve verifies the ack path: a PublishWithAck
// pending on the message id is resolved with the broker-assigned offset when
// the PublishAck arrives.
func TestClientPublishWithAckResolve(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	done := make(chan ackOutcome, 1)
	go func() {
		off, err := c.PublishWithAck(context.Background(), "ch", msg)
		done <- ackOutcome{offset: off, err: err}
	}()

	id := waitForPublish(t, trans)

	trans.push(&clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_PublishAck{
			PublishAck: &clientpb.PublishAck{Id: id, Position: Position("", 42)},
		},
	})

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("PublishWithAck failed: %v", res.err)
		}
		if res.offset != 42 {
			t.Fatalf("offset = %d, want 42", res.offset)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishWithAck timed out waiting for the ack")
	}

	if n := pendingAckCount(c); n != 0 {
		t.Fatalf("pending ack not cleaned after resolve: %d entries", n)
	}
}

// TestClientPublishWithAckTimeout verifies the context deadline rejects the
// pending publish and cleans it up.
func TestClientPublishWithAckTimeout(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	callCtx, callCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer callCancel()

	done := make(chan ackOutcome, 1)
	go func() {
		off, err := c.PublishWithAck(callCtx, "ch", msg)
		done <- ackOutcome{offset: off, err: err}
	}()

	_ = waitForPublish(t, trans)

	select {
	case res := <-done:
		if !errors.Is(res.err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want DeadlineExceeded", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishWithAck hung past the context deadline")
	}

	if n := pendingAckCount(c); n != 0 {
		t.Fatalf("pending ack not cleaned after timeout: %d entries", n)
	}
}

// TestClientPublishWithAckDisconnect verifies that a lost connection rejects
// all pending publishes so callers fail fast instead of hanging.
func TestClientPublishWithAckDisconnect(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	c.opts.AutoReconnect = false
	go c.receiveLoop(trans, 0)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	done := make(chan ackOutcome, 1)
	go func() {
		off, err := c.PublishWithAck(context.Background(), "ch", msg)
		done <- ackOutcome{offset: off, err: err}
	}()

	_ = waitForPublish(t, trans)

	trans.mu.Lock()
	trans.closed = true
	close(trans.closeCh)
	trans.mu.Unlock()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("PublishWithAck succeeded after disconnect, want error")
		}
		if !strings.Contains(res.err.Error(), "connection lost before publish ack") {
			t.Fatalf("error = %v, want connection-lost rejection", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishWithAck hung after disconnect")
	}

	if n := pendingAckCount(c); n != 0 {
		t.Fatalf("pending ack not cleaned after disconnect: %d entries", n)
	}
}

// TestClientPublishWithAckClose verifies Close() rejects pending publishes,
// mirroring the pendingRPC cleanup.
func TestClientPublishWithAckClose(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	done := make(chan ackOutcome, 1)
	go func() {
		off, err := c.PublishWithAck(context.Background(), "ch", msg)
		done <- ackOutcome{offset: off, err: err}
	}()

	// Wait until the publish is registered as pending so the Close cleanup
	// deterministically races with the caller.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if pendingAckCount(c) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("PublishWithAck did not register as pending")
		}
		time.Sleep(time.Millisecond)
	}

	if err := c.Close(); err != nil {
		cancel()
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case res := <-done:
		if res.err == nil {
			cancel()
			t.Fatal("PublishWithAck succeeded after Close, want error")
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("PublishWithAck hung after Close")
	}
	cancel()
}

// TestClientErrorEnvelopeRejectsPendingAck verifies an Error envelope
// referencing the pending publish id fails the call fast with the server
// error, mirroring the RPC error routing.
func TestClientErrorEnvelopeRejectsPendingAck(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	done := make(chan ackOutcome, 1)
	go func() {
		off, err := c.PublishWithAck(context.Background(), "ch", msg)
		done <- ackOutcome{offset: off, err: err}
	}()

	id := waitForPublish(t, trans)

	trans.push(&clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "INTERNAL_ERROR",
				Type:    "server_error",
				Message: "publish boom",
			},
		},
	})

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("PublishWithAck succeeded, want server error")
		}
		if !strings.Contains(res.err.Error(), "publish boom") {
			t.Fatalf("error = %v, want the server error message", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishWithAck hung, error envelope not routed to pending publish")
	}

	if n := pendingAckCount(c); n != 0 {
		t.Fatalf("pending ack not cleaned after error: %d entries", n)
	}
}

// TestClientPublishWithAckCarriesToken verifies that per-publish options
// (WithPublishToken) are applied to the outbound message sent by
// PublishWithAck.
func TestClientPublishWithAckCarriesToken(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)
	go c.receiveLoop(trans, 0)

	msg := NewMessageWithData("ch", NewTextData("hi"))
	done := make(chan ackOutcome, 1)
	go func() {
		off, err := c.PublishWithAck(context.Background(), "ch", msg, WithPublishToken("pub-tok"))
		done <- ackOutcome{offset: off, err: err}
	}()

	id := waitForPublish(t, trans)

	// The sent publish must carry the option.
	last := trans.lastSent()
	if last.GetPublish().GetToken() != "pub-tok" {
		t.Fatalf("publish token = %q, want pub-tok", last.GetPublish().GetToken())
	}

	trans.push(&clientpb.OutboundMessage{
		Id: id,
		Envelope: &clientpb.OutboundMessage_PublishAck{
			PublishAck: &clientpb.PublishAck{Id: id, Position: Position("", 7)},
		},
	})

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("PublishWithAck failed: %v", res.err)
		}
		if res.offset != 7 {
			t.Fatalf("offset = %d, want 7", res.offset)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PublishWithAck timed out")
	}
}

type ackOutcome struct {
	offset uint64
	err    error
}

// waitForPublish blocks until the fake transport recorded a Publish envelope
// and returns its message id.
func waitForPublish(t *testing.T, trans *fakeTransport) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sent := trans.lastSent(); sent != nil && sent.GetPublish() != nil && sent.GetId() != "" {
			return sent.GetId()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("publish was not sent")
	return ""
}

// pendingAckCount returns the number of pending publish acks.
func pendingAckCount(c *client) int {
	c.pendingAckMu.RLock()
	defer c.pendingAckMu.RUnlock()
	return len(c.pendingAck)
}
