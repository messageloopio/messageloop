package messageloopgo

import (
	"context"
	"strings"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// waitSent polls the fake transport until a message matching pred is sent and
// returns it. Fails the test on timeout.
func waitSent(t *testing.T, trans *fakeTransport, pred func(*clientpb.InboundMessage) bool) *clientpb.InboundMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msg := trans.lastSent(); msg != nil && pred(msg) {
			return msg
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no matching message was sent")
	return nil
}

// TestSDK_SubscribeWithRecover verifies WithRecover encodes recover=true plus
// the optional v2 Position cursor, and that WithFresh marks an explicit
// from-the-start replay. No API form treats "offset 0" as from the start.
func TestSDK_SubscribeWithRecover(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	if err := c.SubscribeWith("chat.recover", WithRecover(Position("ep", 7))); err != nil {
		t.Fatalf("SubscribeWith failed: %v", err)
	}
	sub := trans.lastSent().GetSubscribe()
	if sub == nil || len(sub.GetSubscriptions()) != 1 {
		t.Fatal("no Subscribe message with one subscription sent")
	}
	s := sub.GetSubscriptions()[0]
	if !s.GetRecover() {
		t.Fatal("recover flag not set")
	}
	if s.GetCursor() == nil || s.GetCursor().GetStreamEpoch() != "ep" || s.GetCursor().GetOffset() != 7 {
		t.Fatalf("cursor = %v, want epoch ep offset 7", s.GetCursor())
	}

	// A no-hint recover (nil cursor) still sends recover=true without a
	// cursor and never claims fresh.
	if err := c.SubscribeWith("chat.nohint", WithRecover(nil)); err != nil {
		t.Fatalf("SubscribeWith failed: %v", err)
	}
	s = trans.lastSent().GetSubscribe().GetSubscriptions()[0]
	if !s.GetRecover() {
		t.Fatal("recover flag not set for no-hint recover")
	}
	if s.GetFresh() {
		t.Fatal("no-hint recover must not set fresh")
	}

	// WithFresh forces an explicit from-the-start replay.
	if err := c.SubscribeWith("chat.fresh", WithFresh()); err != nil {
		t.Fatalf("SubscribeWith failed: %v", err)
	}
	s = trans.lastSent().GetSubscribe().GetSubscriptions()[0]
	if !s.GetRecover() || !s.GetFresh() {
		t.Fatal("fresh subscription must set recover=true and fresh=true")
	}
}

// TestSDK_RecoverySinglePath verifies §5: replay and live publications both
// reach the same OnMessage handler, the cursor advances only from
// RecoverComplete.position (replay) and live Message.position (never from the
// ack or from replay batches), and recoverResults are never read.
func TestSDK_RecoverySinglePath(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	got := make(chan []*Message, 4)
	c.OnMessage(func(msgs []*Message) { got <- msgs })

	// A replay publication (replay=true) is delivered to the very same
	// OnMessage handler as a live one, but must not advance the cursor
	// mid-replay: only the RecoverComplete position is authoritative.
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{
				{Id: "m1", Channel: "chat.recover", Replay: true, Position: Position("ep", 42), Payload: newTestTextPayload(t, "replayed")},
			}},
		},
	}, 0)

	select {
	case msgs := <-got:
		if len(msgs) != 1 || msgs[0].String() != "replayed" {
			t.Fatalf("OnMessage got %v, want the replayed message", msgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnMessage was not called for the replay publication")
	}

	c.offsetMu.RLock()
	_, advanced := c.channelOffsets["chat.recover"]
	c.offsetMu.RUnlock()
	if advanced {
		t.Fatal("replay publication advanced the cursor before RecoverComplete")
	}

	// RecoverComplete echoes the authoritative position.
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_RecoverComplete{
			RecoverComplete: &clientpb.RecoverComplete{Channel: "chat.recover", Position: Position("ep", 42)},
		},
	}, 0)
	c.offsetMu.RLock()
	off := c.channelOffsets["chat.recover"]
	c.offsetMu.RUnlock()
	if off != 42 {
		t.Fatalf("channel offset = %d, want 42", off)
	}

	// A live publication (replay=false) hits the same handler and advances
	// the cursor from Message.position.
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{
				{Id: "m2", Channel: "chat.recover", Position: Position("ep", 43), Payload: newTestTextPayload(t, "live")},
			}},
		},
	}, 0)

	select {
	case msgs := <-got:
		if len(msgs) != 1 || msgs[0].String() != "live" {
			t.Fatalf("OnMessage got %v, want the live message", msgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnMessage was not called for the live publication")
	}

	c.offsetMu.RLock()
	off = c.channelOffsets["chat.recover"]
	c.offsetMu.RUnlock()
	if off != 43 {
		t.Fatalf("live message must advance the cursor: %d, want 43", off)
	}
}

// TestSDK_PresenceEvent verifies an outbound presence event reaches OnPresence
// with channel/session/user/client.
func TestSDK_PresenceEvent(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	got := make(chan PresenceEvent, 1)
	c.OnPresence(func(ev PresenceEvent) { got <- ev })

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_PresenceEvent{
			PresenceEvent: &clientpb.PresenceEvent{
				Channel: "room.x",
				Action:  "join",
				Info: &clientpb.PresenceInfo{
					SessionId:   "s1",
					UserId:      "u1",
					ClientId:    "c1",
					ConnectedAt: 1234,
				},
			},
		},
	}, 0)

	select {
	case ev := <-got:
		if ev.Channel != "room.x" || ev.Action != "join" {
			t.Fatalf("event = %+v, want channel room.x action join", ev)
		}
		if ev.Info.SessionID != "s1" || ev.Info.UserID != "u1" || ev.Info.ClientID != "c1" || ev.Info.ConnectedAt != 1234 {
			t.Fatalf("event info = %+v, want s1/u1/c1/1234", ev.Info)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnPresence was not called")
	}
}

// TestSDK_PresenceSnapshotPushedAfterConnected verifies presence snapshots are
// delivered as separate Presence envelopes (v2 has no presence list on
// Connected), and that a RecoverComplete with an unset position never creates
// or wipes a per-channel cursor.
func TestSDK_PresenceSnapshotPushedAfterConnected(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	got := make(chan PresenceSnapshot, 1)
	c.OnPresenceSnapshot(func(snap PresenceSnapshot) { got <- snap })

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_Presence{
			Presence: &clientpb.PresenceSnapshot{
				Channel:   "room.x",
				Occupancy: 3,
				Truncated: true,
				Clients: []*clientpb.PresenceInfo{
					{SessionId: "s1", UserId: "u1"},
					{SessionId: "s2", UserId: "u2"},
				},
			},
		},
	}, 0)

	select {
	case snap := <-got:
		if snap.Channel != "room.x" || !snap.Truncated || snap.Occupancy != 3 {
			t.Fatalf("snapshot = %+v, want room.x truncated occupancy 3", snap)
		}
		if len(snap.Clients) != 2 || snap.Clients[0].UserID != "u1" || snap.Clients[1].SessionID != "s2" {
			t.Fatalf("clients = %+v, want [u1 s2]", snap.Clients)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnPresenceSnapshot was not called")
	}

	// A RecoverComplete echoing an unset position must not create or wipe a
	// cursor: an unset position is never "0 means from the start".
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_RecoverComplete{
			RecoverComplete: &clientpb.RecoverComplete{Channel: "chat.unset", Position: &sharedpb.Position{}},
		},
	}, 0)
	c.offsetMu.RLock()
	_, unsetKept := c.channelOffsets["chat.unset"]
	c.offsetMu.RUnlock()
	if unsetKept {
		t.Fatal("unset RecoverComplete position must not create a cursor")
	}

	// A set RecoverComplete position is authoritative even when the replay
	// batch was empty.
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_RecoverComplete{
			RecoverComplete: &clientpb.RecoverComplete{Channel: "chat.empty", Position: Position("ep", 77)},
		},
	}, 0)
	c.offsetMu.RLock()
	emptyOff := c.channelOffsets["chat.empty"]
	c.offsetMu.RUnlock()
	if emptyOff != 77 {
		t.Fatalf("RecoverComplete position offset = %d, want 77", emptyOff)
	}
}

// TestSDK_PresenceSnapshotOnSubscribeAck verifies SubscribeAck.presence
// snapshots dispatch OnPresenceSnapshot after the subscription write-back.
func TestSDK_PresenceSnapshotOnSubscribeAck(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	got := make(chan PresenceSnapshot, 1)
	c.OnPresenceSnapshot(func(snap PresenceSnapshot) { got <- snap })

	c.handleSubscribeAck(&clientpb.SubscribeAck{
		Subscriptions: []*clientpb.Subscription{{Channel: "room.x"}},
		Presence: []*clientpb.PresenceSnapshot{{
			Channel:   "room.x",
			Occupancy: 2,
			Clients:   []*clientpb.PresenceInfo{{SessionId: "s1", UserId: "u1"}},
		}},
	})

	select {
	case snap := <-got:
		if snap.Channel != "room.x" || snap.Occupancy != 2 {
			t.Fatalf("snapshot = %+v, want room.x occupancy 2", snap)
		}
		if len(snap.Clients) != 1 || snap.Clients[0].SessionID != "s1" {
			t.Fatalf("clients = %+v, want [s1]", snap.Clients)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnPresenceSnapshot was not called for SubscribeAck")
	}
}

// TestSDK_PresenceQuery verifies the Presence round trip: the query is sent
// with the channel, the same-id snapshot reply returns the expected values,
// and the reply also fires OnPresenceSnapshot.
func TestSDK_PresenceQuery(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	snaps := make(chan PresenceSnapshot, 1)
	c.OnPresenceSnapshot(func(snap PresenceSnapshot) { snaps <- snap })

	type result struct {
		snap *PresenceSnapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, err := c.Presence(context.Background(), "room.x")
		done <- result{snap: snap, err: err}
	}()

	sent := waitSent(t, trans, func(m *clientpb.InboundMessage) bool { return m.GetPresenceQuery() != nil })
	if sent.GetPresenceQuery().GetChannel() != "room.x" {
		t.Fatalf("query channel = %q, want room.x", sent.GetPresenceQuery().GetChannel())
	}

	trans.push(&clientpb.OutboundMessage{
		Id: sent.GetId(),
		Envelope: &clientpb.OutboundMessage_Presence{
			Presence: &clientpb.PresenceSnapshot{
				Channel:   "room.x",
				Occupancy: 5,
				Truncated: true,
				Clients: []*clientpb.PresenceInfo{
					{SessionId: "s1", UserId: "u1", ClientId: "c1", ConnectedAt: 9},
				},
			},
		},
	})

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Presence failed: %v", res.err)
		}
		if res.snap == nil || res.snap.Channel != "room.x" || res.snap.Occupancy != 5 || !res.snap.Truncated {
			t.Fatalf("snapshot = %+v, want room.x occupancy 5 truncated", res.snap)
		}
		if len(res.snap.Clients) != 1 || res.snap.Clients[0].SessionID != "s1" || res.snap.Clients[0].UserID != "u1" {
			t.Fatalf("clients = %+v, want [s1 u1]", res.snap.Clients)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Presence hung")
	}

	select {
	case snap := <-snaps:
		if snap.Occupancy != 5 {
			t.Fatalf("OnPresenceSnapshot occupancy = %d, want 5", snap.Occupancy)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnPresenceSnapshot was not called for the query reply")
	}
}

// TestSDK_PresenceDisconnectFails verifies a lost transport fails an
// in-flight Presence query instead of leaving it hanging until ctx timeout.
func TestSDK_PresenceDisconnectFails(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.Presence(context.Background(), "room.x")
		done <- err
	}()

	_ = waitSent(t, trans, func(m *clientpb.InboundMessage) bool { return m.GetPresenceQuery() != nil })
	_ = trans.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Presence succeeded after disconnect, want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Presence hung after disconnect")
	}
}

// TestSDK_PresenceQueryDenied verifies a same-id top-level error fails the
// Presence call instead of hanging.
func TestSDK_PresenceQueryDenied(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.Presence(context.Background(), "room.x")
		done <- err
	}()

	sent := waitSent(t, trans, func(m *clientpb.InboundMessage) bool { return m.GetPresenceQuery() != nil })

	trans.push(&clientpb.OutboundMessage{
		Id: sent.GetId(),
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "PERMISSION_DENIED",
				Type:    "acl_error",
				Message: "presence query denied",
			},
		},
	})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Presence succeeded, want error")
		}
		if !strings.Contains(err.Error(), "PERMISSION_DENIED") {
			t.Fatalf("Presence error = %v, want PERMISSION_DENIED", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Presence hung on denied query")
	}
}

// TestSDK_SurveyRoundTrip verifies Survey sends a SurveyRequest with the
// channel / request_id / timeout and that the same-request_id SurveyResult
// returns answers with user_id from metadata.
func TestSDK_SurveyRoundTrip(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	type result struct {
		answers []SurveyAnswer
		err     error
	}
	done := make(chan result, 1)
	go func() {
		answers, err := c.Survey(context.Background(), "chat.x",
			NewMessageWithData("q", NewTextData("ping")), 1500*time.Millisecond)
		done <- result{answers: answers, err: err}
	}()

	sent := waitSent(t, trans, func(m *clientpb.InboundMessage) bool { return m.GetSurveyRequest() != nil })
	sr := sent.GetSurveyRequest()
	if sr.GetChannel() != "chat.x" {
		t.Fatalf("survey channel = %q, want chat.x", sr.GetChannel())
	}
	if sr.GetRequestId() == "" {
		t.Fatal("survey request_id is empty")
	}
	if sr.GetTimeoutMs() != 1500 {
		t.Fatalf("survey timeout_ms = %d, want 1500", sr.GetTimeoutMs())
	}
	if sr.GetPayload() == nil || sr.GetPayload().GetText() != "ping" {
		t.Fatalf("survey payload = %v, want ping", sr.GetPayload())
	}

	trans.push(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyResult{
			SurveyResult: &clientpb.SurveyResult{
				RequestId: sr.GetRequestId(),
				Channel:   "chat.x",
				Answers: []*clientpb.SurveyAnswer{
					{
						SessionId: "s-1",
						Metadata:  &sharedpb.Metadata{Entries: map[string]string{"user_id": "u-1"}},
						Payload:   newTestTextPayload(t, "pong"),
					},
				},
			},
		},
	})

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Survey failed: %v", res.err)
		}
		if len(res.answers) != 1 {
			t.Fatalf("answers = %d, want 1", len(res.answers))
		}
		a := res.answers[0]
		if a.SessionID != "s-1" || a.UserID != "u-1" {
			t.Fatalf("answer = %+v, want session s-1 user u-1", a)
		}
		if a.Payload == nil || a.Payload.String() != "pong" {
			t.Fatalf("answer payload = %v, want pong", a.Payload)
		}
		if a.Error != nil {
			t.Fatalf("answer error = %v, want nil", a.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Survey hung")
	}
}

// TestSDK_SurveyTopError verifies a same-id top-level survey rejection fails
// the Survey call without hanging.
func TestSDK_SurveyTopError(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.Survey(context.Background(), "chat.x",
			NewMessageWithData("q", NewTextData("ping")), 0)
		done <- err
	}()

	sent := waitSent(t, trans, func(m *clientpb.InboundMessage) bool { return m.GetSurveyRequest() != nil })

	trans.push(&clientpb.OutboundMessage{
		Id: sent.GetId(),
		Envelope: &clientpb.OutboundMessage_Error{
			Error: &sharedpb.Error{
				Code:    "SURVEY_DISABLED",
				Type:    "policy_error",
				Message: "survey disabled by channel policy",
			},
		},
	})

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Survey succeeded, want error")
		}
		if !strings.Contains(err.Error(), "SURVEY_DISABLED") {
			t.Fatalf("Survey error = %v, want SURVEY_DISABLED", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Survey hung on top-level error")
	}
}

// TestSDK_OnSurveyCompat verifies that with only the legacy OnSurvey handler
// registered, an outbound SurveyRequest still produces a SurveyReply through
// the legacy signature.
func TestSDK_OnSurveyCompat(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	c.OnSurvey(func(requestID string, req *Message) (*Message, error) {
		return NewMessageWithData("resp", NewTextData("pong-"+requestID)), nil
	})
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: "s1",
				Channel:   "chat.x",
				Payload:   newTestTextPayload(t, "ping"),
			},
		},
	}, 0)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if last := trans.lastSent(); last != nil && last.GetSurveyReply() != nil && last.GetSurveyReply().GetRequestId() == "s1" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	last := trans.lastSent()
	if last == nil || last.GetSurveyReply() == nil || last.GetSurveyReply().GetPayload() == nil {
		t.Fatal("no survey reply sent")
	}
	if last.GetSurveyReply().GetPayload().GetText() != "pong-s1" {
		t.Fatalf("reply payload = %v, want pong-s1", last.GetSurveyReply().GetPayload())
	}
}

// TestSDK_OnSurveyRequestChannel verifies OnSurveyRequest receives the
// outbound channel and that the reply carries the correct request_id.
func TestSDK_OnSurveyRequestChannel(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	got := make(chan [2]string, 1)
	c.OnSurveyRequest(func(requestID, channel string, req *Message) (*Message, error) {
		got <- [2]string{requestID, channel}
		return NewMessageWithData("resp", NewTextData("reply-"+requestID)), nil
	})
	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_SurveyRequest{
			SurveyRequest: &clientpb.SurveyRequest{
				RequestId: "s1",
				Channel:   "chat.x",
				Payload:   newTestTextPayload(t, "ping"),
			},
		},
	}, 0)

	select {
	case ids := <-got:
		if ids[0] != "s1" || ids[1] != "chat.x" {
			t.Fatalf("handler args = %v, want [s1 chat.x]", ids)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnSurveyRequest not invoked")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if last := trans.lastSent(); last != nil && last.GetSurveyReply() != nil && last.GetSurveyReply().GetRequestId() == "s1" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	last := trans.lastSent()
	if last == nil || last.GetSurveyReply() == nil {
		t.Fatal("no survey reply sent")
	}
	if last.GetSurveyReply().GetPayload().GetText() != "reply-s1" {
		t.Fatalf("reply payload = %v, want reply-s1", last.GetSurveyReply().GetPayload())
	}
}

// TestSDK_ServerPingPong verifies an outbound Ping is answered with an inbound
// Pong carrying the same id.
func TestSDK_ServerPingPong(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())
	c.connected.Store(true)

	go c.receiveLoop(trans, 0)
	trans.push(&clientpb.OutboundMessage{
		Id:       "ping-1",
		Envelope: &clientpb.OutboundMessage_Ping{Ping: &clientpb.Ping{}},
	})

	sent := waitSent(t, trans, func(m *clientpb.InboundMessage) bool { return m.GetPong() != nil })
	if sent.GetId() != "ping-1" {
		t.Fatalf("pong id = %q, want ping-1", sent.GetId())
	}
}

// TestSDK_ServerPingKeepsAlive verifies that a connection receiving only
// server pings (and no pongs) is not killed by the client's own pong timeout:
// each server ping counts as liveness evidence.
func TestSDK_ServerPingKeepsAlive(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := defaultOptions()
	opts.PingInterval = 30 * time.Millisecond
	opts.PingTimeout = 20 * time.Millisecond
	opts.AutoReconnect = false

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
			trans.push(&clientpb.OutboundMessage{
				Id:       "sping",
				Envelope: &clientpb.OutboundMessage_Ping{Ping: &clientpb.Ping{}},
			})
			time.Sleep(8 * time.Millisecond)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)

	trans.mu.Lock()
	closed := trans.closed
	trans.mu.Unlock()
	if closed {
		t.Fatal("transport closed by pong timeout despite server pings")
	}
}
