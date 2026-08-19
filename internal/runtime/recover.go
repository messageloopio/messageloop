package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/lynx-go/x/log"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// RecoverStatus classifies the outcome of one channel recovery attempt.
type RecoverStatus int

const (
	// RecoverSkipped means History was never called: recover was not
	// requested, the channel is a wildcard pattern, channel policy denies
	// recovery, a resume snapshot carries no server-recorded offset, or a
	// non-resume recover carries no cursor and the session has no
	// server-recorded delivered offset (§4.1).
	RecoverSkipped RecoverStatus = iota
	// RecoverOK means History succeeded (possibly with zero publications).
	RecoverOK
	// RecoverTruncated means History hit the request-level or policy cap.
	RecoverTruncated
	// RecoverFailed means History returned an error.
	RecoverFailed
	// RecoverEpochReset means the offsets were invalidated by a broker epoch
	// change and recovery started over from the beginning successfully.
	RecoverEpochReset
)

// String returns the metric/log label for the status.
func (s RecoverStatus) String() string {
	switch s {
	case RecoverSkipped:
		return "skipped"
	case RecoverOK:
		return "ok"
	case RecoverTruncated:
		return "truncated"
	case RecoverFailed:
		return "failed"
	case RecoverEpochReset:
		return "epoch_reset"
	}
	return "unknown"
}

// ChannelRecovery is the outcome of one replay for one channel. The streamed
// publications go out through the session's Send during recoverSubscription;
// the struct only carries the authoritative cursor and status for the
// per-channel RecoverComplete. Offset is the authoritative echo (last
// delivered offset, or the pre-replay cursor for skipped/failed/empty), with
// OffsetSet false when the offset is deliberately unset (fresh start with an
// empty batch).
type ChannelRecovery struct {
	Channel   string
	Status    RecoverStatus
	Offset    uint64
	OffsetSet bool
	Epoch     string
	Err       error
	gap       bool
	gapReason sharedv2.GapReason
	pubCount  int
}

// positionFrom builds the client-wire Position: the offset is only set when
// set == true; otherwise it stays unset (transient / fresh / unknown), never
// 0-means-unset (KD-K22).
func positionFrom(epoch string, offset uint64, set bool) *sharedv2.Position {
	p := &sharedv2.Position{StreamEpoch: epoch}
	if set {
		off := offset
		p.Offset = &off
	}
	return p
}

// offsetFrom reads the client subscription cursor's optional offset.
func offsetFrom(p *sharedv2.Position) (offset uint64, set bool) {
	if p == nil || p.Offset == nil {
		return 0, false
	}
	return p.GetOffset(), true
}

// streamEpoch returns the broker's StreamEpoch, or "" when the broker does
// not expose one.
func (n *Node) streamEpoch() string {
	if epocher, ok := n.broker.(interface{ Epoch() string }); ok {
		return epocher.Epoch()
	}
	return ""
}

// recoverQuota is the per-request MaxRecoveredPublications budget shared by
// every channel recovered in one Connect or Subscribe request.
type recoverQuota struct {
	remaining int
}

func newRecoverQuota() *recoverQuota {
	return &recoverQuota{remaining: MaxRecoveredPublications}
}

// recoverState classifies the whole-request recover state for SubscribeAck
// (§4.2): NONE when the batch has no recover request, SKIPPED when every
// recover request is skippable before History, PENDING otherwise. Callers send
// the bare ack first and then streamRecoveries.
func (n *Node) recoverState(c *Session, subs []*clientpb.Subscription, snapshot *ClusterSessionSnapshot) clientpb.RecoverState {
	anyRecover := false
	anyWillStream := false
	for _, sub := range subs {
		if sub == nil || !sub.GetRecover() {
			continue
		}
		anyRecover = true
		if n.recoverySkip(n.streamEpoch(), sub, snapshot, c) == "" {
			anyWillStream = true
		}
	}
	if !anyRecover {
		return clientpb.RecoverState_RECOVER_STATE_NONE
	}
	if anyWillStream {
		return clientpb.RecoverState_RECOVER_STATE_PENDING
	}
	return clientpb.RecoverState_RECOVER_STATE_SKIPPED
}

// streamRecoveries replays every recover=true channel of one request through
// the shared Replayer in request order. The caller has already sent the bare
// Connected / SubscribeAck; this method only streams per-channel replay
// publications followed by exactly one RecoverComplete each (§4.2).
func (n *Node) streamRecoveries(ctx context.Context, c *Session, in *clientpb.InboundMessage, subs []*clientpb.Subscription, snapshot *ClusterSessionSnapshot, path string) {
	quota := newRecoverQuota()
	for _, sub := range subs {
		if sub == nil || !sub.GetRecover() {
			continue
		}
		n.recoverSubscription(ctx, c, in, sub, snapshot, quota, path)
	}
}

// recoverSubscription recovers history for one exact subscription on behalf of
// a Connect or Subscribe request and streams it out: every replayed
// publication (each its own outbound frame, replay=true) is written through
// the session's Send before the single RecoverComplete for the channel lands.
// snapshot != nil marks a session resume: the server-recorded ChannelOffsets
// win over the client-reported cursor, and a channel missing from
// ChannelOffsets is skipped (never replayed from the beginning) unless the
// client asked for a fresh start. The quota is decremented once publications
// are delivered, so a single request shares the MaxRecoveredPublications cap
// across all of its channels. A recovery failure never unsubscribes the
// channel (KD-9).
func (n *Node) recoverSubscription(
	ctx context.Context,
	c *Session,
	in *clientpb.InboundMessage,
	sub *clientpb.Subscription,
	snapshot *ClusterSessionSnapshot, // nil = 非 resume
	quota *recoverQuota,
	path string,
) ChannelRecovery {
	currentEpoch := n.streamEpoch()
	resume := snapshot != nil

	if sub == nil || sub.Channel == "" {
		// Defensive: callers only pass valid subscriptions; an empty channel
		// must never reach History.
		res := ChannelRecovery{Status: RecoverSkipped, Epoch: currentEpoch}
		if sub != nil {
			res.Channel = sub.Channel
			if sub.Recover {
				res.Err = errors.New("recovery skipped: empty channel")
			}
		}
		return n.finishRecovery(ctx, c, in, res, path)
	}

	res := ChannelRecovery{Channel: sub.Channel, Epoch: currentEpoch}

	// §4.1 cursor and sinceOffset. skip is non-empty exactly when History must
	// not be called (wildcard / policy / no cursor and no server record).
	cursor, cursorSet, epochReset, sinceOffset, cursorSkip := n.recoveryCursor(currentEpoch, sub, snapshot, resume, c)
	skip := n.recoverySkip(currentEpoch, sub, snapshot, c)
	if skip == "" && cursorSkip != "" {
		// The pre-History gates (recoverySkip) are the authority; recoveryCursor
		// returns skip details only for cases those gates already cover, so
		// this arm is defensive.
		skip = cursorSkip
	}
	res.Offset, res.OffsetSet = cursor, cursorSet

	if skip != "" {
		res.Status = RecoverSkipped
		if sub.Recover {
			res.Err = fmt.Errorf("recovery skipped: %s", skip)
		}
		return n.finishRecovery(ctx, c, in, res, path)
	}

	// §5.3 limit and quota: the request-level cap wins over the policy cap
	// wins over the global default.
	pol := n.ChannelPolicy(sub.Channel)
	limit := MaxRecoveredPublications
	if pol.RecoverLimit > 0 && pol.RecoverLimit < limit {
		limit = pol.RecoverLimit
	}
	if quota.remaining < limit {
		limit = quota.remaining
	}
	if limit < 0 {
		limit = 0
	}

	if limit == 0 {
		// Quota exhausted: no History call, the channel is truncated with an
		// empty batch so the client can catch up with a later request.
		res.Status = RecoverTruncated
		return n.finishRecovery(ctx, c, in, res, path)
	}

	historyPage, err := n.broker.History(sub.Channel, sinceOffset, limit)
	if err != nil {
		// The subscription is already committed and must not be rolled back:
		// a history hiccup must not prevent the client from entering the
		// channel (KD-9). Surface the failure in RecoverComplete.error.
		res.Status = RecoverFailed
		res.Err = err
		return n.finishRecovery(ctx, c, in, res, path)
	}

	historyPubs := historyPage.Pubs()
	lastOffset, lastSet := cursor, cursorSet
	for _, pub := range historyPubs {
		out := MakeOutboundMessage(in, func(o *clientpb.OutboundMessage) {
			o.Envelope = &clientpb.OutboundMessage_Publication{
				Publication: publicationToClient(sub.Channel, pub, true),
			}
		})
		if err := c.Send(ctx, out); err != nil {
			// The write queue is failing (slow consumer / closed attachment):
			// abort the replay loop. The RecoverComplete still goes through the
			// Control lane so per-channel completion semantics hold best-effort.
			log.WarnContext(ctx, "replay send failed", err, "channel", sub.Channel)
			break
		}
		lastOffset, lastSet = pub.Offset, true
		quota.remaining--
		res.pubCount++
	}

	gap := historyPage != nil && historyPage.Gap
	res.gap = gap
	res.gapReason = gapReasonV2(historyPage.GapReason)
	if epochReset {
		res.gap = true
		res.gapReason = sharedv2.GapReason_GAP_REASON_EPOCH_RESET
	}
	if gap {
		n.observeRecoveryGap(historyPage.GapReason)
	}

	// §5.4: the authoritative position is the last delivered publication's,
	// never the plain cursor.
	res.Offset, res.OffsetSet = lastOffset, lastSet

	if epochReset {
		res.Status = RecoverEpochReset
	} else if len(historyPubs) == limit {
		// Conservative: a full batch means the broker may have more.
		res.Status = RecoverTruncated
	} else if gap && len(historyPubs) == 0 {
		// A gap with an empty batch means the client's cursor cannot be
		// proven covered: never claim RecoverOK (that would be "pretending
		// to have caught up"). Report truncated and echo the cursor so the
		// client can fall back or retry.
		res.Status = RecoverTruncated
	} else {
		res.Status = RecoverOK
	}
	return n.finishRecovery(ctx, c, in, res, path)
}

// recoveryCursor computes the authoritative pre-replay cursor for one channel
// plus the History sinceOffset §4.1. It returns:
//
//	skip != ""      History must not be called (report RecoverSkipped).
//	cursor/curSet   the authoritative echo position.
//	epochReset      the offsets were invalidated by an epoch change: recover
//	                from the beginning.
//
// Only two conditions mean "from the start": sub.Fresh == true, or a resume
// whose snapshot epoch differs from the broker epoch (both known). offset 0
// alone never means from the start.
func (n *Node) recoveryCursor(currentEpoch string, sub *clientpb.Subscription, snapshot *ClusterSessionSnapshot, resume bool, c *Session) (cursor uint64, curSet, epochReset bool, sinceOffset uint64, skip string) {
	if sub.GetFresh() {
		// Explicit from the start: ignore cursor.offset and server records.
		return 0, false, false, 0, ""
	}

	if resume {
		serverOffset, ok := snapshot.ChannelOffsets[sub.Channel]
		if snapshot.BrokerEpoch != "" && currentEpoch != "" && snapshot.BrokerEpoch != currentEpoch {
			// Both epochs are known and differ: the recorded offsets belong
			// to an invalidated history generation, recover from scratch.
			return 0, false, true, 0, ""
		}
		if !ok {
			// The snapshot deliberately omits channels that never delivered
			// history: replaying from the client cursor could flood 1000
			// messages on every takeover.
			return 0, false, false, 0, "resume snapshot has no server-recorded offset"
		}
		return serverOffset, true, false, serverOffset + 1, ""
	}

	// Non-resume: the client cursor is a hint. A set offset (including 0) is a
	// legal resume point; an unset cursor falls back to the server-recorded
	// delivered offset for this session/channel, or skips to avoid flooding the
	// full history on every re-subscribe.
	if offset, set := offsetFrom(sub.Cursor); set {
		return offset, true, false, offset + 1, ""
	}
	if delivered := n.deliveredOffset(c, sub.Channel); delivered > 0 {
		return delivered, true, false, delivered + 1, ""
	}
	return 0, false, false, 0, "no cursor and no server-recorded delivered offset"
}

// recoverySkip merges the pre-History skip gates (§5.1): the wildcard /
// channel-policy / resume-offset gates plus the non-resume cursor gate.
// Only a gate order that skips BEFORE History may skip; everything else
// streams.
func (n *Node) recoverySkip(currentEpoch string, sub *clientpb.Subscription, snapshot *ClusterSessionSnapshot, c *Session) string {
	if !sub.Recover {
		return "recover not requested"
	}
	if isWildcard(sub.Channel) {
		return "wildcard channels have no history"
	}
	pol := n.ChannelPolicy(sub.Channel)
	if !pol.Recover || !pol.History || pol.TransientOnly {
		return "channel policy denies recovery"
	}
	if snapshot != nil {
		_, ok := snapshot.ChannelOffsets[sub.Channel]
		if !ok && !sub.GetFresh() {
			// A channel missing from ChannelOffsets is skipped (never replayed
			// from the beginning) unless the client explicitly asked for a
			// fresh start.
			return "resume snapshot has no server-recorded offset"
		}
		return ""
	}
	// Non-resume, fresh, recover=true: History always runs.
	if sub.GetFresh() {
		return ""
	}
	// Non-resume with a cursor: History always runs.
	if _, set := offsetFrom(sub.Cursor); set {
		return ""
	}
	// No hint: fall back to the server-recorded delivered offset; without one
	// the channel is skipped.
	if delivered := n.deliveredOffset(c, sub.Channel); delivered > 0 {
		return ""
	}
	return "no cursor and no server-recorded delivered offset"
}

// deliveredOffset returns the last offset successfully delivered to session c
// on exact channel ch, tracked by the hub during live broadcast (0 when
// nothing was delivered, or ch is a wildcard pattern).
func (n *Node) deliveredOffset(c *Session, ch string) uint64 {
	if c == nil {
		return 0
	}
	if sub, ok := n.hub.LookupSubscriber(ch, c); ok {
		return sub.DeliveredOffset
	}
	return 0
}

// gapReasonV2 maps the internal HistoryGapReason to the client-wire GapReason.
func gapReasonV2(reason HistoryGapReason) sharedv2.GapReason {
	switch reason {
	case HistoryGapHeadTrimmed:
		return sharedv2.GapReason_GAP_REASON_HEAD_TRIMMED
	case HistoryGapEmptyExpired:
		return sharedv2.GapReason_GAP_REASON_EMPTY_EXPIRED
	case HistoryGapMiddle:
		return sharedv2.GapReason_GAP_REASON_MIDDLE
	}
	return sharedv2.GapReason_GAP_REASON_NONE
}

// observeRecoveryGap records the history gap reason metric for one channel
// recovery attempt.
func (n *Node) observeRecoveryGap(reason HistoryGapReason) {
	if n.metrics == nil {
		return
	}
	label := "unknown"
	switch reason {
	case HistoryGapHeadTrimmed:
		label = "head_trimmed"
	case HistoryGapEmptyExpired:
		label = "empty_expired"
	case HistoryGapMiddle:
		label = "middle"
	}
	n.metrics.RecoveryGapTotal.WithLabelValues(label).Inc()
}

// finishRecovery sends the per-channel RecoverComplete and records the
// metrics and log line for one recovery attempt (§4.2: every recover=true
// channel ends with exactly one RecoverComplete).
func (n *Node) finishRecovery(ctx context.Context, c *Session, in *clientpb.InboundMessage, res ChannelRecovery, path string) ChannelRecovery {
	if c != nil {
		complete := &clientpb.RecoverComplete{
			Channel:   res.Channel,
			Position:  positionFrom(res.Epoch, res.Offset, res.OffsetSet),
			Truncated: res.Status == RecoverTruncated,
			Gap:       res.gap,
			GapReason: res.gapReason,
		}
		switch res.Status {
		case RecoverFailed:
			complete.Error = &sharedv2.Error{
				Code:    "RECOVER_FAILED",
				Type:    "recover_error",
				Message: res.Err.Error(),
			}
		case RecoverSkipped:
			if res.Err != nil {
				complete.Error = &sharedv2.Error{
					Code:    "RECOVER_SKIPPED",
					Type:    "recover_error",
					Message: res.Err.Error(),
				}
			}
		}
		_ = c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
			out.Envelope = &clientpb.OutboundMessage_RecoverComplete{RecoverComplete: complete}
		}))
	}

	if n.metrics != nil {
		result := "ok"
		switch res.Status {
		case RecoverSkipped:
			result = "skipped"
		case RecoverTruncated:
			result = "truncated"
		case RecoverFailed:
			result = "failed"
		}
		n.metrics.RecoveryTotal.WithLabelValues(path, result).Inc()
		n.metrics.RecoveryPublications.WithLabelValues(path).Observe(float64(res.pubCount))
		if res.Status == RecoverTruncated {
			n.metrics.RecoveryTruncatedTotal.WithLabelValues(path).Inc()
		}
	}

	fields := []any{
		"channel", res.Channel,
		"status", res.Status.String(),
		"count", res.pubCount,
		"truncated", res.Status == RecoverTruncated,
	}
	if res.Err != nil {
		fields = append(fields, "error", res.Err)
	}
	if res.Status == RecoverTruncated || res.Status == RecoverFailed {
		log.WarnContext(ctx, "channel recovery finished", fields...)
	} else {
		log.DebugContext(ctx, "channel recovery finished", fields...)
	}
	return res
}

// publicationToClient converts one broker publication into the client
// protocol Publication envelope (v2, one message per frame). Replay delivery
// sets replay=true. Realtime delivery and recovery share the stable
// channel-offset message ID so clients can deduplicate.
func publicationToClient(channel string, pub *Publication, replay bool) *clientpb.Publication {
	return &clientpb.Publication{
		Messages: []*clientpb.Message{
			{
				Id:       publicationID(channel, pub.Offset),
				Channel:  channel,
				Position: positionFrom(pub.Epoch, pub.Offset, true),
				Payload:  pub.PayloadProtoV2(),
				Metadata: func() *sharedv2.Metadata {
					if len(pub.Metadata) == 0 {
						return nil
					}
					return &sharedv2.Metadata{Entries: pub.Metadata}
				}(),
				Replay: replay,
			},
		},
	}
}
