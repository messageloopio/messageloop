package session

import (
	"context"
	"testing"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression for the stale-shell delegate close: closeFromAttachment /
// closeFromLoop used to call delegate.Close unconditionally, so after a
// chained resume (E resumed by Y, then by Z) the superseded shell Y's read
// loop exit killed the healthy session now served by Z (subscriptions torn
// down, presence left, directory unbound).

const resumeTestChannel = "resume.ch"

// newResumableSession builds a live, attached, subscribed session on a shared
// runtime, the way a completed connect leaves it.
func newResumableSession(t *testing.T, node *fakeRuntime, sessionID string) *Session {
	t.Helper()
	transport := newScriptedTransport()
	transport.unblock()
	sess, _, err := NewClient(context.Background(), node, transport, JSONMarshaler{})
	require.NoError(t, err)
	sess.ForceTestIDs(sessionID, "user-1", "client-1")
	require.NoError(t, node.AddClient(sess))
	require.NoError(t, node.AddSubscription(context.Background(), resumeTestChannel, Subscriber{Session: sess, Ephemeral: false}))
	return sess
}

// newResumeShell builds the temporary Authenticating session a fresh
// connection runs before its Connect completes, returning its per-connection
// close func (what the read loop's defer runs).
func newResumeShell(t *testing.T, node *fakeRuntime) (shell *Session, transport *scriptedTransport, closeConn func() error) {
	t.Helper()
	transport = newScriptedTransport()
	transport.unblock()
	shell, closeConn, err := NewClient(context.Background(), node, transport, JSONMarshaler{})
	require.NoError(t, err)
	return shell, transport, closeConn
}

// resumeOnto replays the local-resume takeover from handleConnect: the shell
// hands its transport to target via a fresh attachment, target is torn off
// its previous transport (Detach closes it) and rebound to the shell's, and
// the shell becomes a delegate-only read loop.
func resumeOnto(t *testing.T, shell, target *Session) {
	t.Helper()
	shell.mu.RLock()
	tempAtt := shell.attachment
	shell.mu.RUnlock()
	require.NotNil(t, tempAtt, "a shell that has not been closed must still hold its attachment")

	newAtt := &Attachment{
		Transport: tempAtt.Transport,
		Marshaler: tempAtt.Marshaler,
		Protocol:  tempAtt.Protocol,
	}
	target.Detach(Disconnect{})
	require.NoError(t, target.Attach(newAtt))

	shell.mu.Lock()
	shell.delegate = target
	shell.attachment = nil
	shell.stopHeartbeatLocked()
	shell.mu.Unlock()
}

// assertSessionServing asserts the resumed session is still alive and serving
// on the given transport: attached, registered in the hub, still subscribed,
// and able to deliver to the wire.
func assertSessionServing(t *testing.T, node *fakeRuntime, sess *Session, transport *scriptedTransport) {
	t.Helper()
	assert.Equal(t, SessionAttached, sess.State(), "the resumed session must stay attached")
	assert.Same(t, sess, node.hub.LookupSession(sess.SessionID()),
		"the resumed session must stay registered in the hub")
	assert.Equal(t, 1, node.hub.NumSubscribers(resumeTestChannel),
		"the resumed session's subscriptions must survive the stale shell's death")
	before := transport.writeCount()
	msg := MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Pong{Pong: &clientpb.Pong{}}
	})
	require.NoError(t, sess.Send(context.Background(), msg))
	assert.Equal(t, before+1, transport.writeCount(),
		"the resumed session must keep delivering on the current connection")
}

// TestSession_ChainedResume_StaleShellDeathKeepsSession is the defect
// scenario: E is resumed by Y and then by Z; the takeover closed Y's
// transport, so Y's read loop exits and runs its close paths. E must keep
// serving Z through both of them.
func TestSession_ChainedResume_StaleShellDeathKeepsSession(t *testing.T) {
	node := newFakeRuntime()

	sessE := newResumableSession(t, node, "sess-chain")
	shellY, transportY, closeConnY := newResumeShell(t, node)
	resumeOnto(t, shellY, sessE)
	require.Equal(t, SessionAttached, sessE.State())

	shellZ, transportZ, _ := newResumeShell(t, node)
	resumeOnto(t, shellZ, sessE)
	require.Len(t, transportY.record(), 1, "the chained takeover must have closed Y's transport")

	// Y's read loop exits: the deferred per-connection close runs
	// (closeFromAttachment with the shell's original attachment).
	require.NoError(t, closeConnY())
	assertSessionServing(t, node, sessE, transportZ)

	// A stale frame on Y's read loop returns a Disconnect instead
	// (closeFromLoop).
	shellY.closeFromLoop(Disconnect{})
	assertSessionServing(t, node, sessE, transportZ)
}

// TestSession_SingleResume_ConnectionDeathClosesSession pins semantic (a):
// with a single resume, the resuming connection IS the session's current
// connection, so its death closes the session normally.
func TestSession_SingleResume_ConnectionDeathClosesSession(t *testing.T) {
	t.Run("closeFromAttachment", func(t *testing.T) {
		node := newFakeRuntime()
		sessE := newResumableSession(t, node, "sess-single-att")
		shellY, _, closeConnY := newResumeShell(t, node)
		resumeOnto(t, shellY, sessE)

		require.NoError(t, closeConnY())
		assert.Equal(t, SessionClosed, sessE.State())
		assert.Nil(t, node.hub.LookupSession("sess-single-att"))
		assert.Zero(t, node.hub.NumSubscribers(resumeTestChannel))
	})

	t.Run("closeFromLoop", func(t *testing.T) {
		node := newFakeRuntime()
		sessE := newResumableSession(t, node, "sess-single-loop")
		shellY, _, _ := newResumeShell(t, node)
		resumeOnto(t, shellY, sessE)

		shellY.closeFromLoop(Disconnect{})
		assert.Equal(t, SessionClosed, sessE.State())
		assert.Nil(t, node.hub.LookupSession("sess-single-loop"))
		assert.Zero(t, node.hub.NumSubscribers(resumeTestChannel))
	})
}

// TestSession_ChainedResume_CurrentConnectionDeathClosesSession pins semantic
// (c): after a chained resume, the death of the CURRENT connection (Z) still
// closes the session normally.
func TestSession_ChainedResume_CurrentConnectionDeathClosesSession(t *testing.T) {
	t.Run("closeFromAttachment", func(t *testing.T) {
		node := newFakeRuntime()
		sessE := newResumableSession(t, node, "sess-chain-att")
		shellY, _, _ := newResumeShell(t, node)
		resumeOnto(t, shellY, sessE)
		shellZ, _, closeConnZ := newResumeShell(t, node)
		resumeOnto(t, shellZ, sessE)

		require.NoError(t, closeConnZ())
		assert.Equal(t, SessionClosed, sessE.State())
		assert.Nil(t, node.hub.LookupSession("sess-chain-att"))
		assert.Zero(t, node.hub.NumSubscribers(resumeTestChannel))
	})

	t.Run("closeFromLoop", func(t *testing.T) {
		node := newFakeRuntime()
		sessE := newResumableSession(t, node, "sess-chain-loop")
		shellY, _, _ := newResumeShell(t, node)
		resumeOnto(t, shellY, sessE)
		shellZ, _, _ := newResumeShell(t, node)
		resumeOnto(t, shellZ, sessE)

		shellZ.closeFromLoop(Disconnect{})
		assert.Equal(t, SessionClosed, sessE.State())
		assert.Nil(t, node.hub.LookupSession("sess-chain-loop"))
		assert.Zero(t, node.hub.NumSubscribers(resumeTestChannel))
	})
}
