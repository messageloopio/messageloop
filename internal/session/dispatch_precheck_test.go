package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// precheckClass tells the census test whether an InboundMessage envelope
// variant is enforced by the dispatch-entry namespace precheck or explicitly
// exempt from it (with the verified reason).
type precheckClass string

const (
	precheckCovered precheckClass = "covered"
	precheckExempt  precheckClass = "exempt"
)

type precheckEntry struct {
	class  precheckClass
	reason string
}

// precheckEnvelopeRegistry is the G1 census table: every InboundMessage
// envelope oneof variant (proto field name) must be registered here, covered
// or exempt. When you add a variant to protocol/client/v2/service.proto,
// register it in this table in the same change — the census test fails on an
// unregistered variant, so a new channel-carrying message cannot silently
// skip the namespace precheck (same idiom as error_codes_test.go).
var precheckEnvelopeRegistry = map[string]precheckEntry{
	// Exempt, each reason verified against the handler (see precheckNamespace).
	"connect":      {precheckExempt, "resolves the session namespace itself; its initial subscriptions pass checkSubscribeACL step 0"},
	"ping":         {precheckExempt, "carries no channel reference"},
	"pong":         {precheckExempt, "carries no channel reference"},
	"rpc_request":  {precheckExempt, "handleRPC enforces checkNamespace itself before proxy routing (the channel participates in the route match)"},
	"survey_reply": {precheckExempt, "no channel: routed by request id to an in-flight survey, and AddSurveyResponse drops replies from sessions the survey was not sent to"},
	// Covered: enforced at the dispatch entry, handlers keep defense in depth.
	"subscribe":      {precheckCovered, "per-channel filter; same NAMESPACE_MISMATCH envelope as checkSubscribeACL step 0"},
	"publish":        {precheckCovered, "single NAMESPACE_MISMATCH envelope; empty/wildcard channels left to the handler's BAD_REQUEST"},
	"unsubscribe":    {precheckCovered, "single NAMESPACE_MISMATCH envelope (new check: closes the proxy-notification channel-name leak)"},
	"sub_refresh":    {precheckCovered, "single NAMESPACE_MISMATCH envelope (review #4: forged cross-namespace presence leave / backend channel-name leak)"},
	"survey_request": {precheckCovered, "NAMESPACE_MISMATCH via sendSurveyError; empty/wildcard channels left to the handler"},
	"presence_query": {precheckCovered, "single NAMESPACE_MISMATCH envelope; empty/wildcard channels left to the handler"},
}

// precheckBehaviorCases builds one inbound message per covered variant,
// carrying the given channel. The behavioral tests iterate this table, so a
// covered variant without a case here (or vice versa) fails the census.
var precheckBehaviorCases = map[string]struct {
	build func(channel string) *clientpb.InboundMessage
	// assertRejection asserts the cross-namespace rejection for this variant;
	// frames[0] is always the NAMESPACE_MISMATCH envelope.
	assertRejection func(t *testing.T, c *Session, frames []*clientpb.OutboundMessage)
}{
	"subscribe": {
		build: func(ch string) *clientpb.InboundMessage {
			return &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{{Channel: ch}}},
			}}
		},
		assertRejection: func(t *testing.T, c *Session, frames []*clientpb.OutboundMessage) {
			// Subscribe keeps the handler's ack semantics for a fully
			// rejected batch: rejection envelope, then the (empty) ack.
			require.Len(t, frames, 2)
			ack := frames[1].GetSubscribeAck()
			require.NotNil(t, ack, "a fully rejected batch is still acked, as the handler always has")
			assert.Empty(t, ack.GetSubscriptions())
			assert.False(t, c.HasSubscription("other:chat"), "cross-namespace channel must not be subscribed")
			assert.Zero(t, c.rt.Hub().NumSubscribers("other:chat"), "cross-namespace channel must not reach the hub")
		},
	},
	"publish": {
		build: func(ch string) *clientpb.InboundMessage {
			return &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_Publish{
				Publish: &clientpb.Publish{Channel: ch, Payload: &sharedv2.Payload{Data: &sharedv2.Payload_Text{Text: "x"}}},
			}}
		},
		assertRejection: requireSingleRejection,
	},
	"unsubscribe": {
		build: func(ch string) *clientpb.InboundMessage {
			return &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_Unsubscribe{
				Unsubscribe: &clientpb.Unsubscribe{Subscriptions: []*clientpb.Subscription{{Channel: ch}}},
			}}
		},
		assertRejection: requireSingleRejection,
	},
	"sub_refresh": {
		build: func(ch string) *clientpb.InboundMessage {
			return &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_SubRefresh{
				SubRefresh: &clientpb.SubRefresh{Channels: []string{ch}},
			}}
		},
		assertRejection: requireSingleRejection,
	},
	"survey_request": {
		build: func(ch string) *clientpb.InboundMessage {
			return &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_SurveyRequest{
				SurveyRequest: &clientpb.SurveyRequest{Channel: ch},
			}}
		},
		assertRejection: func(t *testing.T, c *Session, frames []*clientpb.OutboundMessage) {
			requireSingleRejection(t, c, frames)
			assert.False(t, c.surveyInFlight.Load(), "cross-namespace survey must not start a survey worker")
		},
	},
	"presence_query": {
		build: func(ch string) *clientpb.InboundMessage {
			return &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_PresenceQuery{
				PresenceQuery: &clientpb.PresenceQuery{Channel: ch},
			}}
		},
		assertRejection: requireSingleRejection,
	},
}

// requireSingleRejection asserts the common rejection shape: exactly one
// frame and it is the NAMESPACE_MISMATCH error envelope.
func requireSingleRejection(t *testing.T, _ *Session, frames []*clientpb.OutboundMessage) {
	t.Helper()
	require.Len(t, frames, 1,
		"a cross-namespace request must be answered by exactly one rejection envelope")
	errEnv := frames[0].GetError()
	require.NotNil(t, errEnv, "rejection must be a top-level Error envelope")
	assert.Equal(t, "NAMESPACE_MISMATCH", errEnv.Code)
	assert.Equal(t, "acl_error", errEnv.Type)
	assert.Contains(t, errEnv.Message, "other:chat")
}

// newPrecheckTestClient builds an authenticated session scoped to the
// "acme" namespace with a recording transport.
func newPrecheckTestClient(t *testing.T) (*Session, *mockTransport) {
	t.Helper()
	transport := &mockTransport{}
	c, _, err := NewClient(context.Background(), newFakeRuntime(), transport, JSONMarshaler{})
	require.NoError(t, err)
	c.ForceTestIDs("sess-precheck", "user-1", "client-1")
	c.SetNamespaceForTest("acme")
	return c, transport
}

func decodePrecheckOutbound(t *testing.T, data []byte) *clientpb.OutboundMessage {
	t.Helper()
	var out clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(data, &out))
	return &out
}

// TestSession_DispatchNamespacePrecheck_Census pins the G1 registry against
// the proto definition: every envelope variant must be classified (a new
// channel-carrying message cannot ship without passing the precheck question)
// and the classification must stay consistent with the behavioral table.
func TestSession_DispatchNamespacePrecheck_Census(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	desc := (&clientpb.InboundMessage{}).ProtoReflect().Descriptor()
	oneof := desc.Oneofs().ByName("envelope")
	require.NotNil(oneof, "InboundMessage must keep the envelope oneof the precheck census enumerates")

	fields := oneof.Fields()
	protoNames := make(map[string]bool, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		protoNames[name] = true
		entry, registered := precheckEnvelopeRegistry[name]
		assert.True(registered,
			"new InboundMessage envelope variant %q must be registered in precheckEnvelopeRegistry (covered by or exempt from the namespace precheck) before it ships", name)
		if registered {
			assert.NotEmpty(entry.reason, "registry entry %q must document why it is covered or exempt", name)
			assert.True(entry.class == precheckCovered || entry.class == precheckExempt)
		}
	}
	assert.Len(precheckEnvelopeRegistry, len(protoNames),
		"precheckEnvelopeRegistry must list exactly the proto envelope variants (a variant was renamed or removed)")

	for name, entry := range precheckEnvelopeRegistry {
		_, hasCase := precheckBehaviorCases[name]
		if entry.class == precheckCovered {
			assert.True(hasCase, "covered variant %q must keep a behavioral case in precheckBehaviorCases", name)
		} else {
			assert.False(hasCase, "exempt variant %q must not have a behavioral case", name)
		}
	}
}

// TestSession_DispatchNamespacePrecheck_RejectsCrossNamespace drives every
// covered envelope type through HandleMessage with an out-of-namespace
// channel: each must be rejected at the dispatch entry with a top-level
// NAMESPACE_MISMATCH error envelope, and no handler work may happen for the
// violating channel (no subscription, no survey worker, no non-error acks).
func TestSession_DispatchNamespacePrecheck_RejectsCrossNamespace(t *testing.T) {
	for name, tc := range precheckBehaviorCases {
		t.Run(name, func(t *testing.T) {
			c, transport := newPrecheckTestClient(t)
			require.NoError(t, c.HandleMessage(context.Background(), tc.build("other:chat")))

			frames := make([]*clientpb.OutboundMessage, transport.getMessageCount())
			for i := range frames {
				frames[i] = decodePrecheckOutbound(t, transport.getMessage(i))
			}
			tc.assertRejection(t, c, frames)
		})
	}
}

// TestSession_DispatchNamespacePrecheck_CrossNamespaceWildcard pins the
// wildcard rule for the two types the precheck protects without a handler
// backstop: a wildcard pattern still names foreign channels, so
// unsubscribe/sub_refresh with "other:*" are rejected like exact channels.
func TestSession_DispatchNamespacePrecheck_CrossNamespaceWildcard(t *testing.T) {
	for _, name := range []string{"unsubscribe", "sub_refresh"} {
		t.Run(name, func(t *testing.T) {
			c, transport := newPrecheckTestClient(t)
			require.NoError(t, c.HandleMessage(context.Background(), precheckBehaviorCases[name].build("other:*")))

			require.Equal(t, 1, transport.getMessageCount())
			errEnv := decodePrecheckOutbound(t, transport.getMessage(0)).GetError()
			require.NotNil(t, errEnv)
			assert.Equal(t, "NAMESPACE_MISMATCH", errEnv.Code)
		})
	}
}

// TestSession_DispatchNamespacePrecheck_SameNamespaceReachesHandler drives
// every covered envelope type with an in-namespace channel: the precheck must
// pass the request through untouched and the handler's own reply must come
// back, proving the gate does not over-reject.
func TestSession_DispatchNamespacePrecheck_SameNamespaceReachesHandler(t *testing.T) {
	servedAssertions := map[string]func(t *testing.T, c *Session, out *clientpb.OutboundMessage){
		"subscribe": func(t *testing.T, c *Session, out *clientpb.OutboundMessage) {
			assert.NotNil(t, out.GetSubscribeAck(), "in-namespace subscribe must reach the handler and be acked")
			assert.True(t, c.HasSubscription("acme:chat"), "in-namespace subscription must be registered")
		},
		"publish": func(t *testing.T, _ *Session, out *clientpb.OutboundMessage) {
			assert.NotNil(t, out.GetPublishAck(), "in-namespace publish must reach the broker path and be acked")
		},
		"unsubscribe": func(t *testing.T, _ *Session, out *clientpb.OutboundMessage) {
			ack := out.GetUnsubscribeAck()
			require.NotNil(t, ack, "in-namespace unsubscribe must reach the handler and be acked")
			require.Len(t, ack.GetSubscriptions(), 1)
			assert.Equal(t, "acme:chat", ack.GetSubscriptions()[0].GetChannel())
		},
		"sub_refresh": func(t *testing.T, _ *Session, out *clientpb.OutboundMessage) {
			assert.NotNil(t, out.GetSubRefreshAck(), "in-namespace sub refresh must reach the handler and be acked")
		},
		// Neither session below is subscribed to the channel, so the handlers'
		// own coverage gate rejects it — a handler-emitted error proves the
		// request passed the precheck AND the handler's namespace check into
		// the handler logic behind them.
		"survey_request": func(t *testing.T, _ *Session, out *clientpb.OutboundMessage) {
			errEnv := out.GetError()
			require.NotNil(t, errEnv, "uncovered survey must be rejected by the handler's coverage gate")
			assert.Equal(t, "PERMISSION_DENIED", errEnv.Code)
			assert.Contains(t, errEnv.Message, "not covered by session")
		},
		"presence_query": func(t *testing.T, _ *Session, out *clientpb.OutboundMessage) {
			errEnv := out.GetError()
			require.NotNil(t, errEnv, "uncovered presence query must be rejected by the handler's coverage gate")
			assert.Equal(t, "PERMISSION_DENIED", errEnv.Code)
			assert.Contains(t, errEnv.Message, "not covered by session")
		},
	}

	for name, tc := range precheckBehaviorCases {
		t.Run(name, func(t *testing.T) {
			c, transport := newPrecheckTestClient(t)
			require.NoError(t, c.HandleMessage(context.Background(), tc.build("acme:chat")))

			require.Equal(t, 1, transport.getMessageCount(),
				"an in-namespace request must produce exactly the handler's reply")
			out := decodePrecheckOutbound(t, transport.getMessage(0))
			servedAssertions[name](t, c, out)
		})
	}
}

// TestSession_DispatchNamespacePrecheck_SubscribeMixedBatch pins the Subscribe
// filter semantics: the violating channel gets the same per-channel error
// envelope checkSubscribeACL step 0 always sent, the in-namespace channels are
// still subscribed, and the ack lists only them.
func TestSession_DispatchNamespacePrecheck_SubscribeMixedBatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	c, transport := newPrecheckTestClient(t)
	in := &clientpb.InboundMessage{Id: "m1", Envelope: &clientpb.InboundMessage_Subscribe{
		Subscribe: &clientpb.Subscribe{Subscriptions: []*clientpb.Subscription{
			{Channel: "acme:chat"},
			{Channel: "other:chat"},
		}},
	}}
	require.NoError(c.HandleMessage(context.Background(), in))

	require.Equal(2, transport.getMessageCount())
	errEnv := decodePrecheckOutbound(t, transport.getMessage(0)).GetError()
	require.NotNil(errEnv, "the violating channel must produce the per-channel error envelope")
	assert.Equal("NAMESPACE_MISMATCH", errEnv.Code)
	assert.Contains(errEnv.Message, "other:chat")

	ack := decodePrecheckOutbound(t, transport.getMessage(1)).GetSubscribeAck()
	require.NotNil(ack, "the in-namespace remainder must still be acked")
	require.Len(ack.GetSubscriptions(), 1)
	assert.Equal("acme:chat", ack.GetSubscriptions()[0].GetChannel())
	assert.True(c.HasSubscription("acme:chat"))
	assert.False(c.HasSubscription("other:chat"))
}

// TestSession_DispatchNamespacePrecheck_PreservesHandlerPrecedence pins the
// client-visible behavior the precheck must not change: where a handler
// checks emptiness/wildcards before its namespace scope, the precheck passes
// those channels through so the handler's BAD_REQUEST still wins, and the
// channel-less/tolerance edges keep their exact existing replies.
func TestSession_DispatchNamespacePrecheck_PreservesHandlerPrecedence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	send := func(t *testing.T, in *clientpb.InboundMessage) (*Session, []*clientpb.OutboundMessage) {
		t.Helper()
		c, transport := newPrecheckTestClient(t)
		require.NoError(c.HandleMessage(context.Background(), in))
		frames := make([]*clientpb.OutboundMessage, transport.getMessageCount())
		for i := range frames {
			frames[i] = decodePrecheckOutbound(t, transport.getMessage(i))
		}
		return c, frames
	}

	t.Run("publish empty channel keeps handler BAD_REQUEST", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["publish"].build(""))
		require.Len(frames, 1)
		assert.Equal("BAD_REQUEST", frames[0].GetError().GetCode())
	})
	t.Run("publish wildcard keeps handler BAD_REQUEST ahead of namespace", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["publish"].build("other:*"))
		require.Len(frames, 1)
		assert.Equal("BAD_REQUEST", frames[0].GetError().GetCode(),
			"the handler's wildcard rejection precedes the namespace check today; the precheck must not reorder it")
	})
	t.Run("survey empty channel keeps handler BAD_REQUEST", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["survey_request"].build(""))
		require.Len(frames, 1)
		assert.Equal("BAD_REQUEST", frames[0].GetError().GetCode())
	})
	t.Run("survey wildcard keeps handler BAD_REQUEST", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["survey_request"].build("acme:*"))
		require.Len(frames, 1)
		assert.Equal("BAD_REQUEST", frames[0].GetError().GetCode())
	})
	t.Run("presence empty channel keeps handler BAD_REQUEST", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["presence_query"].build(""))
		require.Len(frames, 1)
		assert.Equal("BAD_REQUEST", frames[0].GetError().GetCode())
	})
	t.Run("subscribe empty channel keeps the handler's NAMESPACE_MISMATCH plus ack", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["subscribe"].build(""))
		require.Len(frames, 2)
		assert.Equal("NAMESPACE_MISMATCH", frames[0].GetError().GetCode())
		assert.NotNil(frames[1].GetSubscribeAck(), "the handler still acks the request with the violating channel dropped")
	})
	t.Run("unsubscribe empty channel still acked", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["unsubscribe"].build(""))
		require.Len(frames, 1)
		assert.NotNil(frames[0].GetUnsubscribeAck())
	})
	t.Run("sub_refresh empty channel still acked", func(t *testing.T) {
		_, frames := send(t, precheckBehaviorCases["sub_refresh"].build(""))
		require.Len(frames, 1)
		assert.NotNil(frames[0].GetSubRefreshAck())
	})
}
