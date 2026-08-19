package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lynx-go/x/log"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// SessionState is the lifecycle state of a Session (KD-K2). Detached is only
// the local handover window: the process still holds the Session and the
// Directory still recognizes this fencing, but the attachment is torn off.
// A fenced node never enters Detached.
type SessionState uint8

const (
	SessionAuthenticating SessionState = iota + 1
	SessionAttached
	SessionDetached
	SessionClosed
)

// Attachment is one transport binding: the socket plus how bytes are framed.
// It does not own subscriptions, fencing, or Occupancy.
type Attachment struct {
	Transport Transport
	Marshaler Marshaler
	Protocol  string // "ws" | "grpc" | "quic"
}

// Send queue depths (§7 of the PR-KA-B1 spec).
const (
	sendQueueControlDepth = 32
	sendQueueDataDepth    = 256
)

var (
	// ErrSendQueueFull is returned by Send when a queue lane is full (Data
	// 256 / Control 32). The session is closed with DisconnectSlowConsumer.
	ErrSendQueueFull = errors.New("send queue full")
	// ErrSessionNotAttached is returned by Send on a session that has no
	// live writer (Detached window, or after Close).
	ErrSessionNotAttached = errors.New("session not attached")
	// ErrOutboundTooLarge is returned by Send when the marshaled frame
	// exceeds MaxMessageSize (§7: the outbound path honors the same cap as
	// the inbound path).
	ErrOutboundTooLarge = errors.New("outbound frame exceeds MaxMessageSize")
	// errMarshalerChanged is returned by enqueueBytes when the session's
	// current attachment encoding no longer matches the marshaler the frame
	// bytes were produced with (a resume/reattach swapped the attachment
	// between marshaling and enqueueing). Callers holding the original
	// message re-marshal and retry; it never reaches clients.
	errMarshalerChanged = errors.New("marshaler changed during send")
)

// Session is the recoverable logical connection (KD-K2). The Hub holds this
// pointer for the lifetime of the session on this node; a local resume only
// replaces the Attachment, never the Session object.
type Session struct {
	mu   sync.RWMutex
	ctx  context.Context
	user string // 用户 ID
	// client is the client-reported device/end ID.
	client string
	// session is the server-issued session ID.
	session string
	// authenticated is set once Connect completed successfully.
	authenticated bool
	// state is the session lifecycle state; it is never left at the zero
	// value: NewClient starts every session at SessionAuthenticating.
	state SessionState
	// attachment is the current transport binding. The initial attachment
	// is created by NewClient; Attach replaces it on resume.
	attachment *Attachment
	// out is the single outbound queue. It is dropped on Detach and
	// recreated on Attach.
	out *sendQueue
	// delegate, when set, routes this connection's reads/writes/close to the
	// canonical session object: after a local resume the temporary
	// Authenticating session becomes a read-loop shell over the resumed
	// session (the resumed object keeps serving from the new attachment).
	delegate *Session
	// loopAtt is the attachment this connection object was created with
	// (immutable). A read loop whose loopAtt is no longer the session's
	// current attachment belongs to a detached connection and must not tear
	// the session down.
	loopAtt *Attachment
	rt      Runtime

	// Connection metadata
	protocol    string // ws, grpc, or quic
	connectedAt time.Time

	// Heartbeat fields
	lastActivity    time.Time
	heartbeatCancel context.CancelFunc
	// pingDeadline is the one-shot timer armed after every outbound server
	// ping; it disconnects with 3511 when it fires unanswered (strategy B).
	// Guarded by mu. See heartbeat.go.
	pingDeadline *time.Timer
	// heartbeatDisconnectOnce makes the 3511 close idempotent: when the ping
	// deadline and the idle ticker race, exactly one caller issues the close
	// and counts heartbeat_idle_disconnects_total.
	heartbeatDisconnectOnce atomic.Bool

	// Tracks channels this session is subscribed to, for presence cleanup.
	subscribedChannels  map[string]struct{}
	clusterLeaseVersion uint64

	// Rate limiter for publish operations.
	publishLimiter *rate.Limiter

	// surveyInFlight guards against a second client survey while the first
	// worker is still collecting responses (KD-15: one survey per session).
	surveyInFlight atomic.Bool
	// surveyLimiter rate-limits client survey initiation: 1/s, burst 1.
	surveyLimiter *rate.Limiter

	// metricsCharged is set once AddClient has counted this connection in
	// ConnectionsTotal; Close only decrements the gauge when it is set.
	metricsCharged bool

	// lastClusterSyncNano is the UnixNano timestamp of the last presence /
	// cluster refresh triggered by a ping, used to throttle repeated syncs.
	lastClusterSyncNano atomic.Int64
}

// Client is the transitional alias for Session (PR-KA-B1 §2): the kernel type
// is Session; the alias keeps external callers and tests compiling while the
// rename settles.
type Client = Session

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

// State returns the session lifecycle state.
func (s *Session) State() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// String returns the numeric state as a string (metrics/logging).
func (s SessionState) String() string {
	switch s {
	case SessionAuthenticating:
		return "1"
	case SessionAttached:
		return "2"
	case SessionDetached:
		return "3"
	case SessionClosed:
		return "4"
	default:
		return "unknown"
	}
}

// queuedFrame is one marshaled outbound frame in the send queue.
type queuedFrame struct {
	bytes   []byte
	control bool
	// done is signaled with the write result once the frame was handed to
	// the transport (or failed). Send waits on it, so a caller observes the
	// write outcome synchronously while ordering and Control priority stay
	// with the single writer goroutine.
	done chan error
}

// sendQueue is the session's single outbound queue with two lanes: Control
// (depth 32) and Data (depth 256). The next-frame selection always prefers
// Control (§7).
type sendQueue struct {
	mu      sync.Mutex
	notFull *sync.Cond
	closed  bool
	control []*queuedFrame
	data    []*queuedFrame
}

func newSendQueue() *sendQueue {
	q := &sendQueue{}
	q.notFull = sync.NewCond(&q.mu)
	return q
}

// tryEnqueue is the non-blocking variant used by Send: a full lane fails
// fast with ErrSendQueueFull instead of blocking the caller.
func (q *sendQueue) tryEnqueue(frame *queuedFrame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrSessionNotAttached
	}
	if frame.control {
		if len(q.control) >= sendQueueControlDepth {
			return ErrSendQueueFull
		}
		q.control = append(q.control, frame)
	} else {
		if len(q.data) >= sendQueueDataDepth {
			return ErrSendQueueFull
		}
		q.data = append(q.data, frame)
	}
	q.notFull.Broadcast()
	return nil
}

// dequeue blocks until a frame is available; ok is false when the queue was
// closed (Detach/Fence/Close), which also fails every pending frame.
func (q *sendQueue) dequeue() (frame *queuedFrame, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for !q.closed && len(q.control) == 0 && len(q.data) == 0 {
		q.notFull.Wait()
	}
	if q.closed {
		return nil, false
	}
	if len(q.control) > 0 {
		frame = q.control[0]
		q.control[0] = nil
		q.control = q.control[1:]
		return frame, true
	}
	frame = q.data[0]
	q.data[0] = nil
	q.data = q.data[1:]
	return frame, true
}

// close wakes the writer and fails every pending frame with
// ErrSessionNotAttached. It is called by Detach/Fence/Close; the queue is
// discarded (replaced by a fresh one on the next Attach).
func (q *sendQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	frames := append(q.control, q.data...)
	q.control = nil
	q.data = nil
	for _, frame := range frames {
		frame.done <- ErrSessionNotAttached
	}
	q.notFull.Broadcast()
}

// Attach binds a new attachment to the session and makes it live: the
// attachment is swapped, the state moves to Attached, a fresh queue is
// created and the single writer goroutine starts draining it. Allowed from
// Authenticating (initial connect) and Detached (local resume). The
// attachment is probed first so a transport that is already closed fails the
// attach synchronously (§6: a failed Attach after Detach falls through to
// Close, never to "Directory held with no attachment").
func (s *Session) Attach(att *Attachment) error {
	if att == nil || att.Transport == nil {
		return fmt.Errorf("attach: nil attachment")
	}
	if err := att.Transport.WriteMany(); err != nil {
		return fmt.Errorf("attach: transport probe failed: %w", err)
	}

	s.mu.Lock()
	if s.state == SessionClosed {
		s.mu.Unlock()
		return fmt.Errorf("attach: session closed")
	}
	if s.state == SessionAttached {
		s.mu.Unlock()
		return fmt.Errorf("attach: session already attached")
	}
	wasDetached := s.state == SessionDetached
	s.state = SessionAttached
	s.attachment = att
	s.protocol = att.Protocol
	// From Authenticating the pending pre-attach queue carries over (frames
	// enqueued before the writer existed — e.g. §9.8 ordering); from Detached
	// the queue was dropped and a fresh one starts.
	queue := s.out
	if wasDetached {
		queue = newSendQueue()
		s.out = queue
	}
	s.stopHeartbeatLocked()
	s.mu.Unlock()

	s.startHeartbeat(s.ctx)
	go s.writerLoop(att, queue)
	return nil
}

// Detach tears off the current attachment: the transport is closed, the
// writer is stopped and the queue is dropped. The session stays in the Hub
// (state Detached) for the local takeover window. Detach on a non-Attached
// session is a no-op (§8).
func (s *Session) Detach(reason Disconnect) {
	s.mu.Lock()
	if s.state != SessionAttached {
		s.mu.Unlock()
		return
	}
	s.state = SessionDetached
	s.stopHeartbeatLocked()
	s.stopWriterLocked()
	att := s.attachment
	s.attachment = nil
	s.mu.Unlock()

	if att != nil && att.Transport != nil {
		_ = att.Transport.Close(reason)
	}
}

// Fence closes a session that another owner took over (KD-K5): local
// subscriptions and the hub entry are removed, the attachment is closed —
// but the Directory is not unbound and presence is not left, because the
// session now belongs to the new owner. Idempotent on a Closed session.
func (s *Session) Fence(reason Disconnect) error {
	c := s.canonical()
	if c != s {
		return c.Fence(reason)
	}
	s.mu.Lock()
	if s.state == SessionClosed {
		s.mu.Unlock()
		return nil
	}
	s.state = SessionClosed
	s.stopHeartbeatLocked()
	s.stopWriterLocked()
	channels := make([]string, 0, len(s.subscribedChannels))
	for ch := range s.subscribedChannels {
		channels = append(channels, ch)
	}
	sessionID := s.session
	att := s.attachment
	s.attachment = nil
	s.mu.Unlock()

	// Remove every channel even when individual removals fail, so no channel
	// is left behind; track which channels were actually removed from the hub
	// for rollback and shared projection adjustment.
	var fenceErrs []error
	removed := make([]string, 0, len(channels))
	ephemeralByChannel := make(map[string]bool, len(channels))
	for _, ch := range channels {
		if stored, ok := s.rt.Hub().LookupSubscriber(ch, s); ok {
			ephemeralByChannel[ch] = stored.Ephemeral
		}
		wasRemoved, err := s.rt.RemoveLocalSubscriptionOnly(ch, s, true)
		if err != nil {
			fenceErrs = append(fenceErrs, fmt.Errorf("remove channel %s: %w", ch, err))
		}
		if !wasRemoved {
			continue
		}
		removed = append(removed, ch)
		s.rt.AdjustClusterChannelSubscriptionsTimeout(ch, -1)
	}

	if len(fenceErrs) > 0 {
		// Roll back every removed channel (and the projection deltas) so the
		// session is not left half-fenced, then restore the attachment and
		// restart the writer: the fence failed, the session is still alive
		// and must keep serving. Report the aggregated error.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), clusterEvictRollbackTimeout)
		for _, ch := range removed {
			if err := s.rt.RestoreLocalSubscription(rollbackCtx, ch, NewSubscriber(s, ephemeralByChannel[ch])); err != nil {
				fenceErrs = append(fenceErrs, fmt.Errorf("rollback channel %s: %w", ch, err))
			}
			s.rt.AdjustClusterChannelSubscriptionsTimeout(ch, 1)
		}
		cancel()

		var restoredQueue *sendQueue
		s.mu.Lock()
		if s.state == SessionClosed && att != nil {
			s.state = SessionAttached
			s.attachment = att
			restoredQueue = newSendQueue()
			s.out = restoredQueue
			s.stopHeartbeatLocked()
		}
		s.mu.Unlock()
		if restoredQueue != nil {
			s.startHeartbeat(s.ctx)
			go s.writerLoop(att, restoredQueue)
		}
		return errors.Join(fenceErrs...)
	}

	if sessionID != "" {
		// Only evict the session when the registered session is still this
		// one: a newer connection may have taken over the session ID between
		// LookupSession and this removal, and RemoveSessionIfMatches
		// protects it from being evicted by a stale takeover.
		s.rt.Hub().RemoveSessionIfMatches(sessionID, s)
	}
	if att != nil && att.Transport != nil {
		if err := att.Transport.Close(reason); err != nil {
			fenceErrs = append(fenceErrs, err)
		}
	}
	return errors.Join(fenceErrs...)
}

// Close really terminates the session: presence leave, subscription removal,
// hub entry removal (when this session still owns it), Directory unbind, and
// the attachment close (§8). Idempotent on a Closed session.
func (s *Session) Close(reason Disconnect) error {
	c := s.canonical()
	if c != s {
		return c.Close(reason)
	}
	s.mu.Lock()
	if s.state == SessionClosed {
		s.mu.Unlock()
		return nil
	}
	s.state = SessionClosed
	s.stopHeartbeatLocked()
	s.stopWriterLocked()
	channels := make([]string, 0, len(s.subscribedChannels))
	for ch := range s.subscribedChannels {
		channels = append(channels, ch)
	}
	sessionID := s.session
	userID := s.user
	metricsCharged := s.metricsCharged
	att := s.attachment
	s.attachment = nil
	s.mu.Unlock()

	// Remove presence for all subscribed channels first, while the
	// subscriptions are still registered in the hub: ephemeral subscriptions
	// are identified this way and skipped (they never register presence or
	// publish join/leave events). Cleanup runs with bounded concurrency.
	if len(channels) > 0 {
		const maxConcurrentPresence = 16
		presCtx := context.Background()
		work := make(chan string)
		var wg sync.WaitGroup
		for i := 0; i < maxConcurrentPresence; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ch := range work {
					ephemeral := false
					if stored, ok := s.rt.Hub().LookupSubscriber(ch, s); ok {
						ephemeral = stored.Ephemeral
					}
					s.rt.PresenceLeave(presCtx, ch, sessionID, userID, ephemeral)
				}
			}()
		}
		for _, ch := range channels {
			work <- ch
		}
		close(work)
		wg.Wait()
	}

	// Remove local subscriptions before clearing hub state.
	if len(channels) > 0 {
		const maxConcurrentRemovals = 16
		work := make(chan string)
		var wg sync.WaitGroup
		for i := 0; i < maxConcurrentRemovals; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ch := range work {
					if err := s.rt.RemoveSubscription(ch, s); err != nil {
						log.WarnContext(context.Background(), "failed to remove subscription during close", "channel", ch, "session", sessionID, "error", err)
					}
				}
			}()
		}
		for _, ch := range channels {
			work <- ch
		}
		close(work)
		wg.Wait()
	}

	// Only remove the hub entry (and the matching cluster state) when this
	// session still owns it: a failed resume or a takeover must not evict
	// the session currently being served.
	if sessionID != "" {
		if s.rt.Hub().RemoveSessionIfMatches(sessionID, s) {
			if err := s.rt.DeleteClusterSessionState(context.Background(), sessionID); err != nil {
				log.WarnContext(context.Background(), "failed to delete cluster session state", "session", sessionID, "error", err)
			}
		}
	}

	if s.rt.Metrics() != nil && metricsCharged {
		s.rt.Metrics().ConnectionsTotal.WithLabelValues(s.TransportLabel()).Dec()
	}

	// Notify proxy about disconnection
	p := s.rt.FindProxy("", "disconnect")
	if p != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		disconnectedReq := &proxy.OnDisconnectedProxyRequest{
			SessionID: sessionID,
			Username:  userID,
		}
		_, _ = p.OnDisconnected(ctx, disconnectedReq) // Ignore error for notification
	}
	if att != nil && att.Transport != nil {
		return att.Transport.Close(reason)
	}
	return nil
}

// stopWriterLocked closes the send queue, which wakes the writer goroutine
// and fails every pending frame. Callers must hold s.mu.
func (s *Session) stopWriterLocked() {
	if s.out != nil {
		s.out.close()
	}
}

// stopHeartbeatLocked cancels the heartbeat loop. Callers must hold s.mu.
func (s *Session) stopHeartbeatLocked() {
	if s.heartbeatCancel != nil {
		s.heartbeatCancel()
		s.heartbeatCancel = nil
	}
}

// startHeartbeat starts the heartbeat loop bound to ctx (restarting it after
// a resume: Detach cancelled the previous loop).
func (s *Session) startHeartbeat(ctx context.Context) {
	if s.rt == nil || s.rt.Heartbeat() == nil {
		return
	}
	s.rt.Heartbeat().Start(ctx, s)
}

// writerLoop is the session's single writer goroutine: it drains the send
// queue onto att's transport, Control frames first, until the queue is
// closed (Detach/Fence/Close) or a write fails. Write failures are mapped to
// the §7 table (io.EOF / peer close → 3000, timeouts and slow consumers →
// 3512, fenced → 3502 via Fence) and close the session — unless the
// attachment was already detached/closed in the meantime.
func (s *Session) writerLoop(att *Attachment, queue *sendQueue) {
	for {
		frame, ok := queue.dequeue()
		if !ok {
			return
		}
		err := s.writeFrame(att, frame)
		frame.done <- err
		if err != nil {
			s.handleWriteError(att, err)
			return
		}
	}
}

// writeFrame writes one frame to the attachment transport.
func (s *Session) writeFrame(att *Attachment, frame *queuedFrame) error {
	if att == nil || att.Transport == nil {
		return ErrSessionNotAttached
	}
	return att.Transport.Write(frame.bytes)
}

// handleWriteError maps a transport write error to the §7 table and closes
// the session, unless the attachment was superseded (Detach/Close ran while
// the write was in flight — the session must not tear itself down then).
func (s *Session) handleWriteError(att *Attachment, err error) {
	s.mu.RLock()
	current := s.attachment
	s.mu.RUnlock()
	if current != att {
		return
	}
	switch {
	case errors.Is(err, ErrSessionFenced):
		// The Directory no longer recognizes this fencing (A1): the session
		// was taken over. Fence — no Leave, no Unbind.
		_ = s.Fence(DisconnectStale)
	case isPeerClosedError(err):
		_ = s.Close(DisconnectConnectionClosed)
	default:
		_ = s.Close(DisconnectSlowConsumer)
	}
}

// isPeerClosedError reports whether a write error means the peer went away
// (io.EOF, closed network connection, WebSocket close 1000/1001, gRPC
// Canceled/Unavailable) — mapped to Disconnect 3000, never 3512 (§7).
func isPeerClosedError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return true
	}
	code := status.Code(err)
	return code == codes.Canceled || code == codes.Unavailable
}

// isConnectedEnvelope was the pre-B3 recovery batch carrier exemption
// (the Connected envelope carried up to 1000 recovered publications in one
// frame). B3 streams recovery, so the exemption — and this predicate — are
// gone: every outbound frame honors MaxMessageSize.

// outboundFrameClass reports whether the outbound envelope is a Control
// frame. Classification is by envelope, never by payload shape (§7).
// RecoverComplete is Control: the per-frame replay publications have already
// landed on the wire one by one, so a Control Complete cannot reorder ahead
// of them (§4.3).
func outboundFrameClass(msg proto.Message) bool {
	out, ok := msg.(*clientpb.OutboundMessage)
	if !ok {
		return false
	}
	switch out.Envelope.(type) {
	case *clientpb.OutboundMessage_Ping,
		*clientpb.OutboundMessage_Pong,
		*clientpb.OutboundMessage_PublishAck,
		*clientpb.OutboundMessage_SubscribeAck,
		*clientpb.OutboundMessage_UnsubscribeAck,
		*clientpb.OutboundMessage_Connected,
		*clientpb.OutboundMessage_RecoverComplete,
		*clientpb.OutboundMessage_GapNotice,
		*clientpb.OutboundMessage_Error,
		*clientpb.OutboundMessage_SubRefreshAck:
		return true
	default:
		return false
	}
}

// enqueue marshals msg and appends it to the send queue, then waits for the
// writer to deliver it to the transport. The caller observes write failures
// synchronously (the frame's done channel), while ordering and Control
// priority stay with the single writer goroutine.
//
// While the session is still Authenticating (no writer exists yet — e.g. an
// auth-rejection envelope sent before Connect completes) the frame is written
// directly to the attachment synchronously, exactly like the pre-queue write
// path. On a Detached/Closed session with no attachment the send fails fast.
func (s *Session) enqueue(ctx context.Context, msg proto.Message) error {
	if log.FromContext(ctx).Enabled(ctx, slog.LevelDebug) {
		log.DebugContext(ctx, "sending message", "message", jsonLog(msg))
	}
	buf := getBuffer()
	defer putBuffer(buf)
	for {
		s.mu.RLock()
		marshaler := s.attachmentMarshalerLocked()
		s.mu.RUnlock()
		frameBytes, err := marshaler.MarshalAppend((*buf)[:0], msg)
		if err != nil {
			return err
		}
		*buf = frameBytes
		// The queued frame is written asynchronously by the writer goroutine, so
		// the bytes must outlive the pooled buffer: copy before enqueueing.
		err = s.enqueueBytes(ctx, append([]byte(nil), (*buf)...), outboundFrameClass(msg), marshaler)
		if !errors.Is(err, errMarshalerChanged) {
			return err
		}
		// A resume/reattach swapped the attachment to a different encoding
		// between marshaling and enqueueing: re-marshal with the current
		// marshaler and retry.
	}
}

// enqueueBytes queues an already-marshaled frame and waits for its write
// result, following the same paths as enqueue. It backs the broadcast fan-out,
// which serializes a shared publication once per wire encoding and hands the
// same bytes to every subscriber with that encoding — the caller must not
// mutate frameBytes until all enqueueBytes calls sharing it have returned.
//
// marshaler must be the marshaler frameBytes were produced with; it is checked
// against the session's current attachment under the same lock that reads the
// attachment/queue/state, so a mid-flight encoding change fails with
// errMarshalerChanged instead of delivering bytes in the wrong encoding.
func (s *Session) enqueueBytes(ctx context.Context, frameBytes []byte, control bool, marshaler Marshaler) error {
	// The outbound frame cap applies per queue frame. B3 streams recovery as
	// single-message replay frames, so there is no Connected batch exemption
	// anymore: every frame, Connected included, honors MaxMessageSize (§4.3).
	if max := s.rt.MaxMessageSize(); max > 0 && len(frameBytes) > max {
		return ErrOutboundTooLarge
	}
	s.mu.RLock()
	if s.attachmentMarshalerLocked() != marshaler {
		s.mu.RUnlock()
		return errMarshalerChanged
	}
	att := s.attachment
	out := s.out
	state := s.state
	s.mu.RUnlock()
	frame := &queuedFrame{
		bytes:   frameBytes,
		control: control,
		done:    make(chan error, 1),
	}
	if state == SessionAttached {
		if err := out.tryEnqueue(frame); err != nil {
			if errors.Is(err, ErrSendQueueFull) {
				// Data or Control lane full: the peer is not draining fast
				// enough (§7: Data full → 3512 SlowConsumer; Control full →
				// the peer is considered dead, 3512 as well).
				_ = s.Close(DisconnectSlowConsumer)
			}
			return err
		}
		select {
		case err := <-frame.done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if att == nil || att.Transport == nil {
		return ErrSessionNotAttached
	}
	return att.Transport.Write(frame.bytes)
}

// attachmentMarshalerLocked returns the marshaler of the current attachment
// (the initial one is created by NewClient, so it is never nil while the
// session can send). Callers must hold s.mu (read is enough).
func (s *Session) attachmentMarshalerLocked() Marshaler {
	if s.attachment != nil && s.attachment.Marshaler != nil {
		return s.attachment.Marshaler
	}
	return ProtoJSONMarshaler
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
		return delegate.Close(Disconnect{})
	}
	if current != att {
		return nil
	}
	return s.Close(Disconnect{})
}

// closeFromLoop closes the session from a read-loop error path (a handler
// returned a Disconnect). Same identity rule as closeFromAttachment: a
// detached connection's stale frames must not close the resumed session.
func (s *Session) closeFromLoop(dis Disconnect) {
	s.mu.RLock()
	delegate := s.delegate
	current := s.attachment
	loopAtt := s.loopAtt
	s.mu.RUnlock()
	if delegate != nil {
		// This connection handed its attachment to the resumed session: the
		// close is the current attachment's close.
		_ = delegate.Close(dis)
		return
	}
	if current != loopAtt {
		return
	}
	_ = s.Close(dis)
}

// TransportLabel returns the transport label value ("ws", "grpc", or "quic")
// for the connections metric. Anything unknown defaults to "ws".
func (s *Session) TransportLabel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return MetricsTransportLabel(s.protocol)
}
