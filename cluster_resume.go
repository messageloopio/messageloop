package messageloop

import (
	"context"
	"errors"
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
		ExpiresAt:      time.Now().Add(defaultClusterSessionLeaseTTL),
	}
	claimed, err := directory.CompareAndSwapSessionLease(ctx, lease, desired, defaultClusterSessionLeaseTTL)
	if err != nil {
		return nil, false, err
	}
	if !claimed {
		return nil, false, DisconnectStale
	}

	if lease.NodeID != "" && lease.IncarnationID != "" && (lease.NodeID != n.ClusterNodeID() || lease.IncarnationID != n.ClusterIncarnationID()) {
		if err := n.requestSessionTakeover(ctx, lease); err != nil {
			nodeLease, leaseErr := directory.GetNodeLease(ctx, lease.NodeID, lease.IncarnationID)
			if leaseErr != nil {
				return nil, false, leaseErr
			}
			if nodeLease != nil {
				return nil, false, err
			}
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

func (n *Node) restoreSessionSubscriptions(ctx context.Context, client *Client, subscriptions []ClusterSubscriptionSnapshot) error {
	restored := make([]string, 0, len(subscriptions))
	for _, sub := range subscriptions {
		if err := n.restoreLocalSubscription(ctx, sub.Channel, NewSubscriber(client, sub.Ephemeral)); err != nil {
			n.rollbackRestoredSubscriptions(client, restored)
			return err
		}
		// shouldTrackPresence gates the restore exactly like every other
		// presence writer: wildcard patterns, ephemeral subscriptions and
		// presence=false channels never enter the store. Restore never emits
		// join events — members already in the channel must not see a
		// duplicate join for a resumed session.
		if n.shouldTrackPresence(sub.Channel, sub.Ephemeral) {
			if err := n.SetPresenceForSession(ctx, sub.Channel, client); err != nil {
				n.rollbackRestoredSubscriptions(client, append(restored, sub.Channel))
				return err
			}
		}
		restored = append(restored, sub.Channel)
		n.adjustClusterChannelSubscriptionsTimeout(sub.Channel, 1)
	}
	return nil
}

// rollbackRestoredSubscriptions undoes restored subscriptions after a partial
// restore failure, compensating the shared channel projection for each channel
// that was actually removed and clearing the presence entries the restore
// path added.
func (n *Node) rollbackRestoredSubscriptions(client *Client, channels []string) {
	for _, channel := range channels {
		removed, _ := n.removeLocalSubscriptionOnly(channel, client, true)
		if removed {
			n.adjustClusterChannelSubscriptionsTimeout(channel, -1)
			// A partial restore must not leave a ghost online member behind.
			// Remove on a channel that never registered presence is a no-op.
			ctx, cancel := context.WithTimeout(context.Background(), clusterProjectionAdjustTimeout)
			_ = n.presence.Remove(ctx, channel, client.SessionID())
			cancel()
		}
	}
}

func (n *Node) restoreLocalSubscription(ctx context.Context, ch string, sub Subscriber) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	if _, exists := n.hub.LookupSubscriber(ch, sub.Client); exists {
		return nil
	}

	first, err := n.hub.addSub(ch, sub)
	if err != nil {
		return err
	}
	if first {
		if err := n.broker.Subscribe(ch); err != nil {
			n.hub.removeSub(ch, sub.Client)
			return err
		}
		if n.metrics != nil {
			n.metrics.ActiveChannels.Inc()
		}
	}
	sub.Client.mu.Lock()
	sub.Client.subscribedChannels[ch] = struct{}{}
	sub.Client.mu.Unlock()
	if n.metrics != nil {
		n.metrics.SubscriptionsTotal.Inc()
	}
	return nil
}

// removeLocalSubscriptionOnly removes one local subscription without touching
// the shared channel projection. The returned bool reports whether the
// subscription was removed from the hub (true even when the subsequent broker
// Unsubscribe fails, since the hub state has already been mutated).
func (n *Node) removeLocalSubscriptionOnly(ch string, client *Client, updateMetrics bool) (bool, error) {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	last, removed := n.hub.removeSub(ch, client)
	if !removed {
		return false, nil
	}
	client.mu.Lock()
	delete(client.subscribedChannels, ch)
	client.mu.Unlock()
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

func (n *Node) evictSessionForTakeover(client *Client) error {
	client.mu.Lock()
	if client.status == statusClosed {
		client.mu.Unlock()
		return nil
	}
	client.status = statusClosed
	if client.heartbeatCancel != nil {
		client.heartbeatCancel()
		client.heartbeatCancel = nil
	}
	channels := make([]string, 0, len(client.subscribedChannels))
	for ch := range client.subscribedChannels {
		channels = append(channels, ch)
	}
	sessionID := client.session
	client.mu.Unlock()

	// Remove every channel even when individual removals fail, so no channel
	// is left behind; track which channels were actually removed from the hub
	// for rollback and shared projection adjustment.
	var evictErrs []error
	removed := make([]string, 0, len(channels))
	ephemeralByChannel := make(map[string]bool, len(channels))
	for _, ch := range channels {
		if stored, ok := n.hub.LookupSubscriber(ch, client); ok {
			ephemeralByChannel[ch] = stored.Ephemeral
		}
		wasRemoved, err := n.removeLocalSubscriptionOnly(ch, client, true)
		if err != nil {
			evictErrs = append(evictErrs, fmt.Errorf("remove channel %s: %w", ch, err))
		}
		if !wasRemoved {
			continue
		}
		removed = append(removed, ch)
		n.adjustClusterChannelSubscriptionsTimeout(ch, -1)
	}

	if len(evictErrs) > 0 {
		// Roll back every removed channel (and the projection deltas) so the
		// session is not left half-evicted, then report the aggregated error.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), clusterEvictRollbackTimeout)
		for _, ch := range removed {
			if err := n.restoreLocalSubscription(rollbackCtx, ch, NewSubscriber(client, ephemeralByChannel[ch])); err != nil {
				evictErrs = append(evictErrs, fmt.Errorf("rollback channel %s: %w", ch, err))
			}
			n.adjustClusterChannelSubscriptionsTimeout(ch, 1)
		}
		cancel()
		return errors.Join(evictErrs...)
	}

	if sessionID != "" {
		// Only evict the session when the registered client is still this
		// one: a newer connection may have taken over the session ID between
		// LookupSession and this removal, and RemoveSessionIfMatches
		// protects it from being evicted by a stale takeover.
		n.hub.RemoveSessionIfMatches(sessionID, client)
	}
	return client.transport.Close(Disconnect{})
}
