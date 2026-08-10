package messageloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/proxy"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
)

func (c *capturingTransport) getMessage(i int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i < 0 || i >= len(c.messages) {
		return nil
	}
	return c.messages[i]
}

// connectAuthProxyStub is a minimal Proxy that authenticates successfully with
// a fixed user ID, or returns the configured error.
type connectAuthProxyStub struct {
	userID string
	err    error
}

func (m *connectAuthProxyStub) RPC(context.Context, *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	return nil, nil
}

func (m *connectAuthProxyStub) Authenticate(context.Context, *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &proxy.AuthenticateProxyResponse{UserInfo: &proxy.UserInfo{ID: m.userID}}, nil
}

func (m *connectAuthProxyStub) SubscribeAcl(context.Context, *proxy.SubscribeAclProxyRequest) (*proxy.SubscribeAclProxyResponse, error) {
	return &proxy.SubscribeAclProxyResponse{}, nil
}

func (m *connectAuthProxyStub) PublishAcl(context.Context, *proxy.PublishAclProxyRequest) (*proxy.PublishAclProxyResponse, error) {
	return &proxy.PublishAclProxyResponse{}, nil
}

func (m *connectAuthProxyStub) OnConnected(context.Context, *proxy.OnConnectedProxyRequest) (*proxy.OnConnectedProxyResponse, error) {
	return &proxy.OnConnectedProxyResponse{}, nil
}

func (m *connectAuthProxyStub) OnSubscribed(context.Context, *proxy.OnSubscribedProxyRequest) (*proxy.OnSubscribedProxyResponse, error) {
	return &proxy.OnSubscribedProxyResponse{}, nil
}

func (m *connectAuthProxyStub) OnUnsubscribed(context.Context, *proxy.OnUnsubscribedProxyRequest) (*proxy.OnUnsubscribedProxyResponse, error) {
	return &proxy.OnUnsubscribedProxyResponse{}, nil
}

func (m *connectAuthProxyStub) OnDisconnected(context.Context, *proxy.OnDisconnectedProxyRequest) (*proxy.OnDisconnectedProxyResponse, error) {
	return &proxy.OnDisconnectedProxyResponse{}, nil
}

func (m *connectAuthProxyStub) Name() string { return "connect-auth-stub" }
func (m *connectAuthProxyStub) Close() error { return nil }

// recordingSessionDirectory records whether lease/snapshot deletion was called.
type recordingSessionDirectory struct {
	*fakeSessionDirectory
	deletedLease    bool
	deletedSnapshot bool
}

func (r *recordingSessionDirectory) DeleteSessionLease(context.Context, string) error {
	r.deletedLease = true
	return nil
}

func (r *recordingSessionDirectory) DeleteSessionSnapshot(context.Context, string) error {
	r.deletedSnapshot = true
	return nil
}

// --- P0-3: JSON payload publish must stay valid JSON ---

func TestClientSession_HandleMessage_Publish_JSONPayload(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx) // Register event handler
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	transport.messages = nil

	payloadStruct, err := structpb.NewStruct(map[string]interface{}{
		"hello": "world",
		"nested": map[string]interface{}{
			"count": float64(42),
			"tags":  []interface{}{"a", "b"},
		},
	})
	require.NoError(t, err)

	pubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "json-ch",
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Json{Json: payloadStruct}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, pubMsg))

	// The published bytes must be parseable JSON with the exact content (not
	// the structpb protobuf text format).
	pubs, err := node.Broker().History("json-ch", 0, 0)
	require.NoError(t, err)
	require.Len(t, pubs, 1)
	require.True(t, pubs[0].IsText)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(pubs[0].Payload, &decoded))
	assert.Equal(t, "world", decoded["hello"])
	nested, ok := decoded["nested"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, float64(42), nested["count"])
	assert.Equal(t, []interface{}{"a", "b"}, nested["tags"])
}

// --- P0-5: connect subscriptions must go through subscribe ACL ---

func TestClientSession_HandleMessage_Connect_ACLDeniedSubscription(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{
		ACL: config.ACLConfig{
			Rules: []config.ACLRule{
				{ChannelPattern: "private.*", DenyAll: true},
			},
		},
	})
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "private.secret"},
					{Channel: "public.room"},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	// The denied channel must not be subscribed; the allowed one must.
	assert.Zero(t, node.Hub().NumSubscribers("private.secret"))
	assert.Equal(t, 1, node.Hub().NumSubscribers("public.room"))

	// The connection stays up: a per-channel ACL_DENIED error followed by the
	// Connected envelope.
	require.False(t, transport.isClosed())
	require.Equal(t, 2, transport.getMessageCount())

	var first clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &first))
	errEnv := first.GetError()
	require.NotNil(t, errEnv, "first message should be the ACL error")
	assert.Equal(t, "ACL_DENIED", errEnv.Code)

	var last clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &last))
	connected := last.GetConnected()
	require.NotNil(t, connected, "last message should be Connected")
	assert.Len(t, connected.GetSubscriptions(), 1)
	assert.Equal(t, "public.room", connected.GetSubscriptions()[0].Channel)
}

// --- P0-5: per-client subscription limit on connect ---

func TestClientSession_HandleMessage_Connect_SubscriptionLimit(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{Limits: config.Limits{MaxSubscriptionsPerClient: 2}})
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "ch1"},
					{Channel: "ch2"},
					{Channel: "ch3"},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	require.True(t, transport.isClosed())
	assert.Equal(t, DisconnectChannelLimit.Code, transport.getCloseReason().Code)
	assert.Zero(t, node.Hub().NumSubscribers("ch1"))
}

func TestClientSession_HandleMessage_Connect_SubscriptionLimitWithResume(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true, Limits: config.Limits{MaxSubscriptionsPerClient: 3}})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// Connect without subscriptions.
	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1", Token: "t"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	sessionID := client.SessionID()
	transport.messages = nil

	// Subscribe to two channels.
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{
					{Channel: "ch1"},
					{Channel: "ch2"},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))
	transport.messages = nil

	// Resume the session with two more channels: 2 inherited + 2 new > 3.
	newTransport := &capturingTransport{}
	newClient, _, err := NewClient(ctx, node, newTransport, JSONMarshaler{})
	require.NoError(t, err)

	resumeMsg := &clientpb.InboundMessage{
		Id: "msg-3",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-1",
				Token:     "t",
				SessionId: sessionID,
				Subscriptions: []*clientpb.Subscription{
					{Channel: "ch3"},
					{Channel: "ch4"},
				},
			},
		},
	}
	require.NoError(t, newClient.HandleMessage(ctx, resumeMsg))

	require.True(t, newTransport.isClosed())
	assert.Equal(t, DisconnectChannelLimit.Code, newTransport.getCloseReason().Code)
}

// --- P0-5: connect writes client fields under lock ---

func TestClientSession_HandleMessage_Connect_ConcurrentWithClose(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", "$authenticate"))
	transport := &capturingTransport{}

	client, closeFunc, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-1",
				Token:     "token-1",
				SessionId: "sess-1",
			},
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = closeFunc()
	}()
	go func() {
		defer wg.Done()
		_ = client.HandleMessage(ctx, connectMsg)
	}()
	wg.Wait()

	// Whatever the interleaving, the transport must end up closed.
	require.True(t, transport.isClosed())
}

// --- P1-6: requireAuth + non-empty token without an auth proxy must reject ---

func TestClientSession_HandleMessage_Connect_RequireAuthNoProxyTokenRejected(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true})
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Token:    "any-token",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	// The connection must be rejected with DisconnectInvalidToken.
	require.True(t, transport.isClosed())
	assert.Equal(t, DisconnectInvalidToken.Code, transport.getCloseReason().Code)

	// An error frame must be sent before the disconnect.
	require.GreaterOrEqual(t, transport.getMessageCount(), 1)
	var first clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &first))
	errEnv := first.GetError()
	require.NotNil(t, errEnv, "first message should be the auth error")
	assert.Equal(t, "AUTH_REQUIRED", errEnv.Code)
}

// Regression guard: without requireAuth, a token with no auth proxy still
// connects (the token is simply not verified).
func TestClientSession_HandleMessage_Connect_TokenNoProxyNoRequireAuth_Allowed(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Token:    "any-token",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	require.False(t, transport.isClosed())
	assert.True(t, client.Authenticated())
}

// --- P1-2: takeover must not run before authentication ---

func TestClientSession_HandleMessage_Connect_InvalidTokenNoTakeover(t *testing.T) {
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
	bus := &fakeClusterCommandBus{}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	authProxy := &connectAuthProxyStub{err: errors.New("invalid token")}
	require.NoError(t, node.AddProxy(authProxy, "", "$authenticate"))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-x",
				Token:     "bad-token",
				SessionId: "sess-remote",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	// The connection must fail auth and be closed.
	require.True(t, transport.isClosed())
	assert.Equal(t, DisconnectInvalidToken.Code, transport.getCloseReason().Code)

	// No takeover command may be issued for an unauthenticated connect.
	assert.Empty(t, bus.commands)

	// The remote lease and snapshot must not be deleted.
	assert.False(t, directory.deletedLease)
	assert.False(t, directory.deletedSnapshot)
}

// P1-2: a failed local resume must not evict the old session.
func TestClientSession_HandleMessage_Connect_ResumeAuthFailureKeepsOldSession(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	authProxy := &connectAuthProxyStub{err: errors.New("bad token")}
	require.NoError(t, node.AddProxy(authProxy, "", "$authenticate"))

	oldTransport := &capturingTransport{}
	oldClient, _, err := NewClient(ctx, node, oldTransport, JSONMarshaler{})
	require.NoError(t, err)

	// Establish a session (no token: auth proxy is not consulted).
	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, oldClient.HandleMessage(ctx, connectMsg))
	sessionID := oldClient.SessionID()
	require.NotEmpty(t, sessionID)

	// Subscribe to a channel so we can verify the old session survives.
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "keep-alive"}},
			},
		},
	}
	require.NoError(t, oldClient.HandleMessage(ctx, subMsg))
	require.Equal(t, 1, node.Hub().NumSubscribers("keep-alive"))

	// Attempt a resume with an invalid token: auth runs before the takeover,
	// so the old session must not be evicted.
	newTransport := &capturingTransport{}
	newClient, _, err := NewClient(ctx, node, newTransport, JSONMarshaler{})
	require.NoError(t, err)
	resumeMsg := &clientpb.InboundMessage{
		Id: "msg-3",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-1",
				Token:     "bad-token",
				SessionId: sessionID,
			},
		},
	}
	require.NoError(t, newClient.HandleMessage(ctx, resumeMsg))

	// New connection rejected...
	require.True(t, newTransport.isClosed())
	assert.Equal(t, DisconnectInvalidToken.Code, newTransport.getCloseReason().Code)

	// ...and the old session is untouched: still registered, still subscribed,
	// and its transport is still open.
	assert.Same(t, oldClient, node.Hub().LookupSession(sessionID))
	assert.Equal(t, 1, node.Hub().NumSubscribers("keep-alive"))
	assert.False(t, oldTransport.isClosed())
}

// P1-2: a successful remote resume still performs the takeover.
func TestClientSession_HandleMessage_Connect_ResumeRemoteSendsTakeover(t *testing.T) {
	ctx := context.Background()
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

	node := NewNode(&config.Server{RequireAuth: true})
	node.SetCluster(runtime)

	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", "$authenticate"))

	client, _, err := NewClient(ctx, node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-1",
				Token:     "ok-token",
				SessionId: "sess-remote",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	require.Len(t, bus.commands, 1)
	assert.Equal(t, ClusterCommandTakeover, bus.commands[0].Type)
	assert.Equal(t, uint64(7), bus.commands[0].LeaseVersion)
	assert.Equal(t, "user-1", client.UserID())
	assert.Equal(t, "client-1", client.ClientID())
	assert.True(t, client.hasSubscription("news"))
}

// --- P1-2: deleteClusterSessionState ownership check ---

func TestNode_DeleteClusterSessionState_RemoteLeasePreserved(t *testing.T) {
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{SessionID: "sess-remote"},
	}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	err = node.deleteClusterSessionState(context.Background(), "sess-remote")
	require.NoError(t, err)
	assert.False(t, directory.deletedLease, "foreign lease must not be deleted")
	assert.False(t, directory.deletedSnapshot, "foreign snapshot must not be deleted")
}

func TestNode_DeleteClusterSessionState_OwnLeaseDeleted(t *testing.T) {
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-local",
			NodeID:        "node-a",
			IncarnationID: "inc-a",
			ExpiresAt:     time.Now().Add(time.Hour),
		},
		snapshot: &ClusterSessionSnapshot{SessionID: "sess-local"},
	}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	err = node.deleteClusterSessionState(context.Background(), "sess-local")
	require.NoError(t, err)
	assert.True(t, directory.deletedLease, "own lease must be deleted")
	assert.True(t, directory.deletedSnapshot, "own snapshot must be deleted")
}

func TestNode_DeleteClusterSessionState_ExpiredLeaseDeleted(t *testing.T) {
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-stale",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			ExpiresAt:     time.Now().Add(-time.Minute),
		},
		snapshot: &ClusterSessionSnapshot{SessionID: "sess-stale"},
	}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	err = node.deleteClusterSessionState(context.Background(), "sess-stale")
	require.NoError(t, err)
	assert.True(t, directory.deletedLease, "expired lease must be deleted")
	assert.True(t, directory.deletedSnapshot, "expired snapshot must be deleted")
}

func TestNode_DeleteClusterSessionState_NoLeaseDeleted(t *testing.T) {
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)

	err = node.deleteClusterSessionState(context.Background(), "sess-missing")
	require.NoError(t, err)
	assert.True(t, directory.deletedLease, "missing lease must be deleted (cleanup)")
	assert.True(t, directory.deletedSnapshot, "missing snapshot must be deleted (cleanup)")
}

// --- P2-14: ConnectionsTotal must not drift when close() runs without AddClient ---

func TestClient_Close_NoGaugeDriftOnAuthFailure(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetMetrics(NewMetrics(prometheus.NewRegistry()))
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// Publish before auth: the connection is closed without ever passing
	// AddClient, so the gauge must not be decremented below zero.
	pubMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "test-channel",
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: []byte("test payload")}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, pubMsg))
	require.True(t, transport.isClosed())

	assert.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ConnectionsTotal),
		"gauge must stay at zero for a connection that never passed AddClient")
}

func TestClient_Close_GaugeBalancedForChargedClient(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetMetrics(NewMetrics(prometheus.NewRegistry()))
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	assert.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ConnectionsTotal))

	require.NoError(t, client.Close(Disconnect{}))
	assert.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ConnectionsTotal))
}

// --- P2-17: close() must remove all subscriptions across many channels ---

func TestClient_Close_RemovesAllSubscriptions(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	const numChannels = 64
	subs := make([]*clientpb.Subscription, 0, numChannels)
	for i := 0; i < numChannels; i++ {
		subs = append(subs, &clientpb.Subscription{Channel: fmt.Sprintf("bulk-ch-%d", i)})
	}
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: subs},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))
	for i := 0; i < numChannels; i++ {
		assert.Equal(t, 1, node.Hub().NumSubscribers(fmt.Sprintf("bulk-ch-%d", i)))
	}

	require.NoError(t, client.Close(Disconnect{}))

	for i := 0; i < numChannels; i++ {
		assert.Zero(t, node.Hub().NumSubscribers(fmt.Sprintf("bulk-ch-%d", i)),
			"channel bulk-ch-%d must be cleaned up on close", i)
	}
	assert.Empty(t, client.subscriptionList())
}

// --- P2-21: pings must throttle the presence/cluster refresh work ---

// countingSessionDirectory counts every lease/snapshot write so tests can
// observe how often syncClusterSessionState runs.
type countingSessionDirectory struct {
	*fakeSessionDirectory
	mu   sync.Mutex
	puts int
}

func (c *countingSessionDirectory) PutSessionLease(context.Context, *ClusterSessionLease, time.Duration) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return nil
}

func (c *countingSessionDirectory) PutSessionSnapshot(context.Context, *ClusterSessionSnapshot, time.Duration) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return nil
}

func (c *countingSessionDirectory) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.puts
}

func TestClient_HandlePing_ThrottlesClusterRefresh(t *testing.T) {
	ctx := context.Background()
	directory := &countingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(runtime)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	// Baseline after connect: each cluster sync writes a lease and a snapshot.
	baseline := directory.count()

	// A burst of pings within the throttle interval must only trigger one
	// refresh (lease + snapshot), not one goroutine pair per ping.
	const burst = 5
	for i := 0; i < burst; i++ {
		pingMsg := &clientpb.InboundMessage{
			Id:       fmt.Sprintf("ping-%d", i),
			Envelope: &clientpb.InboundMessage_Ping{Ping: &clientpb.Ping{}},
		}
		require.NoError(t, client.HandleMessage(ctx, pingMsg))
	}
	require.Eventually(t, func() bool { return directory.count() >= baseline+2 }, time.Second, 10*time.Millisecond)
	// Give any stragglers a chance to over-sync, then assert nothing more ran.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, baseline+2, directory.count(),
		"a burst of pings within the interval must trigger exactly one refresh")

	// Simulate the interval elapsing: the next ping refreshes again.
	client.lastClusterSyncNano.Store(time.Now().Add(-pingClusterRefreshInterval).UnixNano())
	latePing := &clientpb.InboundMessage{
		Id:       "ping-late",
		Envelope: &clientpb.InboundMessage_Ping{Ping: &clientpb.Ping{}},
	}
	require.NoError(t, client.HandleMessage(ctx, latePing))
	require.Eventually(t, func() bool { return directory.count() >= baseline+4 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, baseline+4, directory.count())
}

// --- P2-22: clients without an epoch must recover from the beginning ---

// fakeEpochHistoryBroker is a fakeHistoryBroker that also reports a broker
// epoch, simulating a broker that has restarted.
type fakeEpochHistoryBroker struct {
	pubs  []*Publication
	epoch string
}

func (f *fakeEpochHistoryBroker) Epoch() string { return f.epoch }

func (f *fakeEpochHistoryBroker) Start(ctx context.Context, handler PublicationHandler) error {
	<-ctx.Done()
	return nil
}

func (f *fakeEpochHistoryBroker) Subscribe(string) error   { return nil }
func (f *fakeEpochHistoryBroker) Unsubscribe(string) error { return nil }

func (f *fakeEpochHistoryBroker) Publish(ch string, payload []byte, isText bool) (uint64, error) {
	return 0, nil
}

func (f *fakeEpochHistoryBroker) PublishTransient(ch string, payload []byte, isText bool) (uint64, error) {
	return 0, nil
}

func (f *fakeEpochHistoryBroker) History(ch string, sinceOffset uint64, limit int) ([]*Publication, error) {
	result := make([]*Publication, 0, len(f.pubs))
	for _, p := range f.pubs {
		if p.Offset >= sinceOffset {
			result = append(result, p)
		}
	}
	return result, nil
}

func TestClient_Connect_RecoveryFromZeroWhenClientEpochMissing(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	// Broker restarted: it has an epoch, and its history holds offsets 1..3.
	node.SetBroker(&fakeEpochHistoryBroker{
		epoch: "v2",
		pubs: []*Publication{
			{Channel: "epoch-ch", Offset: 1, Payload: []byte("m1")},
			{Channel: "epoch-ch", Offset: 2, Payload: []byte("m2")},
			{Channel: "epoch-ch", Offset: 3, Payload: []byte("m3")},
		},
	})
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// The client carries a stale offset (2) but no epoch: an older SDK cannot
	// prove the offset belongs to the current broker generation, so recovery
	// must fall back to the beginning instead of silently skipping m1.
	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "epoch-ch", Recover: true, Offset: 2, Epoch: ""},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	connected := out.GetConnected()
	require.NotNil(t, connected)
	require.Len(t, connected.GetPublications(), 3)
	var offsets []uint64
	for _, pub := range connected.GetPublications() {
		for _, m := range pub.GetMessages() {
			offsets = append(offsets, m.GetOffset())
		}
	}
	assert.Equal(t, []uint64{1, 2, 3}, offsets,
		"a client without an epoch must recover from the beginning")
}

func TestClient_Connect_RecoveryFromOffsetWhenEpochMatches(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	node.SetBroker(&fakeEpochHistoryBroker{
		epoch: "v2",
		pubs: []*Publication{
			{Channel: "epoch-ch", Offset: 1, Payload: []byte("m1")},
			{Channel: "epoch-ch", Offset: 2, Payload: []byte("m2")},
			{Channel: "epoch-ch", Offset: 3, Payload: []byte("m3")},
		},
	})
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// A client whose epoch matches the broker keeps incremental recovery:
	// offset 2 was already seen, so only offset 3 is replayed.
	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "epoch-ch", Recover: true, Offset: 2, Epoch: "v2"},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getLastMessage(), &out))
	connected := out.GetConnected()
	require.NotNil(t, connected)
	require.Len(t, connected.GetPublications(), 1)
	msgs := connected.GetPublications()[0].GetMessages()
	require.Len(t, msgs, 1)
	assert.Equal(t, uint64(3), msgs[0].GetOffset())
}

// Task 9: anonymous mode must not be able to take over a session by guessing
// its SessionId: the session id is ignored and a fresh session is created.
func TestClientSession_AnonymousResumeRejected(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil) // requireAuth=false

	transportA := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transportA, JSONMarshaler{})
	require.NoError(t, err)
	connectA := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-a"},
		},
	}
	require.NoError(t, clientA.HandleMessage(ctx, connectA))
	sessionA := clientA.SessionID()
	require.NotEmpty(t, sessionA)
	transportA.messages = nil

	// Client B presents the captured session id in anonymous mode.
	transportB := &capturingTransport{}
	clientB, _, err := NewClient(ctx, node, transportB, JSONMarshaler{})
	require.NoError(t, err)
	connectB := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-b", SessionId: sessionA},
		},
	}
	require.NoError(t, clientB.HandleMessage(ctx, connectB))

	// B must not take over the session: it gets a fresh session id...
	require.NotEqual(t, sessionA, clientB.SessionID(), "anonymous resume must be rejected")
	// ...and A stays registered in the hub.
	assert.Same(t, clientA, node.Hub().LookupSession(sessionA), "session A must not be evicted")
}

// Task 9: a local resume must not leak the ConnectionsTotal gauge: the old
// client was counted once, the new client takes over that count (still one),
// and closing the resumed client returns the gauge to zero.
func TestClientSession_LocalResume_MetricsBalanced(t *testing.T) {
	ctx := context.Background()
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)
	node := NewNode(&config.Server{RequireAuth: true})
	node.SetMetrics(metrics)
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transportA := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transportA, JSONMarshaler{})
	require.NoError(t, err)
	connectA := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-a", Token: "t"},
		},
	}
	require.NoError(t, clientA.HandleMessage(ctx, connectA))
	sessionA := clientA.SessionID()
	require.NoError(t, err)
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal))

	// Resume the session locally (same node, same user, valid token).
	transportB := &capturingTransport{}
	clientB, _, err := NewClient(ctx, node, transportB, JSONMarshaler{})
	require.NoError(t, err)
	connectB := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-a", Token: "t", SessionId: sessionA},
		},
	}
	require.NoError(t, clientB.HandleMessage(ctx, connectB))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal), "resume must not double count")

	// Closing the resumed client balances the gauge back to zero.
	require.NoError(t, clientB.Close(Disconnect{}))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ConnectionsTotal))
}