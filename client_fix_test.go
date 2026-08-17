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
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
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
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Json{Json: payloadStruct}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, pubMsg))

	// The published bytes must be parseable JSON with the exact content (not
	// the structpb protobuf text format).
	page, err := node.Broker().History("json-ch", 0, 0)
	require.NoError(t, err)
	pubs := page.Pubs()
	require.Len(t, pubs, 1)
	require.Equal(t, PayloadKindJSON, pubs[0].Kind)

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
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", DenyAll: true},
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
	// Connected envelope, then a separate presence snapshot for the allowed
	// channel (v2 Presence is its own envelope).
	require.False(t, transport.isClosed())
	require.Equal(t, 3, transport.getMessageCount())

	var first clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(0), &first))
	errEnv := first.GetError()
	require.NotNil(t, errEnv, "first message should be the ACL error")
	assert.Equal(t, "ACL_DENIED", errEnv.Code)

	var connectedMsg clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(1), &connectedMsg))
	connected := connectedMsg.GetConnected()
	require.NotNil(t, connected, "second message should be Connected")
	assert.Len(t, connected.GetSubscriptions(), 1)
	assert.Equal(t, "public.room", connected.GetSubscriptions()[0].Channel)

	var presenceMsg clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(2), &presenceMsg))
	presence := presenceMsg.GetPresence()
	require.NotNil(t, presence, "third message should be the presence snapshot for the allowed channel")
	assert.Equal(t, "public.room", presence.GetChannel())
}

// --- PR-KA-A4 §9.7: proxy approval must not bypass static deny ---

// TestClientSession_Subscribe_ProxyAllowDoesNotBypassStaticDeny verifies
// that a proxy which approves a subscription cannot punch a hole in a static
// deny_all rule: the client still receives ACL_DENIED, nothing is subscribed
// and the connection stays up.
func TestClientSession_Subscribe_ProxyAllowDoesNotBypassStaticDeny(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "secret.**", DenyAll: true},
			},
		},
	})
	// A proxy route that matches secret.* for "subscribe" and always allows.
	require.NoError(t, node.AddProxy(&connectAuthProxyStub{}, "secret.*", "subscribe"))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{}},
	}))
	transport.messages = nil

	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "secret.1"}},
			},
		},
	}))

	require.Equal(t, 2, transport.getMessageCount(), "the ACL error followed by the (empty) SubscribeAck")
	var out clientpb.OutboundMessage
	require.NoError(t, (JSONMarshaler{}).Unmarshal(transport.getMessage(0), &out))
	errObj := out.GetError()
	require.NotNil(t, errObj, "the proxy approval must not bypass the static deny")
	assert.Equal(t, "ACL_DENIED", errObj.Code)
	assert.Equal(t, "acl_error", errObj.Type)
	assert.Zero(t, node.Hub().NumSubscribers("secret.1"), "the denied channel must not be subscribed")
	require.False(t, transport.isClosed(), "a denied subscribe must not disconnect")
}

// TestClientSession_Subscribe_ProxyDenyAfterStaticAllow verifies the proxy
// still works as an additional gate: a static allow followed by a proxy deny
// rejects the single request.
func TestClientSession_Subscribe_ProxyDenyAfterStaticAllow(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.AddProxy(&denyingACLProxyStub{}, "rpc.*", "subscribe"))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{}},
	}))
	transport.messages = nil

	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "sub-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "rpc.gate"}},
			},
		},
	}))

	require.Equal(t, 2, transport.getMessageCount(), "the proxy error followed by the (empty) SubscribeAck")
	var out clientpb.OutboundMessage
	require.NoError(t, (JSONMarshaler{}).Unmarshal(transport.getMessage(0), &out))
	errObj := out.GetError()
	require.NotNil(t, errObj)
	assert.Equal(t, "RPC_GATE_DENIED", errObj.Code, "the proxy error must pass through")
	assert.Zero(t, node.Hub().NumSubscribers("rpc.gate"))
}

// denyingACLProxyStub rejects every subscribe ACL with a fixed error.
type denyingACLProxyStub struct{}

func (m *denyingACLProxyStub) RPC(context.Context, *proxy.RPCProxyRequest) (*proxy.RPCProxyResponse, error) {
	return nil, nil
}

func (m *denyingACLProxyStub) Authenticate(context.Context, *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	return &proxy.AuthenticateProxyResponse{}, nil
}

func (m *denyingACLProxyStub) SubscribeAcl(context.Context, *proxy.SubscribeAclProxyRequest) (*proxy.SubscribeAclProxyResponse, error) {
	return &proxy.SubscribeAclProxyResponse{
		Error: &sharedpb.Error{Code: "RPC_GATE_DENIED", Type: "acl_error", Message: "denied by gate"},
	}, nil
}

func (m *denyingACLProxyStub) PublishAcl(context.Context, *proxy.PublishAclProxyRequest) (*proxy.PublishAclProxyResponse, error) {
	return &proxy.PublishAclProxyResponse{}, nil
}

func (m *denyingACLProxyStub) OnConnected(context.Context, *proxy.OnConnectedProxyRequest) (*proxy.OnConnectedProxyResponse, error) {
	return &proxy.OnConnectedProxyResponse{}, nil
}

func (m *denyingACLProxyStub) OnSubscribed(context.Context, *proxy.OnSubscribedProxyRequest) (*proxy.OnSubscribedProxyResponse, error) {
	return &proxy.OnSubscribedProxyResponse{}, nil
}

func (m *denyingACLProxyStub) OnUnsubscribed(context.Context, *proxy.OnUnsubscribedProxyRequest) (*proxy.OnUnsubscribedProxyResponse, error) {
	return &proxy.OnUnsubscribedProxyResponse{}, nil
}

func (m *denyingACLProxyStub) OnDisconnected(context.Context, *proxy.OnDisconnectedProxyRequest) (*proxy.OnDisconnectedProxyResponse, error) {
	return &proxy.OnDisconnectedProxyResponse{}, nil
}

func (m *denyingACLProxyStub) Name() string { return "denying-acl-stub" }

func (m *denyingACLProxyStub) Close() error { return nil }

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
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Binary{Binary: []byte("test payload")}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, pubMsg))
	require.True(t, transport.isClosed())

	assert.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")),
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
	assert.Equal(t, float64(1), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")))

	require.NoError(t, client.Close(Disconnect{}))
	assert.Equal(t, float64(0), testutil.ToFloat64(node.metrics.ConnectionsTotal.WithLabelValues("ws")))
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

// countingSessionDirectory counts every lease (CAS) / snapshot write so
// tests can observe how often syncClusterSessionState runs.
type countingSessionDirectory struct {
	*fakeSessionDirectory
	mu   sync.Mutex
	puts int
}

func (c *countingSessionDirectory) CompareAndSwapSessionLease(ctx context.Context, expected, desired *ClusterSessionLease, ttl time.Duration) (bool, error) {
	ok, err := c.fakeSessionDirectory.CompareAndSwapSessionLease(ctx, expected, desired, ttl)
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return ok, err
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

	// Baseline after connect: each cluster sync writes a lease (via CAS)
	// and a snapshot.
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

// --- PR-KA-A1: a fenced ping refresh must disconnect with 3502 ---

// TestClient_PingRefresh_FencedDisconnects verifies §6.4: when the directory
// no longer recognizes this node's fencing (another node claimed the
// session), the throttled ping refresh disconnects the client with 3502 and
// leaves the new owner's lease untouched.
func TestClient_PingRefresh_FencedDisconnects(t *testing.T) {
	ctx := context.Background()
	directory := &fakeSessionDirectory{lease: &ClusterSessionLease{
		SessionID:     "sess-fenced",
		NodeID:        "node-b",
		IncarnationID: "inc-b",
		LeaseVersion:  3,
		ExpiresAt:     time.Now().Add(time.Hour),
	}}
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
	client.ForceTestIDs("sess-fenced", "user-fenced", "client-fenced")

	// Drive the refresh branch directly (the 10s throttle is not a
	// synchronization point): the sync must detect the fencing and close
	// the connection with DisconnectStale (3502) without unbinding the
	// directory lease.
	client.throttledClusterRefresh()
	require.Eventually(t, func() bool { return transport.isClosed() }, time.Second, 10*time.Millisecond)
	assert.Equal(t, DisconnectStale.Code, transport.getCloseReason().Code)

	// The new owner's lease is untouched: no delete, no write-back.
	lease, err := directory.GetSessionLease(ctx, "sess-fenced")
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, "node-b", lease.NodeID)
	require.Equal(t, uint64(3), lease.LeaseVersion)
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

func (f *fakeEpochHistoryBroker) Publish(ch string, pub *Publication) (uint64, error) {
	return 0, nil
}

func (f *fakeEpochHistoryBroker) PublishTransient(ch string, pub *Publication) error {
	return nil
}

func (f *fakeEpochHistoryBroker) PublishOccupancy(ch string, evt OccupancyEvent) error {
	return nil
}

func (f *fakeEpochHistoryBroker) SetOccupancyHandler(OccupancyHandler) error { return nil }
func (f *fakeEpochHistoryBroker) SetGapHandler(GapHandler)                   {}

func (f *fakeEpochHistoryBroker) History(ch string, sinceOffset uint64, limit int) (*HistoryPage, error) {
	result := make([]*Publication, 0, len(f.pubs))
	for _, p := range f.pubs {
		if p.Offset >= sinceOffset {
			if limit > 0 && len(result) >= limit {
				break
			}
			result = append(result, p)
		}
	}
	return &HistoryPage{Publications: result}, nil
}

func TestClient_Connect_RecoveryFreshFromStart(t *testing.T) {
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

	// fresh=true replays from the beginning, ignoring any cursor hint: a
	// client that wants the full history says so explicitly (KD-K22), never
	// by omitting an epoch or sending offset 0.
	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "epoch-ch", Recover: true, Fresh: true},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	replays := replayPublications(outboundMessages(t, transport))
	assert.Equal(t, []uint64{1, 2, 3}, publicationOffsets(replays),
		"fresh=true must recover from the beginning")
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

	// A client cursor is a hint: offset 2 was already seen, so only offset 3
	// is replayed. The cursor's stream epoch tags the generation it refers to.
	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "epoch-ch", Recover: true, Cursor: cursorOf("v2", 2)},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	replays := replayPublications(outboundMessages(t, transport))
	assert.Equal(t, []uint64{3}, publicationOffsets(replays), "an epoch-tagged cursor continues incrementally")
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
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("ws")))

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
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("ws")), "resume must not double count")

	// Closing the resumed client balances the gauge back to zero.
	require.NoError(t, clientB.Close(Disconnect{}))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.ConnectionsTotal.WithLabelValues("ws")))
}

// Task 12: connect-time recovery must preserve the original payload type: a
// text message recovered from history arrives as Payload_Text, not Binary.
func TestClient_Recovery_PreservesPayloadType(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// Publish two text messages; recovery from offset 1 must return the
	// second one (sinceOffset = 2). The client must present the broker epoch
	// or recovery falls back to the beginning by design.
	first, err := node.Publish("recovery.types", &Publication{Payload: []byte("m1"), Kind: PayloadKindText})
	require.NoError(t, err)
	_, err = node.Publish("recovery.types", &Publication{Payload: []byte("m2"), Kind: PayloadKindText})
	require.NoError(t, err)
	epocher, ok := node.Broker().(interface{ Epoch() string })
	require.True(t, ok)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "recovery.types", Recover: true, Cursor: cursorOf(epocher.Epoch(), first)},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))

	replays := replayPublications(outboundMessages(t, transport))
	require.Len(t, replays, 1)
	msgs := replays[0].GetMessages()
	require.Len(t, msgs, 1)
	payload := msgs[0].GetPayload()
	require.NotNil(t, payload)
	require.True(t, msgs[0].GetReplay(), "recovered messages must carry replay=true")
	require.IsType(t, &sharedv2.Payload_Text{}, payload.Data, "recovered payload must keep the text variant")
	require.Equal(t, "m2", payload.GetText())
}

// --- Fix task 1 (P0-7): ephemeral subscriptions must not register presence
// or publish join/leave events ---

// presenceEventObserver subscribes to a channel exactly and counts the
// first-class presence events (presence_event envelopes) it receives. The
// observer itself is a tracked member of the channel, which must be taken
// into account by store-content assertions in tests that use it.
type presenceEventObserver struct {
	transport *capturingTransport
	client    *Client
}

func newPresenceEventObserver(t *testing.T, node *Node, channel string) *presenceEventObserver {
	t.Helper()
	ctx := context.Background()
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	connectMsg := &clientpb.InboundMessage{
		Id:       "obs-connect",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "presence-observer"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	subMsg := &clientpb.InboundMessage{
		Id: "obs-sub",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: channel}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))
	transport.messages = nil
	return &presenceEventObserver{transport: transport, client: client}
}

// eventCount returns the number of presence events received since the last
// reset.
func (o *presenceEventObserver) eventCount() int {
	count := 0
	for i := 0; i < o.transport.getMessageCount(); i++ {
		var out clientpb.OutboundMessage
		err := (JSONMarshaler{}).Unmarshal(o.transport.getMessage(i), &out)
		if err != nil {
			continue
		}
		if out.GetPresenceEvent() != nil {
			count++
		}
	}
	return count
}

func TestClient_EphemeralSubscription_NoPresenceOrEvents(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	const ch = "ephemeral-ch"
	observer := newPresenceEventObserver(t, node, ch)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	// Connect with an ephemeral subscription.
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: ch, Ephemeral: true},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	// The subscription exists (messages are delivered)...
	assert.Equal(t, 2, node.Hub().NumSubscribers(ch))
	// ...but no presence record for the ephemeral session and no join event.
	// The observer is a tracked member of ch, so the store holds exactly its
	// entry and nothing for the ephemeral session.
	presence, err := node.Presence(ctx, ch)
	require.NoError(t, err)
	require.Len(t, presence, 1, "ephemeral subscription must not register presence (only the observer may be present)")
	require.Contains(t, presence, observer.client.SessionID())
	time.Sleep(50 * time.Millisecond) // presence events are published async
	assert.Zero(t, observer.eventCount(), "ephemeral connect subscription must not publish a join event")

	// An ephemeral Subscribe must behave the same.
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: ch + "-2", Ephemeral: true}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))
	assert.Equal(t, 1, node.Hub().NumSubscribers(ch+"-2"))
	presence, err = node.Presence(ctx, ch+"-2")
	require.NoError(t, err)
	assert.Empty(t, presence)
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, observer.eventCount(), "ephemeral subscribe must not publish a join event")

	// Unsubscribing an ephemeral channel must not publish a leave event.
	unsubMsg := &clientpb.InboundMessage{
		Id: "msg-3",
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: ch + "-2"}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, unsubMsg))
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, observer.eventCount(), "ephemeral unsubscribe must not publish a leave event")

	// Closing a client with only ephemeral subscriptions must not publish
	// leave events either.
	require.NoError(t, client.Close(Disconnect{}))
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, observer.eventCount(), "close of ephemeral subscriptions must not publish leave events")
	assert.Zero(t, node.Hub().NumSubscribers(ch+"-2"), "the ephemeral subscription must be removed on close")
}

// Control test: a non-ephemeral subscription registers presence and publishes
// join/leave events, proving the observer above is wired correctly.
func TestClient_NonEphemeralSubscription_PresenceAndEvents(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)

	const ch = "plain-ch"
	observer := newPresenceEventObserver(t, node, ch)

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: ch},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))

	presence, err := node.Presence(ctx, ch)
	require.NoError(t, err)
	require.Len(t, presence, 2, "observer and the non-ephemeral subscription must both register presence")
	require.Contains(t, presence, client.SessionID())
	require.Eventually(t, func() bool { return observer.eventCount() == 1 }, time.Second, 10*time.Millisecond,
		"non-ephemeral subscription must publish one join event")

	unsubMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Unsubscribe{
			Unsubscribe: &clientpb.Unsubscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: ch}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, unsubMsg))
	require.Eventually(t, func() bool { return observer.eventCount() == 2 }, time.Second, 10*time.Millisecond,
		"non-ephemeral unsubscribe must publish one leave event")
}

// --- Fix task 2 (P1-A1): a failed connect must disconnect, never leave a
// half-open connection ---

// TestClient_Connect_AddClientClusterSyncFailureDisconnects simulates a
// cluster session sync failure inside AddClient: the connection must be
// closed with DisconnectInternal, not left half-open.
func TestClient_Connect_AddClientClusterSyncFailureDisconnects(t *testing.T) {
	ctx := context.Background()
	directory := &fakeSessionDirectory{casErr: errors.New("redis down")}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(&config.Server{RequireAuth: true})
	node.SetCluster(runtime)
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-1", Token: "t"},
		},
	}
	err = client.HandleMessage(ctx, msg)
	require.Error(t, err, "the cluster sync failure must surface as an error")

	// The connection must be closed with the internal-error code...
	require.True(t, transport.isClosed(), "connect failure must close the transport")
	assert.Equal(t, DisconnectInternal.Code, transport.getCloseReason().Code)
	// ...and no half-registered state may remain in the hub.
	assert.Nil(t, node.Hub().LookupSession(client.SessionID()))
}

// TestClient_Connect_RemoteResumeFailureDisconnects simulates a remote resume
// that fails with a plain error (lease store unreachable): the connection
// must be closed instead of remaining half-open.
func TestClient_Connect_RemoteResumeFailureDisconnects(t *testing.T) {
	ctx := context.Background()
	directory := &failingGetLeaseDirectory{err: errors.New("lease store down")}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(&config.Server{RequireAuth: true})
	node.SetCluster(runtime)
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-1", Token: "t", SessionId: "sess-remote"},
		},
	}
	err = client.HandleMessage(ctx, msg)
	require.Error(t, err, "the remote resume failure must surface as an error")

	require.True(t, transport.isClosed())
	assert.Equal(t, DisconnectInternal.Code, transport.getCloseReason().Code)
	assert.Nil(t, node.Hub().LookupSession("sess-remote"))
}

// userPerClientAuthProxy authenticates every client as "user-<client-id>".
type userPerClientAuthProxy struct {
	connectAuthProxyStub
}

func (m *userPerClientAuthProxy) Authenticate(_ context.Context, req *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	return &proxy.AuthenticateProxyResponse{UserInfo: &proxy.UserInfo{ID: "user-" + req.ClientID}}, nil
}

// TestClient_Connect_ResumeAtUserLimit_NoZombie documents the PR-KA-B1 §6
// shape: a same-user local resume keeps the Session object (and its
// subscriptions) stable, the old transport is detached, and no zombie can
// exist because there is no replaced object.
func TestClient_Connect_ResumeAtUserLimit_NoZombie(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true, Limits: config.Limits{MaxConnectionsPerUser: 1}})
	authProxy := &userPerClientAuthProxy{}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	// A (user-a) connects and subscribes.
	transportA := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transportA, JSONMarshaler{})
	require.NoError(t, err)
	connectA := &clientpb.InboundMessage{
		Id: "msg-a1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "a", Token: "t"},
		},
	}
	require.NoError(t, clientA.HandleMessage(ctx, connectA))
	sessionA := clientA.SessionID()
	subA := &clientpb.InboundMessage{
		Id: "msg-a2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "zombie-ch"}},
			},
		},
	}
	require.NoError(t, clientA.HandleMessage(ctx, subA))
	require.Equal(t, 1, node.Hub().NumSubscribers("zombie-ch"))

	// B (user-b) occupies the only slot of its user.
	transportB := &capturingTransport{}
	clientB, _, err := NewClient(ctx, node, transportB, JSONMarshaler{})
	require.NoError(t, err)
	connectB := &clientpb.InboundMessage{
		Id: "msg-b1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "b", Token: "t"},
		},
	}
	require.NoError(t, clientB.HandleMessage(ctx, connectB))
	sessionB := clientB.SessionID()

	// C (user-a) resumes A's session. The resume inherits user-a, so the
	// per-user limit of 1 does not reject it (same-user replacement keeps the
	// count unchanged); the session must be handed over cleanly.
	transportC := &capturingTransport{}
	clientC, _, err := NewClient(ctx, node, transportC, JSONMarshaler{})
	require.NoError(t, err)
	resumeC := &clientpb.InboundMessage{
		Id: "msg-c1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "a", Token: "t", SessionId: sessionA},
		},
	}
	require.NoError(t, clientC.HandleMessage(ctx, resumeC))

	// The new connection stays up and serves the SAME session object...
	require.False(t, transportC.isClosed())
	assert.Same(t, clientA, node.Hub().LookupSession(sessionA), "the resumed session pointer must be stable")
	// ...the subscription still points at the same session (no shard scan,
	// no matcher rebuild)...
	assert.Equal(t, 1, node.Hub().NumSubscribers("zombie-ch"))
	sub, ok := node.Hub().LookupSubscriber("zombie-ch", clientA)
	require.True(t, ok)
	assert.Same(t, clientA, sub.Session)
	// ...the new connection's writes go through the session's writer to the
	// new transport, and the old transport was closed by Detach.
	require.True(t, transportA.isClosed(), "the detached transport is closed by Detach")

	// Post-resume traffic on the NEW connection's read loop must delegate to
	// the resumed session: a Ping is answered with a Pong on the new
	// transport, and the resumed session's state (subscribedChannels) is
	// used.
	transportC.messages = nil
	pingC := &clientpb.InboundMessage{
		Id: "msg-c2",
		Envelope: &clientpb.InboundMessage_Ping{
			Ping: &clientpb.Ping{},
		},
	}
	require.NoError(t, clientC.HandleMessage(ctx, pingC))
	require.Equal(t, 1, transportC.getMessageCount(), "post-resume traffic must be served via the resumed session")
	var pong clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(transportC.getMessage(0), &pong))
	require.NotNil(t, pong.GetPong(), "post-resume Ping must be answered with a Pong")

	// The unrelated B session is untouched.
	assert.Same(t, clientB, node.Hub().LookupSession(sessionB))
	assert.False(t, transportB.isClosed())
}

// TestClient_Connect_ResumeAtUserLimit_KeepsOldSessionAttached exercises
// PR-KA-B1 §6.5 / §9.3: when a cross-user resume hits the per-user
// connection limit, the OLD session must stay fully Attached (hub entry,
// subscriptions, presence, cluster state, transport open) and only the new
// connection is closed.
func TestClient_Connect_ResumeAtUserLimit_KeepsOldSessionAttached(t *testing.T) {
	ctx := context.Background()
	directory := &recordingSessionDirectory{fakeSessionDirectory: &fakeSessionDirectory{}}
	runtime, err := NewCluster(ClusterOptions{Enabled: true, NodeID: "node-a", IncarnationID: "inc-a", Backend: "memory"}, ClusterDependencies{
		SessionDirectory: directory,
		CommandBus:       &fakeClusterCommandBus{},
		QueryStore:       fakeQueryStore{},
	})
	require.NoError(t, err)

	node := NewNode(&config.Server{RequireAuth: true, Limits: config.Limits{MaxConnectionsPerUser: 1}})
	node.SetCluster(runtime)
	authProxy := &userPerClientAuthProxy{}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	// A (user-a) connects, subscribes and owns presence plus cluster state.
	transportA := &capturingTransport{}
	clientA, _, err := NewClient(ctx, node, transportA, JSONMarshaler{})
	require.NoError(t, err)
	connectA := &clientpb.InboundMessage{
		Id: "msg-a1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "a", Token: "t"},
		},
	}
	require.NoError(t, clientA.HandleMessage(ctx, connectA))
	sessionA := clientA.SessionID()
	subA := &clientpb.InboundMessage{
		Id: "msg-a2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "zombie-ch"}},
			},
		},
	}
	require.NoError(t, clientA.HandleMessage(ctx, subA))
	require.Equal(t, 1, node.Hub().NumSubscribers("zombie-ch"))
	present, err := node.presence.Get(ctx, "zombie-ch")
	require.NoError(t, err)
	require.Contains(t, present, sessionA, "the old session must own presence before the failed resume")

	// B (user-b) occupies the only slot of its user.
	transportB := &capturingTransport{}
	clientB, _, err := NewClient(ctx, node, transportB, JSONMarshaler{})
	require.NoError(t, err)
	connectB := &clientpb.InboundMessage{
		Id: "msg-b1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "b", Token: "t"},
		},
	}
	require.NoError(t, clientB.HandleMessage(ctx, connectB))
	sessionB := clientB.SessionID()

	// C authenticates as user-b (at its connection limit) and tries to resume
	// A's session: the per-user limit check runs BEFORE the old attachment is
	// detached, so the old session survives untouched.
	transportC := &capturingTransport{}
	clientC, _, err := NewClient(ctx, node, transportC, JSONMarshaler{})
	require.NoError(t, err)
	resumeC := &clientpb.InboundMessage{
		Id: "msg-c1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "b", Token: "t", SessionId: sessionA},
		},
	}
	require.NoError(t, clientC.HandleMessage(ctx, resumeC))

	// The new connection is closed with the connection-limit code...
	require.True(t, transportC.isClosed())
	assert.Equal(t, DisconnectConnectionLimit.Code, transportC.getCloseReason().Code)

	// ...and the old session is still fully Attached: hub entry,
	// subscription, presence, cluster state and an open transport.
	assert.Same(t, clientA, node.Hub().LookupSession(sessionA), "the old session must stay in the hub")
	assert.Equal(t, SessionAttached, clientA.State(), "the old session must stay Attached")
	assert.Equal(t, 1, node.Hub().NumSubscribers("zombie-ch"), "the old session's subscription must stay")
	present, err = node.presence.Get(ctx, "zombie-ch")
	require.NoError(t, err)
	assert.Contains(t, present, sessionA, "the old session's presence must stay")
	assert.False(t, transportA.isClosed(), "the old session's transport must stay open")
	require.False(t, directory.deletedLease, "the old session's lease must not be deleted")
	require.False(t, directory.deletedSnapshot, "the old session's snapshot must not be deleted")

	// The unrelated B session is untouched.
	assert.Same(t, clientB, node.Hub().LookupSession(sessionB))
	assert.False(t, transportB.isClosed())
}

// failingGetLeaseDirectory makes GetSessionLease fail, simulating an
// unreachable lease store during a remote resume.
type failingGetLeaseDirectory struct {
	*fakeSessionDirectory
	err error
}

func (f *failingGetLeaseDirectory) GetSessionLease(context.Context, string) (*ClusterSessionLease, error) {
	return nil, f.err
}

// --- Fix task 4 (P1-A3): close() must not leak subscriptions added by a
// concurrent Subscribe ---

func TestNode_AddSubscription_RejectedWhenClientClosed(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.Close(Disconnect{}))

	err = node.AddSubscription(ctx, "closed-ch", NewSubscriber(client, false))
	require.Error(t, err, "subscribing a closed client must fail")
	assert.Zero(t, node.Hub().NumSubscribers("closed-ch"))
	assert.Empty(t, client.subscriptionList())
}

func TestClient_Close_ConcurrentSubscribe_NoLeak(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	_ = node.Run(ctx)
	transport := &capturingTransport{}

	client, closeFn, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-race", "user-race", "client-race")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				msg := &clientpb.InboundMessage{
					Id: fmt.Sprintf("sub-%d-%d", worker, j),
					Envelope: &clientpb.InboundMessage_Subscribe{
						Subscribe: &clientpb.Subscribe{
							Subscriptions: []*clientpb.Subscription{{Channel: fmt.Sprintf("race-ch-%d", j%64)}},
						},
					},
				}
				_ = client.HandleMessage(ctx, msg)
			}
		}(i)
	}

	time.Sleep(20 * time.Millisecond)
	close(stop)
	require.NoError(t, closeFn())
	wg.Wait()

	for j := 0; j < 64; j++ {
		assert.Zero(t, node.Hub().NumSubscribers(fmt.Sprintf("race-ch-%d", j)),
			"no subscription may leak in the hub after close")
	}
	assert.Empty(t, client.subscriptionList())
}

// --- Fix task 5 (P1-A4): ClientInfo must not read fields unlocked ---

func TestClient_ClientInfo_ConcurrentWithConnect(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true})
	authProxy := &connectAuthProxyStub{userID: "user-1"}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{ClientId: "client-1", Token: "t", SessionId: "sess-1"},
		},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = client.HandleMessage(ctx, connectMsg)
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = client.ClientInfo()
		}
	}()
	wg.Wait()

	info := client.ClientInfo()
	assert.Equal(t, "client-1", info.ClientID)
	assert.Equal(t, "sess-1", info.SessionID)
	assert.Equal(t, "user-1", info.UserID)
}

// --- Fix task 7 (P1-A6): broker start failure must surface through Run ---

// failStartBroker fails immediately at Start and never reports Ready.
type failStartBroker struct{}

func (b *failStartBroker) Start(context.Context, PublicationHandler) error {
	return errors.New("redis connection refused")
}
func (b *failStartBroker) Ready() <-chan struct{}                            { return make(chan struct{}) }
func (b *failStartBroker) Subscribe(string) error                            { return nil }
func (b *failStartBroker) Unsubscribe(string) error                          { return nil }
func (b *failStartBroker) Publish(string, *Publication) (uint64, error)      { return 0, nil }
func (b *failStartBroker) PublishTransient(string, *Publication) error       { return nil }
func (b *failStartBroker) PublishOccupancy(string, OccupancyEvent) error     { return nil }
func (b *failStartBroker) SetOccupancyHandler(OccupancyHandler) error        { return nil }
func (b *failStartBroker) SetGapHandler(GapHandler)                          {}
func (b *failStartBroker) History(string, uint64, int) (*HistoryPage, error) { return nil, nil }

func TestNode_Run_BrokerStartFailureReturnsError(t *testing.T) {
	node := NewNode(nil)
	node.SetBroker(&failStartBroker{})
	err := node.Run(context.Background())
	require.Error(t, err, "a broker that fails to start must surface as a Run error, not a panic")
	assert.Contains(t, err.Error(), "redis connection refused")
}

// --- Fix task 8: SessionAttached must be set once the connect completes ---

func TestClient_Connect_SetsStateAttached(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	assert.Equal(t, SessionAuthenticating, client.State(), "NewClient must start in Authenticating")
	assert.NotEqual(t, SessionAttached, client.State())

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	assert.Equal(t, SessionAttached, client.State(), "a successful connect must move the client to SessionAttached")
}

// --- Fix task 14: subscription limits must count only newly added channels
// (duplicates and ACL-denied channels are free) ---

func TestClient_SubscribeLimit_IgnoresDuplicatesAndACLDenied(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{
		Limits: config.Limits{MaxSubscriptionsPerClient: 2},
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", DenyAll: true},
			},
		},
	})
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	connectMsg := &clientpb.InboundMessage{
		Id:       "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}
	require.NoError(t, client.HandleMessage(ctx, connectMsg))
	transport.messages = nil

	// ch1, a duplicate of ch1, and an ACL-denied channel: only ch1 and ch2
	// are new, so the batch (2 new) fits the limit of 2.
	subMsg := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{
					{Channel: "ch1"},
					{Channel: "ch1"},
					{Channel: "private.secret"},
					{Channel: "ch2"},
				},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, subMsg))
	require.False(t, transport.isClosed(), "duplicates and ACL-denied channels must not count toward the limit")
	assert.Equal(t, 1, node.Hub().NumSubscribers("ch1"))
	assert.Equal(t, 1, node.Hub().NumSubscribers("ch2"))
	assert.Zero(t, node.Hub().NumSubscribers("private.secret"))

	// A genuinely new channel now exceeds the limit and disconnects.
	overMsg := &clientpb.InboundMessage{
		Id: "msg-3",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{
				Subscriptions: []*clientpb.Subscription{{Channel: "ch3"}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, overMsg))
	require.True(t, transport.isClosed())
	assert.Equal(t, DisconnectChannelLimit.Code, transport.getCloseReason().Code)
}

// --- Fix task 15: the auth proxy must receive the server-generated session
// ID, never a client-forged one ---

// recordingAuthProxy records every Authenticate request's session ID.
type recordingAuthProxy struct {
	connectAuthProxyStub
	mu         sync.Mutex
	sessionIDs []string
}

func (m *recordingAuthProxy) Authenticate(ctx context.Context, req *proxy.AuthenticateProxyRequest) (*proxy.AuthenticateProxyResponse, error) {
	m.mu.Lock()
	m.sessionIDs = append(m.sessionIDs, req.SessionID)
	m.mu.Unlock()
	return m.connectAuthProxyStub.Authenticate(ctx, req)
}

func TestClient_Connect_AuthProxyReceivesServerSessionID(t *testing.T) {
	ctx := context.Background()
	node := NewNode(&config.Server{RequireAuth: true})
	authProxy := &recordingAuthProxy{connectAuthProxyStub: connectAuthProxyStub{userID: "user-1"}}
	require.NoError(t, node.AddProxy(authProxy, "", SystemMethodAuthenticate))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	originalSessionID := client.SessionID()
	require.NotEmpty(t, originalSessionID)

	msg := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId:  "client-1",
				Token:     "t",
				SessionId: "client-forged-session",
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, msg))
	require.False(t, transport.isClosed())

	authProxy.mu.Lock()
	defer authProxy.mu.Unlock()
	require.Len(t, authProxy.sessionIDs, 1)
	assert.Equal(t, originalSessionID, authProxy.sessionIDs[0],
		"the auth proxy must see the server-generated session ID, not the client-supplied one")
	assert.NotEqual(t, "client-forged-session", authProxy.sessionIDs[0])
}

// --- Fix task 16: survey replies must carry their own request ID ---

func TestClient_SurveyReply_WithoutRequestID_Dropped(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}

	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-1", "user-1", "client-1")

	// Register an active survey expecting this session.
	survey := NewSurvey("survey-1", "ch", []byte("ping"), time.Second)
	survey.AddExpectedSession("sess-1")
	node.surveyMu.Lock()
	node.surveys["survey-1"] = survey
	node.surveyMu.Unlock()

	// A reply without a request ID must be dropped (no last-request fallback).
	replyNoID := &clientpb.InboundMessage{
		Id: "msg-1",
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Binary{Binary: []byte("pong")}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, replyNoID))
	assert.Empty(t, survey.Results(), "a reply without request id must not be recorded")

	// Control: a reply carrying the request ID is recorded.
	replyWithID := &clientpb.InboundMessage{
		Id: "msg-2",
		Envelope: &clientpb.InboundMessage_SurveyReply{
			SurveyReply: &clientpb.SurveyReply{
				RequestId: "survey-1",
				Payload:   &sharedv2.Payload{Data: &sharedv2.Payload_Binary{Binary: []byte("pong")}},
			},
		},
	}
	require.NoError(t, client.HandleMessage(ctx, replyWithID))
	require.Len(t, survey.Results(), 1)
	assert.Equal(t, []byte("pong"), survey.Results()[0].Payload)
}

// --- PR-KA-A3: unroutable patterns soft-fail per channel ---

// connectClient is a test helper: a fresh node + client with one Connect
// frame handled, returning the client and its capturing transport.
func connectClient(t *testing.T, node *Node) (*Client, *capturingTransport) {
	t.Helper()
	ctx := context.Background()
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{ClientId: "client-1"}},
	}))
	transport.resetMessages()
	return client, transport
}

// TestClient_SubscribeUnroutablePattern_SoftFail pins A3 §8-5: subscribing an
// unroutable pattern ("*.room") sends a top-level PATTERN_NOT_ROUTABLE
// envelope, keeps the connection up, leaves no hub subscription, and does not
// roll back the other channels in the same request.
func TestClient_SubscribeUnroutablePattern_SoftFail(t *testing.T) {
	node := NewNode(nil)
	client, transport := connectClient(t, node)

	require.NoError(t, client.HandleMessage(context.Background(), &clientpb.InboundMessage{
		Id: "subscribe-1",
		Envelope: &clientpb.InboundMessage_Subscribe{
			Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{
				{Channel: "*.room"},
				{Channel: "good.ch"},
			}},
		},
	}))

	// Connection must stay up.
	require.False(t, transport.isClosed())

	msgs := transport.snapshotMessages()
	require.Len(t, msgs, 2, "one error envelope for the unroutable channel, one SubscribeAck")

	var out clientpb.OutboundMessage
	require.NoError(t, (JSONMarshaler{}).Unmarshal(msgs[0], &out))
	errEnv := out.GetError()
	require.NotNil(t, errEnv, "first envelope must be the PATTERN_NOT_ROUTABLE error")
	require.Equal(t, "PATTERN_NOT_ROUTABLE", errEnv.GetCode())
	require.Equal(t, "request_error", errEnv.GetType())

	require.NoError(t, (JSONMarshaler{}).Unmarshal(msgs[1], &out))
	ack := out.GetSubscribeAck()
	require.NotNil(t, ack, "second envelope must be the SubscribeAck")
	require.Len(t, ack.GetSubscriptions(), 1, "only the routable channel is acknowledged")
	require.Equal(t, "good.ch", ack.GetSubscriptions()[0].GetChannel())

	// The unroutable pattern left no hub subscription behind.
	require.False(t, client.hasSubscription("*.room"))
	require.True(t, client.hasSubscription("good.ch"))
	require.Empty(t, node.Hub().GetMatchingSubscribers("x.room"),
		"the unroutable pattern must not be registered in the hub matcher")
}

// TestClient_ConnectWithUnroutableSubscription_SoftFail pins A3 §8-6: a
// Connect carrying an unroutable channel still succeeds; the channel is
// skipped (error envelope) and stays out of Connected.Subscriptions while the
// routable channels are subscribed.
func TestClient_ConnectWithUnroutableSubscription_SoftFail(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)

	require.NoError(t, client.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{
			Connect: &clientpb.Connect{
				ClientId: "client-1",
				Subscriptions: []*clientpb.Subscription{
					{Channel: "*.room"},
					{Channel: "good.ch"},
				},
			},
		},
	}))

	// The Connect itself must succeed and the connection must stay up.
	require.False(t, transport.isClosed())

	msgs := transport.snapshotMessages()
	require.Len(t, msgs, 3, "one error envelope for the unroutable channel, one Connected, one presence snapshot")

	var out clientpb.OutboundMessage
	require.NoError(t, (JSONMarshaler{}).Unmarshal(msgs[0], &out))
	errEnv := out.GetError()
	require.NotNil(t, errEnv, "first envelope must be the PATTERN_NOT_ROUTABLE error")
	require.Equal(t, "PATTERN_NOT_ROUTABLE", errEnv.GetCode())

	require.NoError(t, (JSONMarshaler{}).Unmarshal(msgs[1], &out))
	connected := out.GetConnected()
	require.NotNil(t, connected, "second envelope must be Connected")
	var channels []string
	for _, sub := range connected.GetSubscriptions() {
		channels = append(channels, sub.GetChannel())
	}
	require.Contains(t, channels, "good.ch")
	require.NotContains(t, channels, "*.room")
	require.False(t, client.hasSubscription("*.room"))
	require.True(t, client.hasSubscription("good.ch"))

	require.NoError(t, (JSONMarshaler{}).Unmarshal(msgs[2], &out))
	presence := out.GetPresence()
	require.NotNil(t, presence, "third envelope must be the presence snapshot for the routable channel")
	require.Equal(t, "good.ch", presence.GetChannel())
}
