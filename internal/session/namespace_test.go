package session

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/metrics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// TestSession_CheckNamespace pins the session-boundary guard: namespace-less
// sessions skip the check (test harnesses only), namespaced sessions reject
// cross-namespace and non-namespaced channels with NAMESPACE_MISMATCH.
func TestSession_CheckNamespace(t *testing.T) {
	assert := assert.New(t)

	c, _, err := NewClient(context.Background(), newFakeRuntime(), &mockTransport{}, JSONMarshaler{})
	require.NoError(t, err)

	// No namespace resolved: the guard is a pass-through.
	assert.Nil(c.checkNamespace("anything"))
	assert.Nil(c.checkNamespace("other:ch"))

	c.SetNamespaceForTest("acme")

	assert.Nil(c.checkNamespace("acme:chat.room1"), "in-namespace channel passes")
	assert.Nil(c.checkNamespace("acme:*"), "in-namespace wildcard passes")

	nsErr := c.checkNamespace("other:chat")
	assert.NotNil(nsErr)
	assert.Equal("NAMESPACE_MISMATCH", nsErr.Code)
	assert.Equal("acl_error", nsErr.Type)

	nsErr = c.checkNamespace("chat.room1")
	assert.NotNil(nsErr, "non-namespaced channels are out of scope")
	assert.Equal("NAMESPACE_MISMATCH", nsErr.Code)
}

// TestSession_NamespaceInIdentitySnapshots verifies the namespace travels with
// every identity snapshot the resume/cluster paths consume.
func TestSession_NamespaceInIdentitySnapshots(t *testing.T) {
	assert := assert.New(t)

	c, _, err := NewClient(context.Background(), newFakeRuntime(), &mockTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	c.ForceTestIDs("sess-1", "user-1", "client-1")

	assert.Empty(c.Namespace())
	assert.Empty(c.ClientInfo().Namespace)
	assert.Empty(c.SnapshotIdentity().Namespace)

	c.SetNamespaceForTest("acme")
	assert.Equal("acme", c.Namespace())
	assert.Equal("acme", c.ClientInfo().Namespace)
	assert.Equal("acme", c.SnapshotIdentity().Namespace)

	// AdoptIdentity fills an empty namespace; a non-empty one overwrites with
	// the same semantics as the user ID (resume paths pre-check consistency).
	c2, _, err := NewClient(context.Background(), newFakeRuntime(), &mockTransport{}, JSONMarshaler{})
	require.NoError(t, err)
	c2.AdoptIdentity("sess-2", "", "user-1", "client-1", nil, 1)
	assert.Empty(c2.Namespace(), "an empty namespace leaves the field unchanged")
	c2.AdoptIdentity("sess-2", "acme", "user-1", "client-1", nil, 1)
	assert.Equal("acme", c2.Namespace())
	c2.AdoptIdentity("sess-2", "other", "user-1", "client-1", nil, 2)
	assert.Equal("other", c2.Namespace())
}

// TestHub_NamespaceScopedConnectionLimit verifies the per-user connection
// limit is scoped to the (namespace, user) pair: the same user ID under two
// namespaces never shares slots.
func TestHub_NamespaceScopedConnectionLimit(t *testing.T) {
	assert := assert.New(t)
	h := NewHub(0, 1) // 1 connection per (namespace, user)

	mk := func(sessionID, namespace, userID string) *Session {
		c, _, err := NewClient(context.Background(), newFakeRuntime(), &mockTransport{}, JSONMarshaler{})
		require.NoError(t, err)
		c.mu.Lock()
		c.session = sessionID
		c.namespace = namespace
		c.user = userID
		c.client = "client-" + sessionID
		c.mu.Unlock()
		return c
	}

	assert.NoError(h.Add(mk("s1", "acme", "user-1")))
	assert.ErrorIs(h.Add(mk("s2", "acme", "user-1")), DisconnectConnectionLimit,
		"the second acme/user-1 session must hit the limit")
	assert.NoError(h.Add(mk("s3", "other", "user-1")),
		"the same user ID under another namespace has its own slot")
	assert.NoError(h.Add(mk("s4", "", "user-1")),
		"a namespace-less session does not consume any namespaced slot")

	assert.Equal("acme", h.SessionsByUser("acme", "user-1")[0].Namespace())
	assert.Len(h.SessionsByUser("acme", "user-1"), 1)
	assert.Len(h.SessionsByUser("other", "user-1"), 1)
	assert.Empty(h.SessionsByUser("acme", "user-2"))
}

// TestSession_PublishNamespaceGuard drives handlePublish on a namespaced
// session: an out-of-namespace publish is rejected with a NAMESPACE_MISMATCH
// error envelope and never reaches the broker.
func TestSession_PublishNamespaceGuard(t *testing.T) {
	assert := assert.New(t)
	reg := prometheus.NewRegistry()
	rt := newFakeRuntime()
	rt.metrics = metrics.NewMetrics(reg)
	transport := &mockTransport{}

	c, _, err := NewClient(context.Background(), rt, transport, JSONMarshaler{})
	require.NoError(t, err)
	c.ForceTestIDs("sess-ns", "user-1", "client-1")
	c.SetNamespaceForTest("acme")
	c.MarkAuthenticated()
	rt.hub.Add(c)

	pub := &clientpb.Publish{Channel: "other:chat", Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}}}
	in := &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_Publish{Publish: pub}}
	require.NoError(t, c.HandleMessage(context.Background(), in))

	// The reply is a NAMESPACE_MISMATCH error envelope, no PublishAck.
	require.Equal(t, 1, transport.getMessageCount())
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &out))
	errEnv := out.GetError()
	require.NotNil(t, errEnv)
	assert.Equal("NAMESPACE_MISMATCH", errEnv.Code)

	// An in-namespace publish passes the guard (reaches the broker path) and
	// gets a PublishAck.
	in = &clientpb.InboundMessage{Id: "m2", Envelope: &clientpb.InboundMessage_Publish{Publish: &clientpb.Publish{
		Channel: "acme:chat",
		Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}},
	}}}
	require.NoError(t, c.HandleMessage(context.Background(), in))
	require.Equal(t, 2, transport.getMessageCount())
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(1), &out))
	assert.NotNil(t, out.GetPublishAck(), "the in-namespace publish must be acked")
}
