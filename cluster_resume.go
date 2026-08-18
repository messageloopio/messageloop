package messageloop

import (
	"context"
	"fmt"
	"time"

	"github.com/lynx-go/x/log"
)

const (
	clusterCommandMetaNewNodeID        = "new_node_id"
	clusterCommandMetaNewIncarnationID = "new_incarnation_id"

	// clusterEvictRollbackTimeout bounds the re-subscription rollback after a
	// partially failed session takeover eviction.
	clusterEvictRollbackTimeout = 5 * time.Second
	// clusterProjectionAdjustTimeout bounds each shared channel projection
	// adjustment; failures are logged but never block the eviction/restore path.
	clusterProjectionAdjustTimeout = 2 * time.Second
)

// adjustClusterChannelSubscriptionsTimeout adjusts the shared channel
// projection with a short timeout, logging failures instead of blocking.
func (n *Node) adjustClusterChannelSubscriptionsTimeout(channel string, delta int64) {
	ctx, cancel := context.WithTimeout(context.Background(), clusterProjectionAdjustTimeout)
	defer cancel()
	if err := n.adjustClusterChannelSubscriptions(ctx, channel, delta); err != nil {
		log.WarnContext(ctx, "failed to adjust cluster channel subscriptions", "channel", channel, "delta", delta, "error", err)
	}
}

func (n *Node) resumeRemoteSession(ctx context.Context, client *Client, sessionID string) (*ClusterSessionSnapshot, bool, error) {
	if !n.ClusterEnabled() || sessionID == "" {
		return nil, false, nil
	}

	directory := n.clusterSessionDirectory()
	lease, err := directory.GetSessionLease(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	if lease == nil {
		return nil, false, nil
	}

	snapshot, err := directory.GetSessionSnapshot(ctx, sessionID)
	if err != nil {
		return nil, false, err
	}
	if snapshot == nil {
		return nil, false, nil
	}

	// Claim the lease atomically with CompareAndSwapSessionLease: another
	// node may have taken over the session while this resume was in flight.
	// A failed CAS aborts the resume without issuing a takeover command or
	// restoring any subscription state.
	desired := &ClusterSessionLease{
		SessionID:      lease.SessionID,
		NodeID:         n.ClusterNodeID(),
		IncarnationID:  n.ClusterIncarnationID(),
		UserID:         lease.UserID,
		ClientID:       lease.ClientID,
		LeaseVersion:   lease.LeaseVersion + 1,
		Authenticated:  lease.Authenticated,
		ConnectedAt:    lease.ConnectedAt,
		LastActivityAt: time.Now().UnixMilli(),
		ExpiresAt:      time.Now().Add(n.sessionLeaseTTL()),
	}
	// The dual-activation window starts at the claim: from the CAS below to
	// the end of the takeover branch (including the KD-K30 dead-node bypass),
	// both the old remote attachment and this one may be live.
	dualActivationStart := time.Now()
	claimed, err := directory.CompareAndSwapSessionLease(ctx, lease, desired, n.sessionLeaseTTL())
	if err != nil {
		return nil, false, err
	}
	if !claimed {
		if n.metrics != nil {
			n.metrics.BindFencedTotal.Inc()
		}
		return nil, false, DisconnectStale
	}

	if lease.NodeID != "" && lease.IncarnationID != "" && (lease.NodeID != n.ClusterNodeID() || lease.IncarnationID != n.ClusterIncarnationID()) {
		// Same nodeID with a strictly newer node epoch (PR-KA-D10 §1.3, the
		// C2 deferral): this process is a newer generation of the recorded
		// owner. INCR allocation is monotonic (KD-K27), so the old generation
		// is dead and a takeover RPC against it is doomed to fall into the
		// KD-K30 dead-node bypass — skip it and continue with the claimed
		// lease. Non-epoch incarnation IDs (test-injected "inc-a" and the
		// like) never parse as epochs and never skip: their behavior is
		// unchanged.
		skipTakeover := lease.NodeID == n.ClusterNodeID() && NodeEpochNewer(n.ClusterIncarnationID(), lease.IncarnationID)
		if !skipTakeover {
			evictStart := time.Now()
			takeoverErr := n.requestSessionTakeover(ctx, lease)
			if n.metrics != nil {
				n.metrics.EvictLag.Observe(time.Since(evictStart).Seconds())
			}
			if takeoverErr != nil {
				nodeLease, leaseErr := directory.GetNodeLease(ctx, lease.NodeID, lease.IncarnationID)
				if leaseErr != nil {
					// Still attempt the rollback before reporting the lease
					// lookup failure.
					n.rollbackSessionTakeover(ctx, directory, desired, lease)
					return nil, false, leaseErr
				}
				if nodeLease != nil {
					// The old node is still alive (the KD-K30 dead-node bypass
					// does not apply): give the fencing back so the directory
					// keeps recognizing the old owner instead of a takeover that
					// never completed.
					n.rollbackSessionTakeover(ctx, directory, desired, lease)
					return nil, false, takeoverErr
				}
				// nodeLease == nil: the old node is dead (KD-K30). Keep the new
				// CAS and continue the resume.
			}
		}
		if n.metrics != nil {
			n.metrics.SessionDualActivationSeconds.Observe(time.Since(dualActivationStart).Seconds())
		}
	}

	client.mu.Lock()
	client.session = sessionID
	if snapshot.UserID != "" {
		client.user = snapshot.UserID
	}
	if snapshot.ClientID != "" {
		client.client = snapshot.ClientID
	}
	client.subscribedChannels = make(map[string]struct{}, len(snapshot.Subscriptions))
	for _, sub := range snapshot.Subscriptions {
		client.subscribedChannels[sub.Channel] = struct{}{}
	}
	if lease.LeaseVersion > 0 {
		client.clusterLeaseVersion = lease.LeaseVersion + 1
	} else if client.clusterLeaseVersion == 0 {
		client.clusterLeaseVersion = 1
	}
	client.mu.Unlock()

	return snapshot, true, nil
}

// rollbackSessionTakeover returns the session lease to its pre-takeover owner
// after a failed takeover: the fencing claimed by the CAS is CAS'd back to the
// original record (claimed and original must not be swapped). A failed
// rollback is logged and never treated as a successful resume — the caller
// still returns the original takeover error.
func (n *Node) rollbackSessionTakeover(ctx context.Context, directory SessionDirectory, claimed, original *ClusterSessionLease) {
	if _, err := directory.CompareAndSwapSessionLease(ctx, claimed, original, n.sessionLeaseTTL()); err != nil {
		log.ErrorContext(ctx, "failed to roll back session takeover lease", err, "session", claimed.SessionID)
	}
}

func (n *Node) requestSessionTakeover(ctx context.Context, lease *ClusterSessionLease) error {
	result, err := n.clusterCommandBus().SendCommand(ctx, &ClusterCommand{
		CommandID:           "",
		Type:                ClusterCommandTakeover,
		TargetNodeID:        lease.NodeID,
		TargetIncarnationID: lease.IncarnationID,
		SessionID:           lease.SessionID,
		LeaseVersion:        lease.LeaseVersion,
		Metadata: map[string]string{
			clusterCommandMetaNewNodeID:        n.ClusterNodeID(),
			clusterCommandMetaNewIncarnationID: n.ClusterIncarnationID(),
		},
	})
	if err != nil {
		return err
	}
	if result == nil || result.Status == ClusterCommandStatusSucceeded || result.ErrorCode == "SESSION_NOT_FOUND" {
		return nil
	}
	return fmt.Errorf("takeover command failed: %s", result.ErrorMessage)
}

// clusterRestoreFailure records one snapshot channel whose restore failed
// during a cross-node resume hydrate (PR-KA-D10 §1.1). The channel is not
// restored; the session stays alive with the channels that did restore, and
// the client learns about the failure from a per-channel RECOVER_FAILED
// envelope sent after Connected (finishConnect).
type clusterRestoreFailure struct {
	channel string
	err     error
}

// restoreSessionSubscriptions re-creates the snapshot's subscriptions one
// channel at a time. There is no saga and no rollback (PR-KA-D10 §1.1): a
// channel whose restore or presence registration fails is not restored — it
// is recorded in the returned failure list and the remaining channels
// continue; channels that already restored stay restored.
func (n *Node) restoreSessionSubscriptions(ctx context.Context, client *Client, subscriptions []ClusterSubscriptionSnapshot) []clusterRestoreFailure {
	var failures []clusterRestoreFailure
	for _, sub := range subscriptions {
		if err := n.restoreLocalSubscription(ctx, sub.Channel, NewSubscriber(client, sub.Ephemeral)); err != nil {
			log.WarnContext(ctx, "failed to restore subscription for resumed session",
				"channel", sub.Channel, "session", client.SessionID(), "error", err)
			// resumeRemoteSession pre-seeds the session's channel set from the
			// snapshot; an unrestored channel must leave it again so the
			// session view (and Connected.Subscriptions) reflects reality.
			client.mu.Lock()
			delete(client.subscribedChannels, sub.Channel)
			client.mu.Unlock()
			failures = append(failures, clusterRestoreFailure{channel: sub.Channel, err: err})
			continue
		}
		// shouldTrackPresence gates the restore exactly like every other
		// presence writer: wildcard patterns, ephemeral subscriptions and
		// presence=false channels never enter the store. Restore never emits
		// join events — members already in the channel must not see a
		// duplicate join for a resumed session.
		if n.shouldTrackPresence(sub.Channel, sub.Ephemeral) {
			if err := n.SetPresenceForSession(ctx, sub.Channel, client); err != nil {
				log.WarnContext(ctx, "failed to restore presence for resumed session",
					"channel", sub.Channel, "session", client.SessionID(), "error", err)
				// The channel must end up fully unrestored: undo the
				// subscription just added (the projection +1 below never ran
				// for it, so no compensation is owed). Other restored
				// channels are untouched.
				if _, rmErr := n.removeLocalSubscriptionOnly(sub.Channel, client, true); rmErr != nil {
					log.WarnContext(ctx, "failed to undo subscription after presence restore failure",
						"channel", sub.Channel, "session", client.SessionID(), "error", rmErr)
				}
				failures = append(failures, clusterRestoreFailure{channel: sub.Channel, err: err})
				continue
			}
		}
		n.adjustClusterChannelSubscriptionsTimeout(sub.Channel, 1)
	}
	return failures
}

func (n *Node) restoreLocalSubscription(ctx context.Context, ch string, sub Subscriber) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	if _, exists := n.hub.LookupSubscriber(ch, sub.Session); exists {
		return nil
	}

	first, err := n.hub.addSub(ch, sub)
	if err != nil {
		return err
	}
	if first {
		if err := n.broker.Subscribe(ch); err != nil {
			n.hub.removeSub(ch, sub.Session)
			return err
		}
		if n.metrics != nil {
			n.metrics.ActiveChannels.Inc()
		}
	}
	sub.Session.mu.Lock()
	sub.Session.subscribedChannels[ch] = struct{}{}
	sub.Session.mu.Unlock()
	if n.metrics != nil {
		n.metrics.SubscriptionsTotal.Inc()
	}
	return nil
}

// removeLocalSubscriptionOnly removes one local subscription without touching
// the shared channel projection. The returned bool reports whether the
// subscription was removed from the hub (true even when the subsequent broker
// Unsubscribe fails, since the hub state has already been mutated).
func (n *Node) removeLocalSubscriptionOnly(ch string, session *Session, updateMetrics bool) (bool, error) {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	last, removed := n.hub.removeSub(ch, session)
	if !removed {
		return false, nil
	}
	session.mu.Lock()
	delete(session.subscribedChannels, ch)
	session.mu.Unlock()
	if last {
		if err := n.broker.Unsubscribe(ch); err != nil {
			return true, err
		}
		if updateMetrics && n.metrics != nil {
			n.metrics.ActiveChannels.Dec()
		}
	}
	if updateMetrics && n.metrics != nil {
		n.metrics.SubscriptionsTotal.Dec()
	}
	return true, nil
}
