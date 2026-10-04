package session

import (
	"errors"
)

// This file is the Takeover module: the single home of the local-resume
// identity algebra (architecture review D14). A local resume keeps ONE
// Session object serving — the same subscriptions, the same fencing, the
// same hub entry — and swaps only the Attachment; the dying connection
// object becomes a read-loop shell delegating to the resumed session.
//
// Who may close what is decided here. Three identity comparators apply, in
// this order:
//
//  1. delegate routing — a shell (delegate set) never acts on its own; its
//     close attempts route to the resumed session (canonical), gated by (3).
//  2. attachment pointer identity — a non-delegate connection may only tear
//     the session down while its attachment (or loopAtt, the immutable
//     attachment it was created with) is still the session's current
//     attachment. A superseded attachment's read loop dies silently.
//  3. transport identity — the resumed session's attachment is a fresh
//     object wrapping the transport the shell handed over, so a shell's
//     close attempt on the resumed session compares TRANSPORTS, not
//     attachment pointers; a chained resume rebinds the session to a newer
//     transport and older shells lose their say.
//
// Before this module these rules were smeared across Session's close paths
// (canonical / closeFromAttachment / closeFromLoop / closeIfServingHandoff
// in session.go plus the takeover block of handleConnect in client.go) with
// no shared vocabulary; every recent P0 in this area (lock re-entrancy
// deadlock, stale-shell delegate kill) landed exactly there.

// canonical returns the session object that actually owns the state: a
// delegated shell (local-resume read loop) routes to the resumed session.
func (s *Session) canonical() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.delegate != nil {
		return s.delegate
	}
	return s
}

// handoffAttachment builds the fresh attachment object the resumed session
// binds during a takeover: it rides the shell connection's transport and
// encoding. A new object is required (not the shell's own attachment)
// because attachment pointer identity is per-connection — comparator (2)
// would otherwise let the shell's later close attempts masquerade as the
// current connection.
func handoffAttachment(att *Attachment) *Attachment {
	return &Attachment{
		Transport: att.Transport,
		Marshaler: att.Marshaler,
		Protocol:  att.Protocol,
	}
}

// takeoverBy hands this (shell) connection's transport over to the resumed
// session and turns this object into a delegating read loop. The caller has
// already detached the resumed session's previous attachment; per §5 of the
// connect flow, an Attach failure after Detach is a real close — the
// directory must not be held by a session with no attachment.
func (c *Session) takeoverBy(existing *Session) error {
	c.mu.RLock()
	tempAtt := c.attachment
	c.mu.RUnlock()
	if tempAtt == nil {
		return errors.New("attach: session closed during connect")
	}
	if err := existing.Attach(handoffAttachment(tempAtt)); err != nil {
		_ = existing.Close(DisconnectInternal)
		return err
	}

	// The temporary Authenticating session never enters the hub: it becomes
	// a read-loop shell delegating to the resumed session.
	c.mu.Lock()
	c.delegate = existing
	c.attachment = nil
	c.stopHeartbeatLocked()
	if c.pingDeadline != nil {
		c.pingDeadline.Stop()
		c.pingDeadline = nil
	}
	c.mu.Unlock()
	return nil
}

// closeFromAttachment closes the session only when att is still the current
// attachment. It backs the per-connection close func from NewClient: the
// read loop of a superseded attachment (replaced by a local resume) must not
// tear down the session now served by a newer attachment.
func (s *Session) closeFromAttachment(att *Attachment) error {
	s.mu.RLock()
	delegate := s.delegate
	current := s.attachment
	s.mu.RUnlock()
	if delegate != nil {
		// Comparator (1) then (3): this shell handed its transport to the
		// resumed session, so it may only close that session while the
		// session is still served by the handed-over transport.
		return delegate.closeIfServingHandoff(att, Disconnect{})
	}
	if current != att {
		return nil
	}
	return s.Close(Disconnect{})
}

// closeFromLoop closes the session from a read-loop error path (a handler
// returned a Disconnect). Same identity rules as closeFromAttachment: a
// detached connection's stale frames must not close the resumed session.
func (s *Session) closeFromLoop(dis Disconnect) {
	s.mu.RLock()
	delegate := s.delegate
	current := s.attachment
	loopAtt := s.loopAtt
	s.mu.RUnlock()
	if delegate != nil {
		// Comparator (1) then (3): routed to the resumed session, gated on
		// the transport this shell handed over.
		_ = delegate.closeIfServingHandoff(loopAtt, dis)
		return
	}
	if current != loopAtt {
		return
	}
	_ = s.Close(dis)
}

// closeIfServingHandoff closes the resumed session only while its current
// attachment still rides the transport the dying shell handed over during
// the resume takeover — comparator (3). While it matches, this shell is the
// session's current connection and its death closes the session normally; a
// chained resume rebinds the session to the newer connection's transport,
// so a superseded shell's read-loop exit must leave the session alone. A
// nil handoff, or a resumed session with no current attachment (the Detach
// window of an in-flight chained resume, or an already closed session), is
// not serving this handoff: the in-flight resume either completes and owns
// the session or fails into an explicit Close.
func (s *Session) closeIfServingHandoff(handoff *Attachment, reason Disconnect) error {
	if handoff == nil {
		return nil
	}
	s.mu.RLock()
	current := s.attachment
	s.mu.RUnlock()
	if current == nil || current.Transport != handoff.Transport {
		return nil
	}
	return s.Close(reason)
}
