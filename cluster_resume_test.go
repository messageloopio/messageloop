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
func (b *evictTestBroker) PublishTransient(string, *Publication) error { return nil }
func (b *evictTestBroker) History(string, uint64, int) ([]*Publication, error) { return nil, nil }

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

	err = node.evictSessionForTakeover(client)
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

	require.NoError(t, node.evictSessionForTakeover(clientA))

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
func (b *failSubscribeBroker) History(string, uint64, int) ([]*Publication, error) {
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