package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evictTestBroker tracks broker-side subscribe/unsubscribe bookkeeping and can
// fail Unsubscribe for one specific channel (channel iteration order over the
// client's subscription map is nondeterministic, so failure is keyed by name).
// Subscribe/Unsubscribe are idempotent, mirroring real brokers (Redis set
// semantics / memory broker no-ops): a failed Unsubscribe leaves state intact.
type evictTestBroker struct {
	mu          sync.Mutex
	failUnsubCh string
	subscribed  map[string]bool
	transients  []string
}

func (b *evictTestBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *evictTestBroker) Subscribe(ch string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribed[ch] = true
	return nil
}
func (b *evictTestBroker) Unsubscribe(ch string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch == b.failUnsubCh {
		return errors.New("unsubscribe failed")
	}
	delete(b.subscribed, ch)
	return nil
}
func (b *evictTestBroker) Publish(string, *Publication) (uint64, error) { return 0, nil }
func (b *evictTestBroker) PublishTransient(ch string, _ *Publication) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transients = append(b.transients, ch)
	return nil
}
func (b *evictTestBroker) PublishOccupancy(string, OccupancyEvent) error { return nil }
func (b *evictTestBroker) SetOccupancyHandler(OccupancyHandler) error    { return nil }
func (b *evictTestBroker) SetGapHandler(GapHandler)                      {}
func (b *evictTestBroker) History(string, uint64, int) (*HistoryPage, error) { return nil, nil }

// projectionQueryStore accumulates shared channel projection deltas and lists
// them back, mimicking the cluster query store used by Channels().
type projectionQueryStore struct {
	mu     sync.Mutex
	deltas map[string]int64
}

func (s *projectionQueryStore) Start(context.Context) error    { return nil }
func (s *projectionQueryStore) Shutdown(context.Context) error { return nil }
func (s *projectionQueryStore) AdjustChannelSubscriptions(_ context.Context, channel string, delta int64, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deltas[channel] += delta
	return nil
}
func (s *projectionQueryStore) ReplaceNodeChannels(context.Context, map[string]int64, time.Duration) error {
	return nil
}
func (s *projectionQueryStore) ListChannels(context.Context) ([]ClusterChannelInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := make([]ClusterChannelInfo, 0, len(s.deltas))
	for channel, subscribers := range s.deltas {
		info = append(info, ClusterChannelInfo{Name: channel, Subscribers: subscribers})
	}
	return info, nil
}
func (s *projectionQueryStore) ListNodeProjections(context.Context) ([]ClusterNodeProjection, error) {
	return nil, nil
}
func (s *projectionQueryStore) DeleteNodeProjection(context.Context, string, string) error {
	return nil
}

func TestNode_EvictSessionForTakeover_RollsBackPartiallyRemovedChannels(t *testing.T) {
	broker := &evictTestBroker{failUnsubCh: "evict.ch.2", subscribed: make(map[string]bool)}
	node := NewNode(nil)
	node.SetBroker(broker)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-evict", "user-evict", "client-evict")
	require.NoError(t, node.AddClient(client))

	for _, ch := range []string{"evict.ch.1", "evict.ch.2", "evict.ch.3"} {
		require.NoError(t, node.AddSubscription(context.Background(), ch, NewSubscriber(client, false)))
	}

	err = client.Fence(DisconnectStale)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsubscribe failed")

	// Every channel must be rolled back: no half-evicted state remains.
	for _, ch := range []string{"evict.ch.1", "evict.ch.2", "evict.ch.3"} {
		_, exists := node.hub.LookupSubscriber(ch, client)
		assert.True(t, exists, "channel %s should be restored in the hub", ch)
		assert.True(t, client.HasSubscription(ch), "client should track channel %s", ch)
	}

	// Broker-side bookkeeping matches the hub state again.
	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, ch := range []string{"evict.ch.1", "evict.ch.2", "evict.ch.3"} {
		assert.True(t, broker.subscribed[ch], "broker should be subscribed to %s", ch)
	}
}

func TestNode_EvictSessionForTakeover_RollbackPreservesEphemeral(t *testing.T) {
	broker := &evictTestBroker{failUnsubCh: "evict.eph.fail", subscribed: make(map[string]bool)}
	node := NewNode(nil)
	node.SetBroker(broker)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-evict-eph", "user-evict", "client-evict")
	require.NoError(t, node.AddClient(client))

	require.NoError(t, node.AddSubscription(context.Background(), "evict.eph", NewSubscriber(client, true)))
	require.NoError(t, node.AddSubscription(context.Background(), "evict.eph.fail", NewSubscriber(client, true)))

	err = client.Fence(DisconnectStale)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsubscribe failed")

	for _, ch := range []string{"evict.eph", "evict.eph.fail"} {
		stored, exists := node.hub.LookupSubscriber(ch, client)
		require.True(t, exists, "channel %s should be restored in the hub", ch)
		require.True(t, stored.Ephemeral, "rollback must preserve the ephemeral flag of channel %s", ch)
	}

	require.NoError(t, client.HandleUnsubscribe(context.Background(), &clientpb.InboundMessage{
		Id: "msg-1",
	}, &clientpb.Unsubscribe{
		Subscriptions: []*clientpb.Subscription{{Channel: "evict.eph"}},
	}))

	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, ch := range broker.transients {
		assert.NotEqual(t, presenceChannel("evict.eph"), ch,
			"ephemeral subscription must not publish a presence leave event")
	}
	assert.False(t, broker.subscribed["evict.eph"], "unsubscribe must remove the channel from the broker")
}

func TestNode_EvictSessionForTakeover_AdjustsSharedProjection(t *testing.T) {
	store := &projectionQueryStore{deltas: make(map[string]int64)}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: &fakeSessionDirectory{},
		QueryStore:       store,
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	clientA, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-a", "user-a", "client-a")
	require.NoError(t, node.AddClient(clientA))
	require.NoError(t, node.AddSubscription(context.Background(), "news", NewSubscriber(clientA, false)))
	require.NoError(t, node.AddSubscription(context.Background(), "alerts", NewSubscriber(clientA, false)))

	clientB, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	clientB.ForceTestIDs("sess-b", "user-b", "client-b")
	require.NoError(t, node.AddClient(clientB))
	require.NoError(t, node.AddSubscription(context.Background(), "news", NewSubscriber(clientB, false)))

	require.NoError(t, clientA.Fence(DisconnectStale))

	channels, err := node.Channels(context.Background())
	require.NoError(t, err)
	assertProjectionCount := func(name string, want int64) {
		for _, ch := range channels {
			if ch.Name == name {
				assert.Equal(t, want, int64(ch.Subscribers), "projection count for %s", name)
				return
			}
		}
		assert.Fail(t, "channel %s missing from projection", name)
	}
	assertProjectionCount("news", 1)
	assertProjectionCount("alerts", 0)

	assert.Nil(t, node.hub.LookupSession("sess-a"))
}

func TestNode_RestoreSessionSubscriptions_AdjustsSharedProjection(t *testing.T) {
	store := &projectionQueryStore{deltas: make(map[string]int64)}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: &fakeSessionDirectory{},
		QueryStore:       store,
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-restore", "user-restore", "client-restore")

	subscriptions := []ClusterSubscriptionSnapshot{{Channel: "news"}, {Channel: "sports"}}
	require.Empty(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

	channels, err := node.Channels(context.Background())
	require.NoError(t, err)
	for _, name := range []string{"news", "sports"} {
		found := false
		for _, ch := range channels {
			if ch.Name == name {
				assert.Equal(t, int64(1), int64(ch.Subscribers), "projection count for %s", name)
				found = true
				break
			}
		}
		assert.True(t, found, "channel %s missing from projection", name)
	}
	assert.True(t, client.HasSubscription("news"))
	assert.True(t, client.HasSubscription("sports"))
}

// Task 10: remote resume must claim the session lease via CAS with the old
// lease version as the expected value.
func TestResumeRemoteSession_UsesCAS(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-remote",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	snapshot, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-remote")
	require.NoError(t, err)
	require.True(t, resumed)
	require.NotNil(t, snapshot)
	require.GreaterOrEqual(t, directory.casCalls, 1, "resume must claim the lease via CAS")
	require.Equal(t, uint64(7), directory.casExpected.LeaseVersion, "CAS expected value must be the old lease version")
	require.Equal(t, uint64(8), directory.casDesired.LeaseVersion, "CAS desired value must bump the lease version")
	require.Equal(t, "node-a", directory.casDesired.NodeID)
	require.Equal(t, "inc-a", directory.casDesired.IncarnationID)
}

// Task 10: when the CAS fails (another node already took over the session),
// the resume must abort: no takeover command, no state inheritance, and the
// new connection is rejected with a disconnect.
func TestResumeRemoteSession_CASConflictAborts(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-remote",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
		forceCasFail: true,
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	_, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-remote")
	require.Error(t, err)
	var dis Disconnect
	require.True(t, errors.As(err, &dis), "resume conflict must surface as a disconnect")
	require.Equal(t, DisconnectStale.Code, dis.Code)
	require.False(t, resumed)
	require.Empty(t, bus.commands, "no takeover command may be issued after a CAS conflict")
	require.False(t, client.HasSubscription("news"), "no subscriptions may be restored after a CAS conflict")
}
// failSubscribeBroker fails every Subscribe so remote subscription restore
// aborts midway.
type failSubscribeBroker struct{}

func (b *failSubscribeBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *failSubscribeBroker) Subscribe(ch string) error                        { return errors.New("injected subscribe failure") }
func (b *failSubscribeBroker) Unsubscribe(ch string) error                      { return nil }
func (b *failSubscribeBroker) Publish(string, *Publication) (uint64, error)     { return 0, nil }
func (b *failSubscribeBroker) PublishTransient(string, *Publication) error      { return nil }
func (b *failSubscribeBroker) PublishOccupancy(string, OccupancyEvent) error    { return nil }
func (b *failSubscribeBroker) SetOccupancyHandler(OccupancyHandler) error       { return nil }
func (b *failSubscribeBroker) SetGapHandler(GapHandler)                         {}
func (b *failSubscribeBroker) History(string, uint64, int) (*HistoryPage, error) {
	return nil, nil
}

// failChannelSubscribeBroker fails Subscribe for one specific channel, so a
// remote resume hydrate fails that channel only (PR-KA-D10 §1.1).
type failChannelSubscribeBroker struct {
	failCh string
}

func (b *failChannelSubscribeBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *failChannelSubscribeBroker) Subscribe(ch string) error {
	if ch == b.failCh {
		return errors.New("injected subscribe failure")
	}
	return nil
}
func (b *failChannelSubscribeBroker) Unsubscribe(string) error                     { return nil }
func (b *failChannelSubscribeBroker) Publish(string, *Publication) (uint64, error) { return 0, nil }
func (b *failChannelSubscribeBroker) PublishTransient(string, *Publication) error  { return nil }
func (b *failChannelSubscribeBroker) PublishOccupancy(string, OccupancyEvent) error {
	return nil
}
func (b *failChannelSubscribeBroker) SetOccupancyHandler(OccupancyHandler) error { return nil }
func (b *failChannelSubscribeBroker) SetGapHandler(GapHandler)                   {}
func (b *failChannelSubscribeBroker) History(string, uint64, int) (*HistoryPage, error) {
	return nil, nil
}

// resumeSoftFailFixture wires a RequireAuth node with a recording directory
// holding one remotely-owned session and runs a resume Connect for it.
func resumeSoftFailFixture(t *testing.T, snapshot *ClusterSessionSnapshot, broker Broker) (*Node, *Session, *capturingTransport, *recordingSessionDirectory) {
	t.Helper()
	ctx := context.Background()
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  3,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: snapshot,
	}}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(&config.Server{RequireAuth: true})
	node.SetCluster(runtime)
	node.SetBroker(broker)
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	resumeMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				Version:   testProtocolVersion,
				ClientId:  "client-1",
				Token:     "ok-token",
				SessionId: "sess-remote",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, resumeMsg))
	return node, client, transport, directory
}

// recoverFailedEnvelopes returns every top-level RECOVER_FAILED error
// envelope captured on the transport, in arrival order.
func recoverFailedEnvelopes(t *testing.T, transport *capturingTransport) []*sharedv2.Error {
	t.Helper()
	var errs []*sharedv2.Error
	for _, data := range transport.snapshotMessages() {
		var out clientpb.OutboundMessage
		require.NoError(t, (JSONMarshaler{}).Unmarshal(data, &out))
		if e := out.GetError(); e != nil && e.GetCode() == "RECOVER_FAILED" {
			errs = append(errs, e)
		}
	}
	return errs
}

// PR-KA-D10 §1.1: a per-channel hydrate failure is soft — the failed channel
// is skipped, every other channel stays restored, the session survives (no
// hub removal, no lease/snapshot delete, no 3502), and the client receives a
// RECOVER_FAILED envelope naming the failed channel after Connected.
func TestClient_RemoteResume_RestorePartialFailureKeepsSession(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID: "sess-remote",
		UserID:    "user-1",
		ClientID:  "client-1",
		Subscriptions: []ClusterSubscriptionSnapshot{
			{Channel: "news"},
			{Channel: "broken.ch"},
		},
	}
	node, client, transport, directory := resumeSoftFailFixture(t, snapshot, &failChannelSubscribeBroker{failCh: "broken.ch"})

	require.False(t, transport.isClosed(), "the connection must survive a partial hydrate failure")
	require.Same(t, client, node.Hub().LookupSession("sess-remote"), "the session must stay registered")
	require.True(t, client.HasSubscription("news"), "the healthy channel must stay restored")
	require.False(t, client.HasSubscription("broken.ch"), "the failed channel must not be restored")
	_, subscribed := node.hub.LookupSubscriber("broken.ch", client)
	require.False(t, subscribed, "the failed channel must not be in the hub")
	require.False(t, directory.deletedLease, "hydrate soft-fail never deletes the lease")
	require.False(t, directory.deletedSnapshot, "hydrate soft-fail never deletes the snapshot")

	// Connected (sent before the failure envelopes) lists only the restored
	// channel.
	var connected *clientpb.Connected
	for _, data := range transport.snapshotMessages() {
		var out clientpb.OutboundMessage
		require.NoError(t, (JSONMarshaler{}).Unmarshal(data, &out))
		if got := out.GetConnected(); got != nil {
			connected = got
			break
		}
	}
	require.NotNil(t, connected, "the resume must still send Connected")
	require.True(t, connected.Resumed)
	require.Len(t, connected.Subscriptions, 1)
	require.Equal(t, "news", connected.Subscriptions[0].Channel)

	failures := recoverFailedEnvelopes(t, transport)
	require.Len(t, failures, 1, "exactly one per-channel RECOVER_FAILED envelope")
	require.Equal(t, "recover_error", failures[0].GetType())
	require.Equal(t, "broken.ch", failures[0].GetMetadata().GetFields()["channel"].GetStringValue())
}

// PR-KA-D10 §1.1 boundary: even when every snapshot channel fails to
// hydrate, the session stays alive with an empty subscription set — no 3502,
// no directory cleanup.
func TestClient_RemoteResume_RestoreAllChannelsFailKeepsSession(t *testing.T) {
	snapshot := &ClusterSessionSnapshot{
		SessionID:     "sess-remote",
		UserID:        "user-1",
		ClientID:      "client-1",
		Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
	}
	node, client, transport, directory := resumeSoftFailFixture(t, snapshot, &failSubscribeBroker{})

	require.False(t, transport.isClosed(), "an all-failed hydrate must not disconnect the client")
	require.Same(t, client, node.Hub().LookupSession("sess-remote"))
	require.False(t, client.HasSubscription("news"))
	require.False(t, directory.deletedLease)
	require.False(t, directory.deletedSnapshot)

	var connected *clientpb.Connected
	for _, data := range transport.snapshotMessages() {
		var out clientpb.OutboundMessage
		require.NoError(t, (JSONMarshaler{}).Unmarshal(data, &out))
		if got := out.GetConnected(); got != nil {
			connected = got
			break
		}
	}
	require.NotNil(t, connected)
	require.True(t, connected.Resumed)
	require.Empty(t, connected.Subscriptions, "no channel restored: Connected lists none")

	failures := recoverFailedEnvelopes(t, transport)
	require.Len(t, failures, 1)
	require.Equal(t, "news", failures[0].GetMetadata().GetFields()["channel"].GetStringValue())
}
// Task 13e: session snapshots must preserve the per-subscription ephemeral
// flag so a cross-node resume does not turn ephemeral subscriptions into
// permanent ones (which would trigger presence join/leave).
func TestNode_ClusterSessionSnapshot_PreservesEphemeral(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-ephemeral", "user-ephemeral", "client-ephemeral")
	require.NoError(t, node.AddClient(client))

	require.NoError(t, node.AddSubscription(context.Background(), "ephemeral.ch", NewSubscriber(client, true)))
	require.NoError(t, node.AddSubscription(context.Background(), "normal.ch", NewSubscriber(client, false)))

	snapshot := node.clusterSessionSnapshot(client)
	require.Len(t, snapshot.Subscriptions, 2)
	byChannel := make(map[string]bool, len(snapshot.Subscriptions))
	for _, sub := range snapshot.Subscriptions {
		byChannel[sub.Channel] = sub.Ephemeral
	}
	require.True(t, byChannel["ephemeral.ch"], "ephemeral subscription must stay ephemeral in the snapshot")
	require.False(t, byChannel["normal.ch"], "permanent subscription must stay non-ephemeral in the snapshot")
}

// TestNode_RestoreSessionSubscriptions_SkipsPresenceForEphemeral verifies
// the ephemeral-presence handoff (docs/protocol.md): restoring an ephemeral
// subscription across nodes must NOT register presence, while permanent
// subscriptions still do.
func TestNode_RestoreSessionSubscriptions_SkipsPresenceForEphemeral(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-restore-eph", "user-restore", "client-restore")

	subscriptions := []ClusterSubscriptionSnapshot{
		{Channel: "eph.ch", Ephemeral: true},
		{Channel: "normal.ch"},
	}
	require.Empty(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

	present, err := node.presence.Get(context.Background(), "eph.ch")
	require.NoError(t, err)
	require.Empty(t, present, "ephemeral subscription must not register presence on restore")

	present, err = node.presence.Get(context.Background(), "normal.ch")
	require.NoError(t, err)
	require.Contains(t, present, "sess-restore-eph",
		"permanent subscription must register presence on restore")
}

// TestNode_RestoreSessionSubscriptions_SkipsPresenceForWildcard verifies
// PR-04a: restoring a wildcard pattern (non-ephemeral) must NOT register
// presence for the pattern itself — wildcard patterns are never store keys.
func TestNode_RestoreSessionSubscriptions_SkipsPresenceForWildcard(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-restore-wc", "user-restore", "client-restore")

	subscriptions := []ClusterSubscriptionSnapshot{
		{Channel: "chat.**", Ephemeral: false},
		{Channel: "normal.ch"},
	}
	require.Empty(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

	present, err := node.presence.Get(context.Background(), "chat.**")
	require.NoError(t, err)
	require.Empty(t, present, "wildcard pattern must not register presence on restore")

	present, err = node.presence.Get(context.Background(), "normal.ch")
	require.NoError(t, err)
	require.Contains(t, present, "sess-restore-wc",
		"tracked exact channels still register presence on restore")
}

// failSecondSubscribeBroker fails the second broker Subscribe so a restore
// aborts after the first channel was already restored.
type failSecondSubscribeBroker struct {
	mu       sync.Mutex
	attempts int
}

func (b *failSecondSubscribeBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *failSecondSubscribeBroker) Subscribe(ch string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	if b.attempts == 2 {
		return errors.New("injected subscribe failure")
	}
	return nil
}
func (b *failSecondSubscribeBroker) Unsubscribe(ch string) error                { return nil }
func (b *failSecondSubscribeBroker) Publish(string, *Publication) (uint64, error) { return 0, nil }
func (b *failSecondSubscribeBroker) PublishTransient(string, *Publication) error  { return nil }
func (b *failSecondSubscribeBroker) PublishOccupancy(string, OccupancyEvent) error {
	return nil
}
func (b *failSecondSubscribeBroker) SetOccupancyHandler(OccupancyHandler) error { return nil }
func (b *failSecondSubscribeBroker) SetGapHandler(GapHandler)                   {}
func (b *failSecondSubscribeBroker) History(string, uint64, int) (*HistoryPage, error) {
	return nil, nil
}

// TestNode_RestoreSessionSubscriptions_PartialFailureKeepsRestoredChannels
// verifies the PR-KA-D10 §1.1 soft-fail semantics: when one channel's restore
// fails midway, the channels that already restored keep their subscription
// and presence entries (no rollback, no projection compensation), and the
// failed channel is reported in the failure list.
func TestNode_RestoreSessionSubscriptions_PartialFailureKeepsRestoredChannels(t *testing.T) {
	node := NewNode(nil)
	node.SetBroker(&failSecondSubscribeBroker{})
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-restore-soft", "user-soft", "client-soft")

	subscriptions := []ClusterSubscriptionSnapshot{
		{Channel: "soft.ch.1"},
		{Channel: "soft.ch.2"},
	}
	failures := node.restoreSessionSubscriptions(context.Background(), client, subscriptions)
	require.Len(t, failures, 1)
	require.Equal(t, "soft.ch.2", failures[0].channel)
	require.Contains(t, failures[0].err.Error(), "injected subscribe failure")

	// The first channel was restored and is NOT rolled back: its presence
	// entry and subscription survive.
	present, getErr := node.presence.Get(context.Background(), "soft.ch.1")
	require.NoError(t, getErr)
	require.Contains(t, present, "sess-restore-soft",
		"a restored channel must keep its presence entry after a later channel fails")
	require.True(t, client.HasSubscription("soft.ch.1"))
	require.False(t, client.HasSubscription("soft.ch.2"))
}

// TestNode_Fence_DoesNotDeleteNewSession verifies P1-C6 under PR-KA-B1
// pointer stability: a stale Fence from an old incarnation of the session
// object (already closed and removed) must not evict a newer session that
// registered the same session ID afterwards — RemoveSessionIfMatches guards
// on the registered pointer.
func TestNode_Fence_DoesNotDeleteNewSession(t *testing.T) {
	node := NewNode(nil)
	clientA, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	clientA.ForceTestIDs("sess-shared", "user-shared", "client-a")
	require.NoError(t, node.AddClient(clientA))
	require.NoError(t, node.AddSubscription(context.Background(), "news", NewSubscriber(clientA, false)))

	// The old incarnation is closed and removed (e.g. a previous close or
	// fence), then a NEW session registers the same session ID.
	require.NoError(t, clientA.Close(Disconnect{}))
	clientB, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	clientB.ForceTestIDs("sess-shared", "user-shared", "client-b")
	require.NoError(t, node.AddClient(clientB))

	// A stale takeover fence arrives for the old incarnation: it must not
	// evict the new session.
	require.NoError(t, clientA.Fence(DisconnectStale))
	require.Same(t, clientB, node.hub.LookupSession("sess-shared"),
		"the new session must survive the stale fence")
}

// TestNode_EvictSessionForTakeover_RemovesOwnSession verifies the matching
// removal still works when the session was not taken over: the registered
// client is the evicted one.
func TestNode_EvictSessionForTakeover_RemovesOwnSession(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-own", "user-own", "client-own")
	require.NoError(t, node.AddClient(client))

	require.NoError(t, client.Fence(DisconnectStale))
	require.Nil(t, node.hub.LookupSession("sess-own"))
}

// --- PR-KA-A1: failed takeover of a live node must roll back the CAS ---

// TestResumeRemoteSession_TakeoverFailureRollsBackCAS verifies §6.5: when
// the CAS claims the lease but the takeover of a still-alive old node fails,
// the lease must be CAS'd back to the original owner and the resume must
// return the takeover error.
func TestResumeRemoteSession_TakeoverFailureRollsBackCAS(t *testing.T) {
	original := &ClusterSessionLease{
		SessionID:     "sess-rollback",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		LeaseVersion:  7,
		ExpiresAt:     time.Now().Add(time.Hour),
	}
	directory := &fakeSessionDirectory{
		lease: original,
		nodeLease: &ClusterNodeLease{
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-rollback",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusFailed, ErrorMessage: "takeover rejected"}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	_, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-rollback")
	require.Error(t, err)
	require.Contains(t, err.Error(), "takeover rejected")
	require.False(t, resumed)

	// The lease is back with the original owner: the claimed fencing was
	// CAS'd back (expected = node-a's claim, desired = the original record).
	lease, err := directory.GetSessionLease(context.Background(), "sess-rollback")
	require.NoError(t, err)
	require.Equal(t, "node-b", lease.NodeID)
	require.Equal(t, "inc-b", lease.IncarnationID)
	require.Equal(t, uint64(7), lease.LeaseVersion)
	require.Equal(t, "node-a", directory.casExpected.NodeID, "rollback CAS must expect the claimed lease")
	require.Equal(t, uint64(8), directory.casExpected.LeaseVersion, "rollback CAS must expect the claimed version")
}

// TestResumeRemoteSession_TakeoverFailureDeadNodeKeepsClaim verifies §6.6
// (KD-K30 bypass): when the takeover fails but the old node's lease is gone,
// the resume keeps the new CAS and continues.
func TestResumeRemoteSession_TakeoverFailureDeadNodeKeepsClaim(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-dead",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		// nodeLease is left nil: the old node is dead.
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-dead",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusFailed, ErrorMessage: "command timed out"}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	snapshot, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-dead")
	require.NoError(t, err)
	require.True(t, resumed)
	require.NotNil(t, snapshot)

	lease, err := directory.GetSessionLease(context.Background(), "sess-dead")
	require.NoError(t, err)
	require.Equal(t, "node-a", lease.NodeID)
	require.Equal(t, uint64(8), lease.LeaseVersion)
}

// TestResumeRemoteSession_NodeLeaseLookupErrorStillRollsBack verifies §5.4:
// when GetNodeLease itself fails, the rollback must still be attempted and
// the lease lookup error returned.
func TestResumeRemoteSession_NodeLeaseLookupErrorStillRollsBack(t *testing.T) {
	original := &ClusterSessionLease{
		SessionID:     "sess-lookup",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		LeaseVersion:  7,
		ExpiresAt:     time.Now().Add(time.Hour),
	}
	directory := &fakeSessionDirectory{
		lease:        original,
		nodeLeaseErr: errors.New("node lease store down"),
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-lookup",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusFailed, ErrorMessage: "takeover rejected"}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	_, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-lookup")
	require.Error(t, err)
	require.Contains(t, err.Error(), "node lease store down")
	require.False(t, resumed)

	lease, err := directory.GetSessionLease(context.Background(), "sess-lookup")
	require.NoError(t, err)
	require.Equal(t, "node-b", lease.NodeID)
	require.Equal(t, uint64(7), lease.LeaseVersion)
}
// --- PR-KA-D3: takeover observability (bind_fenced_total / evict_lag / session_dual_activation_seconds) ---

// resumeMetricsTestNode builds a cluster-enabled node wired with metrics and
// the given directory/command bus for resumeRemoteSession tests.
func resumeMetricsTestNode(t *testing.T, directory SessionDirectory, bus ClusterCommandBus) (*Node, *Metrics) {
	t.Helper()
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)
	node := NewNode(nil)
	node.SetCluster(runtime)
	metrics := NewMetrics(prometheus.NewRegistry())
	node.SetMetrics(metrics)
	return node, metrics
}

func histogramSampleCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, h.Write(&metric))
	return metric.GetHistogram().GetSampleCount()
}

// TestResumeRemoteSession_Metrics_TakeoverClaimFencedCounted verifies D3: a
// lost takeover CAS claim counts towards bind_fenced_total, and no takeover
// timing is observed (no command was sent to a remote old owner).
func TestResumeRemoteSession_Metrics_TakeoverClaimFencedCounted(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID: "sess-remote",
			UserID:    "user-1",
			ClientID:  "client-1",
		},
		forceCasFail: true,
	}
	node, metrics := resumeMetricsTestNode(t, directory, &fakeClusterCommandBus{})
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	_, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-remote")
	require.Error(t, err)
	require.False(t, resumed)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.BindFencedTotal))
	require.Equal(t, uint64(0), histogramSampleCount(t, metrics.EvictLag))
	require.Equal(t, uint64(0), histogramSampleCount(t, metrics.SessionDualActivationSeconds))
}

// TestResumeRemoteSession_Metrics_TakeoverObserved verifies D3: a successful
// takeover of a remotely owned session observes evict_lag and
// session_dual_activation_seconds exactly once each.
func TestResumeRemoteSession_Metrics_TakeoverObserved(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-remote",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	node, metrics := resumeMetricsTestNode(t, directory, bus)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	_, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-remote")
	require.NoError(t, err)
	require.True(t, resumed)
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.BindFencedTotal))
	require.Equal(t, uint64(1), histogramSampleCount(t, metrics.EvictLag))
	require.Equal(t, uint64(1), histogramSampleCount(t, metrics.SessionDualActivationSeconds))
}

// TestResumeRemoteSession_Metrics_NoRemoteOwnerSkipsTiming verifies D3: a
// resume whose lease has no remote old owner (same node incarnation) never
// observes evict_lag or session_dual_activation_seconds.
func TestResumeRemoteSession_Metrics_NoRemoteOwnerSkipsTiming(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-local",
			NodeID:        "node-a",
			IncarnationID: "inc-a",
			LeaseVersion:  3,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID: "sess-local",
			UserID:    "user-1",
			ClientID:  "client-1",
		},
	}
	node, metrics := resumeMetricsTestNode(t, directory, &fakeClusterCommandBus{})
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	_, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-local")
	require.NoError(t, err)
	require.True(t, resumed)
	require.Equal(t, uint64(0), histogramSampleCount(t, metrics.EvictLag))
	require.Equal(t, uint64(0), histogramSampleCount(t, metrics.SessionDualActivationSeconds))
}

// --- PR-KA-D10 §1.3: same-node older-generation leases skip the takeover RPC ---

// TestResumeRemoteSession_SameNodeOlderEpochSkipsTakeover: when the stored
// lease names this nodeID with a strictly older node epoch, the old process
// generation is dead (monotonic INCR, KD-K27) and a takeover RPC against it
// is doomed to the KD-K30 bypass — resume skips the RPC and proceeds with
// the claimed lease.
func TestResumeRemoteSession_SameNodeOlderEpochSkipsTakeover(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-epoch",
			NodeID:        "node-a",
			IncarnationID: "1",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-epoch",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "2", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	snapshot, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-epoch")
	require.NoError(t, err)
	require.True(t, resumed)
	require.NotNil(t, snapshot)
	require.Empty(t, bus.commands, "no takeover RPC against a dead older generation of the same node")

	lease, err := directory.GetSessionLease(context.Background(), "sess-epoch")
	require.NoError(t, err)
	require.Equal(t, "node-a", lease.NodeID)
	require.Equal(t, "2", lease.IncarnationID)
	require.Equal(t, uint64(8), lease.LeaseVersion, "the claim still bumps the lease version")
}

// TestResumeRemoteSession_NonEpochIncarnationDoesNotSkip: same nodeID with a
// non-epoch incarnation ID (ParseNodeEpoch fails) keeps the old behavior —
// the takeover RPC is issued.
func TestResumeRemoteSession_NonEpochIncarnationDoesNotSkip(t *testing.T) {
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-nonepoch",
			NodeID:        "node-a",
			IncarnationID: "inc-a-old",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-nonepoch",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	snapshot, resumed, err := node.resumeRemoteSession(context.Background(), client, "sess-nonepoch")
	require.NoError(t, err)
	require.True(t, resumed)
	require.NotNil(t, snapshot)
	require.Len(t, bus.commands, 1, "a non-epoch incarnation never skips the takeover RPC")
	require.Equal(t, ClusterCommandTakeover, bus.commands[0].Type)
}
