package messageloop

import (
	"context"
	"errors"
	"fmt"

	"github.com/lynx-go/x/log"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
)

// RecoverStatus classifies the outcome of one channel recovery attempt.
type RecoverStatus int

const (
	// RecoverSkipped means History was never called: recover was not
	// requested, the channel is a wildcard pattern, channel policy denies
	// recovery, or a resume snapshot carries no server-recorded offset.
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

// ChannelRecovery is the outcome of one recoverSubscription call for one
// channel. Err carries the history error (RecoverFailed) or, for
// RecoverSkipped, a reason only when the client asked for recovery
// (sub.Recover == true); a plain skip carries no error.
type ChannelRecovery struct {
	Channel      string
	Status       RecoverStatus
	Publications []*clientpb.Publication
	Offset       uint64 // last delivered offset, or the echoed cursor (§5.4)
	Epoch        string
	Err          error
}

// recoverQuota is the per-request MaxRecoveredPublications budget shared by
// every channel recovered in one Connect or Subscribe request.
type recoverQuota struct {
	remaining int
}

func newRecoverQuota() *recoverQuota {
	return &recoverQuota{remaining: MaxRecoveredPublications}
}

// recoverSubscription recovers history for one subscription on behalf of a
// Connect or Subscribe request. snapshot != nil marks a session resume: the
// server-recorded ChannelOffsets win over the client-reported offset, and a
// channel missing from ChannelOffsets is skipped (never replayed from the
// beginning). The quota is decremented once publications are delivered, so a
// single Connect/Subscribe request shares the MaxRecoveredPublications cap
// across all of its channels.
func (n *Node) recoverSubscription(
	ctx context.Context,
	sub *clientpb.Subscription,
	snapshot *ClusterSessionSnapshot, // nil = 非 resume
	quota *recoverQuota,
	path string,
) ChannelRecovery {
	currentEpoch := ""
	if epocher, ok := n.broker.(interface{ Epoch() string }); ok {
		currentEpoch = epocher.Epoch()
	}
	resume := snapshot != nil

	if sub == nil || sub.Channel == "" {
		// Defensive: callers only pass valid subscriptions; an empty channel
		// must never reach History.
		res := ChannelRecovery{Status: RecoverSkipped, Offset: 0, Epoch: currentEpoch}
		if sub != nil {
			res.Channel = sub.Channel
			if sub.Recover {
				res.Err = errors.New("recovery skipped: empty channel")
			}
		}
		return n.finishRecovery(ctx, res, path)
	}

	res := ChannelRecovery{Channel: sub.Channel, Epoch: currentEpoch}

	// Skip cursor definition (§5.1): resume without a snapshot offset is 0;
	// non-resume echoes the client offset.
	cursor := uint64(0)
	if !resume {
		cursor = sub.Offset
	}
	res.Offset = cursor

	// §5.1 Skip gates: History is never called.
	if skipReason := n.recoverSkipReason(sub, snapshot, resume); skipReason != "" {
		res.Status = RecoverSkipped
		if sub.Recover {
			res.Err = fmt.Errorf("recovery skipped: %s", skipReason)
		}
		return n.finishRecovery(ctx, res, path)
	}

	// §5.2 cursor and sinceOffset.
	sinceOffset := uint64(0)
	epochReset := false
	if resume {
		serverOffset := snapshot.ChannelOffsets[sub.Channel]
		if snapshot.BrokerEpoch != "" && currentEpoch != "" && snapshot.BrokerEpoch != currentEpoch {
			// Both epochs are known and differ: the recorded offsets belong
			// to an invalidated history generation, recover from scratch.
			epochReset = true
			cursor = 0
			sinceOffset = 0
		} else {
			// Epoch matches or one side cannot prove invalidation: continue
			// pulling from the server-recorded offset.
			cursor = serverOffset
			sinceOffset = serverOffset + 1
		}
	} else if currentEpoch != "" && sub.Epoch != currentEpoch {
		if sub.Epoch == "" {
			log.WarnContext(ctx, "client sent no epoch but broker epoch is set; recovering from the beginning",
				"channel", sub.Channel, "broker_epoch", currentEpoch)
		}
		epochReset = true
		cursor = 0
		sinceOffset = 0
	} else if sub.Offset == 0 {
		// KD-2: a fresh (non-resume) recover with offset 0 starts from the
		// beginning. This is the only path that replays full history.
		cursor = 0
		sinceOffset = 0
	} else {
		cursor = sub.Offset
		sinceOffset = sub.Offset + 1
	}
	res.Offset = cursor

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
		return n.finishRecovery(ctx, res, path)
	}

	historyPubs, err := n.broker.History(sub.Channel, sinceOffset, limit)
	if err != nil {
		// The subscription is already committed and must not be rolled back:
		// a history hiccup must not prevent the client from entering the
		// channel (KD-9). Surface the failure in RecoverResult instead.
		res.Status = RecoverFailed
		res.Err = err
		return n.finishRecovery(ctx, res, path)
	}

	for _, pub := range historyPubs {
		res.Publications = append(res.Publications, publicationToClient(sub.Channel, pub))
	}
	if len(historyPubs) > 0 {
		// §5.4: the last delivered publication's offset, not the cursor.
		res.Offset = historyPubs[len(historyPubs)-1].Offset
		quota.remaining -= len(historyPubs)
	}

	if len(historyPubs) == limit {
		// Conservative: a full batch means the broker may have more.
		res.Status = RecoverTruncated
	} else if epochReset {
		res.Status = RecoverEpochReset
	} else {
		res.Status = RecoverOK
	}
	return n.finishRecovery(ctx, res, path)
}

// recoverSkipReason returns why recovery is skipped for sub, or "" when
// History may be called. See §5.1 for the gate order.
func (n *Node) recoverSkipReason(sub *clientpb.Subscription, snapshot *ClusterSessionSnapshot, resume bool) string {
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
	if resume {
		if _, ok := snapshot.ChannelOffsets[sub.Channel]; !ok {
			// The snapshot deliberately omits channels that never delivered
			// history: replaying from the client offset could flood 1000
			// messages on every takeover.
			return "resume snapshot has no server-recorded offset"
		}
	}
	return ""
}

// finishRecovery records the metrics and log line for one recovery attempt.
func (n *Node) finishRecovery(ctx context.Context, res ChannelRecovery, path string) ChannelRecovery {
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
		n.metrics.RecoveryPublications.WithLabelValues(path).Observe(float64(len(res.Publications)))
		if res.Status == RecoverTruncated {
			n.metrics.RecoveryTruncatedTotal.WithLabelValues(path).Inc()
		}
	}

	fields := []any{
		"channel", res.Channel,
		"status", res.Status.String(),
		"count", len(res.Publications),
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

// RecoverResult converts the channel recovery into the client protocol
// RecoverResult envelope. RECOVER_FAILED and RECOVER_SKIPPED errors appear
// per-channel; a skip without a client recovery request carries no error.
func (r ChannelRecovery) RecoverResult() *clientpb.RecoverResult {
	res := &clientpb.RecoverResult{
		Channel:   r.Channel,
		Recovered: r.Status == RecoverOK || r.Status == RecoverTruncated || r.Status == RecoverEpochReset,
		Truncated: r.Status == RecoverTruncated,
		Offset:    r.Offset,
		Epoch:     r.Epoch,
	}
	switch r.Status {
	case RecoverFailed:
		res.Error = &sharedpb.Error{
			Code:    "RECOVER_FAILED",
			Type:    "recover_error",
			Message: r.Err.Error(),
		}
	case RecoverSkipped:
		if r.Err != nil {
			res.Error = &sharedpb.Error{
				Code:    "RECOVER_SKIPPED",
				Type:    "recover_error",
				Message: r.Err.Error(),
			}
		}
	}
	return res
}

// publicationToClient converts one broker publication into the client
// protocol Publication envelope. Realtime delivery and recovery share the
// stable channel-offset message ID so clients can deduplicate.
func publicationToClient(channel string, pub *Publication) *clientpb.Publication {
	return &clientpb.Publication{
		Messages: []*clientpb.Message{
			{
				Id:      publicationID(channel, pub.Offset),
				Channel: channel,
				Offset:  pub.Offset,
				Payload: pub.PayloadProto(),
				Metadata: func() *sharedpb.Metadata {
					if len(pub.Metadata) == 0 {
						return nil
					}
					return &sharedpb.Metadata{Entries: pub.Metadata}
				}(),
			},
		},
	}
}
