package messageloop

import (
	"context"
	"fmt"
)

const (
	clusterCommandMetaNewNodeID        = "new_node_id"
	clusterCommandMetaNewIncarnationID = "new_incarnation_id"
)

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
			for _, channel := range restored {
				_ = n.removeLocalSubscriptionOnly(channel, client, true)
			}
			return err
		}
		if err := n.SetPresenceForSession(ctx, sub.Channel, client); err != nil {
			for _, channel := range restored {
				_ = n.removeLocalSubscriptionOnly(channel, client, true)
			}
			_ = n.removeLocalSubscriptionOnly(sub.Channel, client, true)
			return err
		}
		restored = append(restored, sub.Channel)
	}
	return nil
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
	}
	if n.metrics != nil {
		n.metrics.SubscriptionsTotal.Inc()
	}
	return nil
}

func (n *Node) removeLocalSubscriptionOnly(ch string, client *Client, updateMetrics bool) error {
	mu := n.subLock(ch)
	mu.Lock()
	defer mu.Unlock()

	last, removed := n.hub.removeSub(ch, client)
	if !removed {
		return nil
	}
	client.mu.Lock()
	delete(client.subscribedChannels, ch)
	client.mu.Unlock()
	if last {
		if err := n.broker.Unsubscribe(ch); err != nil {
			return err
		}
	}
	if updateMetrics && n.metrics != nil {
		n.metrics.SubscriptionsTotal.Dec()
	}
	return nil
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

	for _, ch := range channels {
		if err := n.removeLocalSubscriptionOnly(ch, client, true); err != nil {
			return err
		}
	}
	if sessionID != "" {
		n.hub.RemoveSession(sessionID)
	}
	return client.transport.Close(Disconnect{})
}
