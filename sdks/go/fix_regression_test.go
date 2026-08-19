package messageloopgo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	proxypb "github.com/messageloopio/messageloop/shared/genproto/proxy/v2"
)

// TestClientHandleConnectedCloseRace reproduces P0-2: handleConnected and
// Close() racing on connectedCh must not panic from closing a nil channel.
// Run with -race.
func TestClientHandleConnectedCloseRace(t *testing.T) {
	// Deterministic regression: Close() nilled connectedCh between
	// handleConnected's closed-flag check and its channel read — the signal
	// step must not close(nil).
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	c := newClient(ctx, cancel, trans, defaultOptions())
	c.generation.Store(1)
	c.mu.Lock()
	c.connectedCh = nil
	c.mu.Unlock()
	c.handleConnected(&clientpb.Connected{SessionId: "s"}, 1)
	cancel()

	// Concurrent stress: race handleConnected against Close; with -race this
	// also validates the shared-state synchronization.
	for i := 0; i < 2000; i++ {
		trans := newFakeTransport()
		ctx, cancel := context.WithCancel(context.Background())
		c := newClient(ctx, cancel, trans, defaultOptions())
		c.generation.Store(1)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.handleConnected(&clientpb.Connected{
				SessionId:     "s",
				Subscriptions: []*clientpb.Subscription{{Channel: "ch"}},
			}, 1)
		}()
		go func() {
			defer wg.Done()
			_ = c.Close()
		}()
		wg.Wait()
		cancel()
	}
}

// TestClientPongTimeoutClosesTransport drives P1-F1: when no pong arrives
// within PingTimeout after a ping, the ping loop must close the transport so
// the receive loop observes the failure and the reconnect flow can take over.
func TestClientPongTimeoutClosesTransport(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := defaultOptions()
	opts.PingInterval = 30 * time.Millisecond
	opts.PingTimeout = 20 * time.Millisecond
	opts.AutoReconnect = false

	c := newClient(ctx, cancel, trans, opts)
	c.connected.Store(true)

	recvErr := make(chan error, 1)
	c.OnError(func(err error) { recvErr <- err })

	go c.receiveLoop(trans, 0)
	c.startPingLoop()

	// No pong is ever delivered; the transport must be closed by the pong
	// timeout and the receive loop must observe the failure.
	deadline := time.Now().Add(5 * time.Second)
	closed := false
	for time.Now().Before(deadline) {
		trans.mu.Lock()
		closed = trans.closed
		trans.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !closed {
		t.Fatal("transport was not closed after pong timeout")
	}

	select {
	case err := <-recvErr:
		if !strings.Contains(err.Error(), "receive error") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiveLoop did not observe the transport close")
	}
}

// TestClientPongKeepsConnectionAlive verifies that a connection delivering
// regular pongs is never closed by the pong timeout.
func TestClientPongKeepsConnectionAlive(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := defaultOptions()
	opts.PingInterval = 60 * time.Millisecond
	opts.PingTimeout = 40 * time.Millisecond

	c := newClient(ctx, cancel, trans, opts)
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)
	c.startPingLoop()

	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			trans.push(&clientpb.OutboundMessage{Envelope: &clientpb.OutboundMessage_Pong{Pong: &clientpb.Pong{}}})
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)

	trans.mu.Lock()
	closed := trans.closed
	trans.mu.Unlock()
	if closed {
		t.Fatal("transport closed despite regular pongs")
	}
}

// TestClientConnectSendFailureClosesTransport verifies P1-F4: a failed
// Connect send must close the transport instead of leaking it.
func TestClientConnectSendFailureClosesTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bad := newFakeTransport()
	bad.sendErr = fmt.Errorf("connect boom")

	c := newClient(ctx, cancel, bad, defaultOptions())
	c.connectTimeout = 200 * time.Millisecond

	err := c.Connect(ctx)
	if err == nil || !strings.Contains(err.Error(), "connect boom") {
		t.Fatalf("Connect error = %v, want send failure", err)
	}

	bad.mu.Lock()
	closed := bad.closed
	bad.mu.Unlock()
	if !closed {
		t.Fatal("failed Connect did not close the transport")
	}
	if c.generation.Load() == 0 {
		t.Fatal("generation was not advanced by Connect")
	}
}

// TestClientConnectRetryAdvancesGeneration verifies P1-F4/F5: a retried
// Connect dials a fresh transport and advances the generation, so no stale
// gen-0 receive loop can survive and the retry completes normally.
func TestClientConnectRetryAdvancesGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bad := newFakeTransport()
	bad.sendErr = fmt.Errorf("connect boom")

	c := newClient(ctx, cancel, bad, defaultOptions())
	c.connectTimeout = 200 * time.Millisecond

	if err := c.Connect(ctx); err == nil {
		t.Fatal("first Connect succeeded, want failure")
	}

	good := newFakeTransport()
	c.newTransport = func() (transport, error) { return good, nil }

	done := make(chan error, 1)
	go func() { done <- c.Connect(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		good.mu.Lock()
		n := len(good.sent)
		good.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	good.push(BuildConnectedMessage("sess-2", nil))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Connect failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second Connect hung")
	}
	if c.SessionID() != "sess-2" {
		t.Fatalf("session id = %q, want sess-2", c.SessionID())
	}
	if c.generation.Load() < 2 {
		t.Fatalf("generation = %d, want >= 2", c.generation.Load())
	}
}

// TestClientConnectAfterReconnect verifies P1-F5: a manual Connect issued
// after an auto-reconnect (generation > 0) must advance the generation, carry
// session resumption data, and complete instead of hanging because its
// Connected message is dropped as stale.
func TestClientConnectAfterReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldTrans := newFakeTransport()
	c := newClient(ctx, cancel, oldTrans, defaultOptions())
	c.connectTimeout = time.Second

	// Simulate a prior auto-reconnect cycle.
	c.generation.Store(3)
	c.mu.Lock()
	c.sessionID = "old-session"
	c.epoch = "ep-1"
	c.mu.Unlock()
	c.subMu.Lock()
	c.subscriptions["ch1"] = &subscriptionState{ephemeral: true}
	c.subMu.Unlock()

	trans := newFakeTransport()
	c.newTransport = func() (transport, error) { return trans, nil }

	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		trans.mu.Lock()
		n := len(trans.sent)
		trans.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	connectMsg := trans.lastSent().GetConnect()
	if connectMsg == nil {
		t.Fatal("no Connect message sent")
	}
	if connectMsg.GetSessionId() != "old-session" {
		t.Fatalf("SessionId = %q, want old-session", connectMsg.GetSessionId())
	}
	subs := connectMsg.GetSubscriptions()
	if len(subs) != 1 || subs[0].GetChannel() != "ch1" {
		t.Fatalf("resume subscriptions = %v, want [ch1]", subs)
	}
	if !subs[0].GetRecover() {
		t.Fatal("resume subscription missing Recover=true")
	}

	trans.push(BuildConnectedMessage("new-session", nil))

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Connect after reconnect failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Connect after reconnect hung — Connected dropped by generation mismatch")
	}
	if c.SessionID() != "new-session" {
		t.Fatalf("session id = %q, want new-session", c.SessionID())
	}
}

// TestClientSupersededLoopDoesNotReconnect verifies that a receive loop bound
// to a superseded transport exits silently when its transport is closed by a
// newer Connect: no error callbacks and no reconnect attempt.
func TestClientSupersededLoopDoesNotReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	oldTrans := newFakeTransport()
	c := newClient(ctx, cancel, oldTrans, defaultOptions())
	c.opts.AutoReconnect = true

	// A newer Connect replaced the transport.
	newTrans := newFakeTransport()
	c.mu.Lock()
	c.transport = newTrans
	c.mu.Unlock()
	c.generation.Store(2)

	errs := make(chan error, 1)
	c.OnError(func(err error) { errs <- err })

	go c.receiveLoop(oldTrans, 1)
	_ = oldTrans.Close()

	select {
	case err := <-errs:
		t.Fatalf("superseded receiveLoop triggered the error handler: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if c.reconnecting.Load() {
		t.Fatal("superseded receiveLoop started a reconnection")
	}
}

// TestClientConnectedResumedWritesSubscriptions verifies P1-F7: the server's
// authoritative subscription list must be written back even for resumed
// sessions, so cluster-snapshot channels survive the next reconnect.
func TestClientConnectedResumedWritesSubscriptions(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	c.handleConnected(&clientpb.Connected{
		SessionId: "s1",
		Resumed:   true,
		Subscriptions: []*clientpb.Subscription{
			{Channel: "resumed-ch", Ephemeral: true},
		},
	}, 0)

	c.subMu.RLock()
	_, ok := c.subscriptions["resumed-ch"]
	eph := false
	if state := c.subscriptions["resumed-ch"]; state != nil {
		eph = state.ephemeral
	}
	c.subMu.RUnlock()
	if !ok {
		t.Fatal("resumed session subscriptions were not written back")
	}
	if !eph {
		t.Fatal("ephemeral flag was not written back")
	}
}

// TestClientUnsubscribeClearsOffsets verifies P1-F9: unsubscribing must drop
// the channel's recovery offset so a later re-subscribe cannot resume from a
// stale offset and re-deliver history from the unsubscribed period.
func TestClientUnsubscribeClearsOffsets(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.offsetMu.Lock()
	c.channelOffsets["ch"] = 42
	c.offsetMu.Unlock()

	c.handleUnsubscribeAck(&clientpb.UnsubscribeAck{
		Subscriptions: []*clientpb.Subscription{{Channel: "ch"}},
	})

	c.offsetMu.RLock()
	_, ok := c.channelOffsets["ch"]
	c.offsetMu.RUnlock()
	if ok {
		t.Fatal("offset not cleared on unsubscribe")
	}
}

// TestClientSubscribeWithEphemeral verifies P1-F3: per-subscription ephemeral
// flags flow through SubscribeWith, default to false for Subscribe, and
// survive the reconnect resume path.
func TestClientSubscribeWithEphemeral(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	if err := c.SubscribeWith("presence.ch", WithEphemeral(true)); err != nil {
		t.Fatalf("SubscribeWith failed: %v", err)
	}
	sub := trans.lastSent().GetSubscribe()
	if sub == nil {
		t.Fatal("no Subscribe message sent")
	}
	if len(sub.GetSubscriptions()) != 1 || !sub.GetSubscriptions()[0].GetEphemeral() {
		t.Fatalf("ephemeral flag not forwarded: %v", sub.GetSubscriptions())
	}

	// Plain Subscribe must keep the default (non-ephemeral).
	if err := c.Subscribe("persistent.ch"); err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}
	sub = trans.lastSent().GetSubscribe()
	if sub.GetSubscriptions()[0].GetEphemeral() {
		t.Fatal("Subscribe defaulted to ephemeral")
	}

	// Ephemeral flags must survive a reconnect resume.
	c.handleSubscribeAck(&clientpb.SubscribeAck{
		Subscriptions: []*clientpb.Subscription{
			{Channel: "presence.ch", Ephemeral: true},
			{Channel: "persistent.ch", Ephemeral: false},
		},
	})
	got := map[string]bool{}
	for _, s := range c.resumeSubscriptions("ep") {
		got[s.GetChannel()] = s.GetEphemeral()
	}
	if !got["presence.ch"] {
		t.Fatal("ephemeral flag lost on resume")
	}
	if got["persistent.ch"] {
		t.Fatal("non-ephemeral channel resumed as ephemeral")
	}
}

// TestClientSurveyRequestDefaultEcho verifies that a SurveyRequest with no
// user handler is answered with an echo reply, matching server semantics.
func TestClientSurveyRequestDefaultEcho(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: "survey-1",
				Payload:   newTestTextPayload(t, "ping"),
			},
		},
	}, 0)

	deadline := time.Now().Add(2 * time.Second)
	var reply *clientpb.InboundMessage
	for time.Now().Before(deadline) {
		reply = trans.lastSent()
		if reply != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if reply == nil {
		t.Fatal("no survey reply sent")
	}
	sr := reply.GetSurveyReply()
	if sr == nil {
		t.Fatal("reply is not a SurveyReply envelope")
	}
	if sr.GetRequestId() != "survey-1" {
		t.Fatalf("reply request id = %q, want survey-1", sr.GetRequestId())
	}
	if sr.GetPayload() == nil || sr.GetPayload().GetText() != "ping" {
		t.Fatalf("reply payload = %v, want echo of ping", sr.GetPayload())
	}
}

// TestClientSurveyCustomHandlerAndSubRefresh verifies the survey callback,
// the survey error path, SubRefresh sending, and that the new envelopes never
// crash the message dispatcher.
func TestClientSurveyCustomHandlerAndSubRefresh(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	// Custom handler payload path.
	got := make(chan string, 1)
	c.OnSurvey(func(requestID string, req *Message) (*Message, error) {
		got <- requestID
		return NewMessageWithData("resp", NewTextData("pong-"+requestID)), nil
	})
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{RequestId: "s1"},
		},
	}, 0)
	select {
	case id := <-got:
		if id != "s1" {
			t.Fatalf("handler request id = %q, want s1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("survey handler not invoked")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if last := trans.lastSent(); last != nil && last.GetSurveyReply() != nil && last.GetSurveyReply().GetRequestId() == "s1" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if last := trans.lastSent(); last.GetSurveyReply().GetPayload().GetText() != "pong-s1" {
		t.Fatalf("handler reply payload = %v, want pong-s1", last.GetSurveyReply().GetPayload())
	}

	// Survey handler error path: the error is carried in the reply.
	c.OnSurvey(func(requestID string, req *Message) (*Message, error) {
		return nil, fmt.Errorf("survey boom")
	})
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{RequestId: "s2"},
		},
	}, 0)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if last := trans.lastSent(); last != nil && last.GetSurveyReply() != nil && last.GetSurveyReply().GetRequestId() == "s2" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if last := trans.lastSent(); last.GetSurveyReply().GetError() == nil || last.GetSurveyReply().GetError().GetMessage() != "survey boom" {
		t.Fatalf("error reply = %v, want survey boom", last.GetSurveyReply().GetError())
	}

	// SubRefreshAck and SurveyReply envelopes must not crash the dispatcher.
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SubRefreshAck{SubRefreshAck: &clientpb.SubRefreshAck{}},
	}, 0)
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyReply{SurveyReply: &clientpb.SurveyReply{RequestId: "x"}},
	}, 0)

	// SubRefresh sends a SubRefresh message.
	if err := c.SubRefresh(ctx, "ch1", "ch2"); err != nil {
		t.Fatalf("SubRefresh failed: %v", err)
	}
	last := trans.lastSent()
	if last.GetSubRefresh() == nil {
		t.Fatal("SubRefresh did not send a SubRefresh envelope")
	}
	if len(last.GetSubRefresh().GetChannels()) != 2 || last.GetSubRefresh().GetChannels()[0] != "ch1" {
		t.Fatalf("sub refresh channels = %v, want [ch1 ch2]", last.GetSubRefresh().GetChannels())
	}
}

// TestClientRPCDefaultTimeout verifies the default RPC timeout is applied when
// the caller's context has no deadline.
func TestClientRPCDefaultTimeout(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := defaultOptions()
	opts.RPCTimeout = 100 * time.Millisecond
	c := newClient(ctx, cancel, trans, opts)
	c.connected.Store(true)

	req := NewMessageWithData("messageloop.rpc", NewTextData("ping"))
	start := time.Now()
	err := c.RPC(context.Background(), "svc.echo", "echo", req, nil)
	if err == nil {
		t.Fatal("RPC succeeded, want default timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RPC error = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("RPC hung past the default timeout")
	}
}

// --- Proxy nil-response guards (P1-F6) ---

type stubNilHandler struct{}

func (h *stubNilHandler) HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
	return nil, nil
}

func TestHandlerImplRPCNilResponse(t *testing.T) {
	h := &HandlerImpl{}
	h.RPCHandler = &stubNilHandler{}

	resp, err := h.RPC(context.Background(), &proxypb.RPCRequest{Id: "1", Channel: "svc", Method: "echo"})
	if err == nil {
		t.Fatal("RPC succeeded, want Internal error for nil response")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("RPC error = %v, want Internal", err)
	}
	if resp != nil {
		t.Fatalf("resp = %v, want nil on error", resp)
	}
}

type stubNilAuthHandler struct{}

func (h *stubNilAuthHandler) Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error) {
	return nil, nil
}

func TestHandlerImplAuthNilResponse(t *testing.T) {
	h := &HandlerImpl{}
	h.AuthHandler = &stubNilAuthHandler{}

	resp, err := h.Authenticate(context.Background(), &proxypb.AuthenticateRequest{ClientId: "c1"})
	if err == nil || status.Code(err) != codes.Internal {
		t.Fatalf("Authenticate = (%v, %v), want Internal error", resp, err)
	}
}

// --- Lifecycle handler params (P1-F8, breaking change) ---

func TestHandlerImplLifecycleSubscribedParams(t *testing.T) {
	h := &HandlerImpl{}
	calls := make(chan []string, 2)
	h.LifecycleHandler = &recordingLifecycleHandler{calls: calls}

	_, err := h.OnSubscribed(context.Background(), &proxypb.OnSubscribedRequest{
		SessionId: "s1", Channel: "ch1", Username: "u1",
	})
	if err != nil {
		t.Fatalf("OnSubscribed failed: %v", err)
	}
	select {
	case got := <-calls:
		if len(got) != 3 || got[0] != "s1" || got[1] != "ch1" || got[2] != "u1" {
			t.Fatalf("OnSubscribed params = %v, want [s1 ch1 u1]", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle handler not called")
	}

	_, err = h.OnUnsubscribed(context.Background(), &proxypb.OnUnsubscribedRequest{
		SessionId: "s2", Channel: "ch2", Username: "u2",
	})
	if err != nil {
		t.Fatalf("OnUnsubscribed failed: %v", err)
	}
	select {
	case got := <-calls:
		if len(got) != 3 || got[0] != "s2" || got[1] != "ch2" || got[2] != "u2" {
			t.Fatalf("OnUnsubscribed params = %v, want [s2 ch2 u2]", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle handler not called")
	}
}

type recordingLifecycleHandler struct {
	calls chan []string
}

func (h *recordingLifecycleHandler) OnConnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *recordingLifecycleHandler) OnDisconnected(ctx context.Context, sessionID, username string) error {
	return nil
}

func (h *recordingLifecycleHandler) OnSubscribed(ctx context.Context, sessionID, channel, username string) error {
	h.calls <- []string{sessionID, channel, username}
	return nil
}

func (h *recordingLifecycleHandler) OnUnsubscribed(ctx context.Context, sessionID, channel, username string) error {
	h.calls <- []string{sessionID, channel, username}
	return nil
}

// TestClientConnectCloseDuringWait verifies P1-F6: a Connect blocked in its
// select while Close() closes connectErrCh/connectedCh must return a
// "client closed" error instead of a zero-value nil receive that falsely
// reports success. Run with -race.
func TestClientConnectCloseDuringWait(t *testing.T) {
	for i := 0; i < 50; i++ {
		trans := newFakeTransport()
		ctx, cancel := context.WithCancel(context.Background())

		c := newClient(ctx, cancel, trans, defaultOptions())
		c.connectTimeout = 5 * time.Second

		done := make(chan error, 1)
		go func() { done <- c.Connect(context.Background()) }()

		// Wait until Connect has sent the connect message and is blocked in
		// its select, then close the client underneath it.
		deadline := time.Now().Add(2 * time.Second)
		for {
			trans.mu.Lock()
			n := len(trans.sent)
			trans.mu.Unlock()
			if n > 0 {
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("Connect did not send the connect message")
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(10 * time.Millisecond)

		if err := c.Close(); err != nil {
			cancel()
			t.Fatalf("Close failed: %v", err)
		}

		select {
		case err := <-done:
			if err == nil {
				cancel()
				t.Fatal("Connect succeeded after Close, want error")
			}
			if !strings.Contains(err.Error(), "client closed") && !strings.Contains(err.Error(), "timeout") {
				cancel()
				t.Fatalf("Connect error = %v, want client closed", err)
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("Connect hung after Close")
		}
		cancel()
	}
}
