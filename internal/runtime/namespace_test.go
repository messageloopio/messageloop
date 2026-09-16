package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
)

// connectEnvelope builds a v2 Connect inbound message.
func connectEnvelope(id string, c *clientpb.Connect) *clientpb.InboundMessage {
	return &clientpb.InboundMessage{Id: id, Envelope: &clientpb.InboundMessage_Connect{Connect: c}}
}

// TestConnect_NamespaceFromAuthProxy pins that the auth proxy's namespace
// wins over the static server.namespace and lands on the session.
func TestConnect_NamespaceFromAuthProxy(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true, Namespace: "static"})
	authProxy := &connectAuthProxyStub{userID: "user-1", namespace: "acme"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	client, _, err := NewClient(ctx, node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, connectEnvelope("m1", &clientpb.Connect{
		Version: testProtocolVersion, ClientId: "client-1", Token: "t",
	})))

	assert.Equal(t, "acme", client.Namespace(), "the proxy namespace must win over the static one")
	assert.Equal(t, "acme", client.ClientInfo().Namespace)
}

// TestConnect_NamespaceStaticFallback pins the static server.namespace
// fallback when the auth proxy returns none.
func TestConnect_NamespaceStaticFallback(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true, Namespace: "static"})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	client, _, err := NewClient(ctx, node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, connectEnvelope("m1", &clientpb.Connect{
		Version: testProtocolVersion, ClientId: "client-1", Token: "t",
	})))

	assert.Equal(t, "static", client.Namespace())
}

// TestConnect_NamespaceRequiredRejects pins the fail-closed rule: a
// require_auth server with neither a proxy namespace nor a static fallback
// rejects the connect (NAMESPACE_REQUIRED envelope + 3500).
func TestConnect_NamespaceRequiredRejects(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, connectEnvelope("m1", &clientpb.Connect{
		Version: testProtocolVersion, ClientId: "client-1", Token: "t",
	})))

	require.True(t, transport.isClosed())
	assert.Equal(t, DisconnectInvalidToken.Code, transport.getCloseReason().Code)
	require.GreaterOrEqual(t, transport.getMessageCount(), 1)
	var first clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &first))
	errEnv := first.GetError()
	require.NotNil(t, errEnv)
	assert.Equal(t, "NAMESPACE_REQUIRED", errEnv.Code)
}

// TestConnect_SubscribeNamespaceMismatchSkipped pins the guard on the
// Connect-carried subscriptions: out-of-namespace channels are skipped with
// an error envelope and the connection stays up.
func TestConnect_SubscribeNamespaceMismatchSkipped(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true, Namespace: "acme"})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, connectEnvelope("m1", &clientpb.Connect{
		Version:   testProtocolVersion,
		ClientId:  "client-1",
		Token:     "t",
		Subscriptions: []*clientpb.Subscription{
			{Channel: "other:chat"},
			{Channel: "acme:chat"},
		},
	})))

	require.False(t, transport.isClosed(), "a namespace mismatch must not disconnect")
	assert.False(t, client.HasSubscription("other:chat"), "the foreign channel must be skipped")
	assert.True(t, client.HasSubscription("acme:chat"), "the in-namespace channel must be subscribed")
}

// TestConnect_LocalResumeNamespaceMismatchDenied pins the resume owner rule
// for namespaces: a connection that resolved a different namespace may not
// take over a session (3500, old session untouched).
func TestConnect_LocalResumeNamespaceMismatchDenied(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true, Namespace: "acme"})
	authProxy := &connectAuthProxyStub{userID: "user-1", namespace: "acme"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transportA := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transportA, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, clientA.HandleMessage(ctx, connectEnvelope("m1", &clientpb.Connect{
		Version: testProtocolVersion, ClientId: "client-a", Token: "t",
	})))
	sessionA := clientA.SessionID()

	// The proxy now hands out a different namespace for the same user.
	authProxy.namespace = "other"

	transportB := &capturingTransport{}
	clientB, _, err := NewClient(ctx, node, transportB, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, clientB.HandleMessage(ctx, connectEnvelope("m2", &clientpb.Connect{
		Version: testProtocolVersion, ClientId: "client-a", Token: "t", SessionId: sessionA,
	})))

	require.True(t, transportB.isClosed(), "the cross-namespace resume must be refused")
	assert.Equal(t, DisconnectInvalidToken.Code, transportB.getCloseReason().Code)
	assert.Same(t, clientA, node.Hub().LookupSession(sessionA), "the old session must stay in the hub")
	assert.False(t, transportA.isClosed(), "the old session must stay attached")
}

// TestConnect_RemoteResumeNamespaceMismatchDenied pins the same rule for the
// cross-node resume: the lease's namespace must match before the CAS.
func TestConnect_RemoteResumeNamespaceMismatchDenied(t *testing.T) {
	ctx := context.Background()
	directory := &fakeSessionDirectory{
		lease: &ClusterSessionLease{
			SessionID:     "sess-remote",
			NodeID:        "node-b",
			IncarnationID: "inc-b",
			Namespace:     "acme",
			LeaseVersion:  7,
			ExpiresAt:     time.Now().Add(time.Hour),
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(&config.Server{RequireAuth: true, Namespace: "other"})
	node.SetCluster(runtime)
	authProxy := &connectAuthProxyStub{userID: "user-1", namespace: "other"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	// The remote-resume denial closes the connection through
	// disconnectOnConnectError, which swallows the error.
	require.NoError(t, client.HandleMessage(ctx, connectEnvelope("m1", &clientpb.Connect{
		Version: testProtocolVersion, ClientId: "client-1", Token: "t", SessionId: "sess-remote",
	})))

	require.True(t, transport.isClosed(), "the cross-namespace resume must be refused")
	assert.Equal(t, DisconnectInvalidToken.Code, transport.getCloseReason().Code)
	assert.Empty(t, bus.commands, "the takeover command must never be issued for a foreign namespace")
	// The lease must be untouched: the refusal happens before the CAS.
	assert.Equal(t, uint64(7), directory.lease.LeaseVersion)
}

// TestSession_SnapshotCarriesNamespace verifies the cluster snapshot writer
// persists the session namespace (and mirrors it into AuthContext).
func TestSession_SnapshotCarriesNamespace(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-1", "user-1", "client-1")
	client.SetNamespaceForTest("acme")

	snapshot := node.clusterSessionSnapshot(client)
	assert.Equal(t, "acme", snapshot.Namespace)
	assert.Equal(t, "acme", snapshot.AuthContext["namespace"])
	assert.Equal(t, "acme", node.clusterSessionLease(client).Namespace)
}

// TestNode_AdminSessionNamespace_Local pins the local-hub path of
// AdminSessionNamespace (admin API key design §2.4): a session with a
// resolved namespace reports it, while a namespace-less session and an
// unknown session ID report not-found — a namespace-less session must stay
// invisible to scoped identities (fail-closed).
func TestNode_AdminSessionNamespace_Local(t *testing.T) {
	node := NewNode(nil)
	client, _, err := NewClient(context.Background(), node, noopTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-ns", "user-ns", "client-ns")
	require.NoError(t, node.AddClient(client))

	// Namespace not yet resolved: invisible ("", false).
	ns, ok := node.AdminSessionNamespace(context.Background(), "sess-ns")
	assert.False(t, ok)
	assert.Empty(t, ns)

	client.SetNamespaceForTest("acme")
	ns, ok = node.AdminSessionNamespace(context.Background(), "sess-ns")
	assert.True(t, ok)
	assert.Equal(t, "acme", ns)

	// Unknown session: not found.
	ns, ok = node.AdminSessionNamespace(context.Background(), "sess-missing")
	assert.False(t, ok)
	assert.Empty(t, ns)
}

// TestNode_AdminSessionNamespace_Cluster pins the cluster-directory fallback
// of AdminSessionNamespace (design §2.4): a session unknown locally resolves
// through its lease namespace; a namespace-less lease and a missing lease
// report not-found (fail-closed).
func TestNode_AdminSessionNamespace_Cluster(t *testing.T) {
	directory := &fakeSessionDirectory{
		leases: map[string]*ClusterSessionLease{
			"sess-remote": {SessionID: "sess-remote", NodeID: "node-b", IncarnationID: "inc-b", Namespace: "acme"},
			"sess-nsless": {SessionID: "sess-nsless", NodeID: "node-b", IncarnationID: "inc-b"},
		},
	}
	bus := &fakeClusterCommandBus{result: &ClusterCommandResult{Status: ClusterCommandStatusSucceeded}}
	rt, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       bus,
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(nil)
	node.SetCluster(rt)

	// Remote session with a namespace: resolved through the directory.
	ns, ok := node.AdminSessionNamespace(context.Background(), "sess-remote")
	assert.True(t, ok)
	assert.Equal(t, "acme", ns)

	// Lease without a namespace: invisible ("", false).
	ns, ok = node.AdminSessionNamespace(context.Background(), "sess-nsless")
	assert.False(t, ok)
	assert.Empty(t, ns)

	// No lease at all: not found.
	ns, ok = node.AdminSessionNamespace(context.Background(), "sess-missing")
	assert.False(t, ok)
	assert.Empty(t, ns)
}
