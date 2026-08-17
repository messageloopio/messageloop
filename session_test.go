package messageloop

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedTransport is a test Transport whose Write behavior is scripted:
// it records every write (in order), optionally returns a fixed error, and
// can block on a gate so the session's writer goroutine stalls.
type scriptedTransport struct {
	mu       sync.Mutex
	writes   [][]byte
	closeReasons []Disconnect
	writeErr error
	probeErr error
	// gate blocks Write until closed (for queue-fill tests).
	gate chan struct{}
}

func newScriptedTransport() *scriptedTransport {
	return &scriptedTransport{gate: make(chan struct{})}
}

func (t *scriptedTransport) Write(data []byte) error {
	<-t.gate
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.writeErr != nil {
		return t.writeErr
	}
	t.writes = append(t.writes, append([]byte(nil), data...))
	return nil
}

func (t *scriptedTransport) WriteMany(data ...[]byte) error {
	if t.probeErr != nil {
		return t.probeErr
	}
	for _, d := range data {
		if err := t.Write(d); err != nil {
			return err
		}
	}
	return nil
}

func (t *scriptedTransport) Close(disconnect Disconnect) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeReasons = append(t.closeReasons, disconnect)
	return nil
}

func (t *scriptedTransport) RemoteAddr() string { return "127.0.0.1:1" }

// unblock releases the write gate (test sync point, not a sleep).
func (t *scriptedTransport) unblock() {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.gate:
	default:
		close(t.gate)
	}
}

func (t *scriptedTransport) record() []Disconnect {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Disconnect(nil), t.closeReasons...)
}

func (t *scriptedTransport) writeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.writes)
}

func (t *scriptedTransport) writeEnvelope(i int) *clientpb.OutboundMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out clientpb.OutboundMessage
	_ = ProtoJSONMarshaler.Unmarshal(t.writes[i], &out)
	return &out
}

// --- §9.7: the state machine is pinned from NewClient onward ---

func TestSession_NewClient_StateAuthenticating(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)

	sess, _, err := NewClient(ctx, node, newScriptedTransport(), JSONMarshaler{})
	require.NoError(t, err)
	assert.Equal(t, SessionAuthenticating, sess.State(),
		"NewClient must start the session at Authenticating, never the zero value")

	// Detach on a non-Attached session is a no-op and must not change state.
	sess.Detach(Disconnect{})
	assert.Equal(t, SessionAuthenticating, sess.State())

	// Attach moves to Attached; Detach opens the local handover window.
	att := &Attachment{Transport: sess.attachment.Transport, Marshaler: JSONMarshaler{}, Protocol: "ws"}
	require.NoError(t, sess.Attach(att))
	assert.Equal(t, SessionAttached, sess.State())

	sess.Detach(Disconnect{})
	assert.Equal(t, SessionDetached, sess.State(), "Detach must be observable in the handover window")
}

// --- §9.8: Control wins the next-frame selection ---

func TestSession_SendQueue_ControlBeforeData(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := newScriptedTransport()
	transport.unblock()
	sess, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	dataMsg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{{Id: "m1"}}},
		}
	})
	controlMsg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{Pong: &clientpb.Pong{}}
	})

	// Enqueue Data first, Control second — both while the writer is not yet
	// running (Authenticating), so both are pending before the writer starts.
	// The control flag comes from the real envelope classifier.
	dataFrame, err := frameFor(dataMsg)
	require.NoError(t, err)
	controlFrame, err := frameFor(controlMsg)
	require.NoError(t, err)
	require.NoError(t, sess.out.tryEnqueue(dataFrame))
	require.NoError(t, sess.out.tryEnqueue(controlFrame))
	assert.False(t, dataFrame.control, "Publication must classify as Data")
	assert.True(t, controlFrame.control, "Pong must classify as Control")

	// Start the writer: the next-frame selection must pick Control first.
	att := &Attachment{Transport: transport, Marshaler: JSONMarshaler{}, Protocol: "ws"}
	require.NoError(t, sess.Attach(att))

	// Sync points: each frame's done channel fires when it hits the wire.
	select {
	case err := <-controlFrame.done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Control frame never written")
	}
	select {
	case err := <-dataFrame.done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Data frame never written")
	}
	require.Equal(t, 2, transport.writeCount())
	first := transport.writeEnvelope(0)
	second := transport.writeEnvelope(1)
	assert.NotNil(t, first.GetPong(), "write order: Control (Pong) must hit the transport first")
	assert.NotNil(t, second.GetPublication(), "write order: Data (Publication) second")
}

// frameFor marshals an outbound message into a queued frame, classifying it
// with the production envelope classifier.
func frameFor(msg *clientpb.OutboundMessage) (*queuedFrame, error) {
	buf := getBuffer()
	defer putBuffer(buf)
	var err error
	*buf, err = ProtoJSONMarshaler.MarshalAppend((*buf)[:0], msg)
	if err != nil {
		return nil, err
	}
	return &queuedFrame{
		bytes:   append([]byte(nil), (*buf)...),
		control: outboundFrameClass(msg),
		done:    make(chan error, 1),
	}, nil
}

// --- §9.8: a full Data lane closes the session with 3512 ---

func TestSession_SendQueue_DataFullClosesSlowConsumer(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := newScriptedTransport()
	sess, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	att := &Attachment{Transport: transport, Marshaler: JSONMarshaler{}, Protocol: "ws"}
	require.NoError(t, sess.Attach(att))

	dataMsg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{{Id: "m"}}},
		}
	})

	// The writer picks up the first frame and blocks on the transport; the
	// next 256 frames fill the Data lane, so the frame after that must fail
	// the enqueue with ErrSendQueueFull, which closes the session with 3512.
	// The state transition is the sync point (no fixed sleeps).
	results := make([]chan error, 1+sendQueueDataDepth+1)
	for i := range results {
		results[i] = make(chan error, 1)
		go func(ch chan<- error) { ch <- sess.Send(ctx, dataMsg) }(results[i])
	}

	// The session must be closed with DisconnectSlowConsumer (3512) — "Data
	// 满 → 3512，关 Session" (§7) — and the blocked writer must not double
	// close (its attachment was already cleared by Close).
	require.Eventually(t, func() bool { return sess.State() == SessionClosed }, 5*time.Second, time.Millisecond)
	reasons := transport.record()
	require.NotEmpty(t, reasons, "the transport must be closed")
	assert.Equal(t, DisconnectSlowConsumer.Code, reasons[0].Code, "Data-lane full must close with 3512")

	// The remaining writers all finish (failed) once the queue is closed.
	transport.unblock()
	for _, ch := range results {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("a blocked send never completed after the queue was closed")
		}
	}
}

// --- §9.8: a full Control lane also closes the session with 3512 ---

func TestSession_SendQueue_ControlFullClosesSlowConsumer(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := newScriptedTransport()
	sess, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	att := &Attachment{Transport: transport, Marshaler: JSONMarshaler{}, Protocol: "ws"}
	require.NoError(t, sess.Attach(att))

	controlMsg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{Pong: &clientpb.Pong{}}
	})

	// 33 Pongs cannot fit in the 32-deep Control lane: the overflow must
	// fail the enqueue and close the session with 3512 (if they were
	// misclassified as Data, 33 would fit in the 256-deep Data lane).
	results := make([]chan error, 1+sendQueueControlDepth+1)
	for i := range results {
		results[i] = make(chan error, 1)
		go func(ch chan<- error) { ch <- sess.Send(ctx, controlMsg) }(results[i])
	}

	require.Eventually(t, func() bool { return sess.State() == SessionClosed }, 5*time.Second, time.Millisecond)
	reasons := transport.record()
	require.NotEmpty(t, reasons)
	assert.Equal(t, DisconnectSlowConsumer.Code, reasons[0].Code, "Control-lane full must close with 3512")

	transport.unblock()
	for _, ch := range results {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatal("a blocked send never completed after the queue was closed")
		}
	}
}

// --- §9.9: io.EOF from Write is peer_closed (3000), never 3512 ---

func TestSession_WriteEOF_ClosesWith3000(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := newScriptedTransport()
	transport.writeErr = io.EOF
	transport.unblock()

	sess, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	att := &Attachment{Transport: transport, Marshaler: JSONMarshaler{}, Protocol: "ws"}
	require.NoError(t, sess.Attach(att))

	dataMsg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{
			Publication: &clientpb.Publication{Messages: []*clientpb.Message{{Id: "m"}}},
		}
	})
	require.ErrorIs(t, sess.Send(ctx, dataMsg), io.EOF, "the caller must observe the write error")

	require.Eventually(t, func() bool { return sess.State() == SessionClosed }, time.Second, time.Millisecond)
	reasons := transport.record()
	require.NotEmpty(t, reasons)
	assert.Equal(t, DisconnectConnectionClosed.Code, reasons[0].Code,
		"io.EOF must map to peer_closed (3000), never 3512")
}

// --- §9.4: Attach failure after Detach falls through to a real Close ---

func TestSession_AttachFailure_ClosesSession(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	goodTransport := newScriptedTransport()
	goodTransport.unblock()
	sess, _, err := NewClient(ctx, node, goodTransport, JSONMarshaler{})
	require.NoError(t, err)
	sess.ForceTestIDs("sess-attach-fail", "user-1", "client-1")
	require.NoError(t, node.AddClient(sess))
	require.NoError(t, node.AddSubscription(ctx, "track.ch", Subscriber{Session: sess, Ephemeral: false}))
	node.presenceJoin(ctx, "track.ch", sess)
	present, err := node.presence.Get(ctx, "track.ch")
	require.NoError(t, err)
	require.Contains(t, present, "sess-attach-fail")

	// Detach (handover window opened), then Attach a broken attachment: the
	// probe fails synchronously.
	sess.Detach(Disconnect{})
	require.Equal(t, SessionDetached, sess.State())
	broken := newScriptedTransport()
	broken.probeErr = errors.New("transport dead")
	err = sess.Attach(&Attachment{Transport: broken, Marshaler: JSONMarshaler{}, Protocol: "ws"})
	require.Error(t, err, "Attach must fail when the transport is unusable")

	// The spec flow (§6): the caller closes the session for real — the
	// directory must not stay held by a session with no attachment.
	_ = sess.Close(DisconnectInternal)
	assert.Equal(t, SessionClosed, sess.State())
	assert.Nil(t, node.hub.LookupSession("sess-attach-fail"), "the hub must not hold the session")
	assert.Zero(t, node.hub.NumSubscribers("track.ch"), "subscriptions must be removed")
	present, err = node.presence.Get(ctx, "track.ch")
	require.NoError(t, err)
	assert.Empty(t, present, "presence must be left on the real close")
}

// --- §9.5: Fence removes local state but never leaves presence or unbinds ---

func TestSession_Fence_NoLeaveNoUnbind(t *testing.T) {
	ctx := context.Background()
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	_ = node.Run(ctx)

	transport := newScriptedTransport()
	transport.unblock()
	sess, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	sess.ForceTestIDs("sess-fence", "user-1", "client-1")
	require.NoError(t, node.AddClient(sess))
	require.NoError(t, node.AddSubscription(ctx, "fence.ch", Subscriber{Session: sess, Ephemeral: false}))
	node.presenceJoin(ctx, "fence.ch", sess)

	require.NoError(t, sess.Fence(DisconnectStale))

	assert.Equal(t, SessionClosed, sess.State())
	assert.Nil(t, node.hub.LookupSession("sess-fence"), "the hub must not hold the session after Fence")
	assert.Zero(t, node.hub.NumSubscribers("fence.ch"), "subscriptions must be removed after Fence")
	present, err := node.presence.Get(ctx, "fence.ch")
	require.NoError(t, err)
	assert.Contains(t, present, "sess-fence", "Fence must NOT leave presence: the new owner serves the session")
	require.False(t, directory.deletedLease, "Fence must not unbind the directory lease")
	require.False(t, directory.deletedSnapshot, "Fence must not delete the directory snapshot")
}

// --- §9.6: Close really leaves and unbinds ---

func TestSession_Close_LeavesAndUnbinds(t *testing.T) {
	ctx := context.Background()
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	_ = node.Run(ctx)

	transport := newScriptedTransport()
	transport.unblock()
	sess, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	sess.ForceTestIDs("sess-close", "user-1", "client-1")
	require.NoError(t, node.AddClient(sess))
	require.NoError(t, node.AddSubscription(ctx, "close.ch", Subscriber{Session: sess, Ephemeral: false}))
	node.presenceJoin(ctx, "close.ch", sess)

	require.NoError(t, sess.Close(Disconnect{}))

	assert.Equal(t, SessionClosed, sess.State())
	assert.Nil(t, node.hub.LookupSession("sess-close"))
	assert.Zero(t, node.hub.NumSubscribers("close.ch"))
	present, err := node.presence.Get(ctx, "close.ch")
	require.NoError(t, err)
	assert.Empty(t, present, "Close must leave presence")
	require.True(t, directory.deletedLease, "Close must unbind the directory lease")
	require.True(t, directory.deletedSnapshot, "Close must delete the directory snapshot")

	// Idempotency: a second Close is a no-op.
	require.NoError(t, sess.Close(Disconnect{}))
}

// TestSession_OutboundFrameClass_GapNotice verifies C6: the GapNotice
// envelope rides the Control lane (small, low-frequency, control semantics),
// like the other control envelopes.
func TestSession_OutboundFrameClass_GapNotice(t *testing.T) {
	notice := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_GapNotice{
			GapNotice: &clientpb.GapNotice{Channel: "ch"},
		}
	})
	require.True(t, outboundFrameClass(notice), "GapNotice must be a Control frame")

	pub := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Publication{Publication: &clientpb.Publication{}}
	})
	require.False(t, outboundFrameClass(pub), "publications stay on the Data lane")
}
