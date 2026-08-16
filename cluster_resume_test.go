package messageloop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
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
		assert.True(t, client.hasSubscription(ch), "client should track channel %s", ch)
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

	require.NoError(t, client.handleUnsubscribe(context.Background(), &clientpb.InboundMessage{
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
	require.NoError(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

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
	assert.True(t, client.hasSubscription("news"))
	assert.True(t, client.hasSubscription("sports"))
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
	require.False(t, client.hasSubscription("news"), "no subscriptions may be restored after a CAS conflict")
}
// failSubscribeBroker fails every Subscribe so remote subscription restore
// aborts midway.
type failSubscribeBroker struct{}

func (b *failSubscribeBroker) Start(context.Context, PublicationHandler) error { return nil }
func (b *failSubscribeBroker) Subscribe(ch string) error                        { return errors.New("injected subscribe failure") }
func (b *failSubscribeBroker) Unsubscribe(ch string) error                      { return nil }
func (b *failSubscribeBroker) Publish(string, *Publication) (uint64, error)     { return 0, nil }
func (b *failSubscribeBroker) PublishTransient(string, *Publication) error      { return nil }
func (b *failSubscribeBroker) History(string, uint64, int) (*HistoryPage, error) {
	return nil, nil
}

// Task 13b: when restoring a remote session's subscriptions fails, the
// partially restored session must be rolled back: no zombie session in the
// hub and no leftover lease/snapshot.
func TestClient_RemoteResume_RestoreFailureRollsBackSession(t *testing.T) {
	ctx := context.Background()
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			LeaseVersion:  3,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{
			SessionID:     "sess-remote",
			UserID:        "user-1",
			ClientID:      "client-1",
			Subscriptions: []ClusterSubscriptionSnapshot{{Channel: "news"}},
		},
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
	node.SetBroker(&failSubscribeBroker{})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	resumeMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-1",
				Token:     "ok-token",
				SessionId: "sess-remote",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, resumeMsg))

	// The new connection is closed...
	require.True(t, transport.isClosed(), "the new connection must be disconnected")

	// ...and no zombie session or cluster state remains.
	require.Nil(t, node.Hub().LookupSession("sess-remote"), "no zombie session in the hub")
	require.True(t, directory.deletedLease, "lease must be cleaned up")
	require.True(t, directory.deletedSnapshot, "snapshot must be cleaned up")
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
	require.NoError(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

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
	require.NoError(t, node.restoreSessionSubscriptions(context.Background(), client, subscriptions))

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
func (b *failSecondSubscribeBroker) History(string, uint64, int) (*HistoryPage, error) {
	return nil, nil
}

// TestNode_RestoreSessionSubscriptions_RollbackClearsPresence verifies that
// a partial restore rollback removes the presence entries the restore path
// added (no ghost online member after a failed restore).
func TestNode_RestoreSessionSubscriptions_RollbackClearsPresence(t *testing.T) {
	node := NewNode(nil)
	node.SetBroker(&failSecondSubscribeBroker{})
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-restore-rollback", "user-rollback", "client-rollback")

	subscriptions := []ClusterSubscriptionSnapshot{
		{Channel: "rollback.ch.1"},
		{Channel: "rollback.ch.2"},
	}
	err = node.restoreSessionSubscriptions(context.Background(), client, subscriptions)
	require.Error(t, err)
	require.Contains(t, err.Error(), "injected subscribe failure")

	// The first channel was restored (presence added) then rolled back: no
	// ghost presence may remain.
	present, getErr := node.presence.Get(context.Background(), "rollback.ch.1")
	require.NoError(t, getErr)
	require.Empty(t, present, "rollback must clear the presence entry of restored channels")
	require.False(t, client.hasSubscription("rollback.ch.1"))
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