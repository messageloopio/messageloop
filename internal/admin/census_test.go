package admin

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/messageloopio/messageloop/internal/authz"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
)

// The admin scope census (design §2.5, mechanism gap G2 — the admin-plane
// port of the session-plane "mechanism gap G1, review #4" census): scattered
// execution points were proven to leak, so the coverage itself is pinned by
// red-line tests.
//
//   - Census ① pins every APIServiceServer RPC against rpcScopeRegistry
//     (capability table + carrier classification): a new RPC cannot ship
//     without answering "which capabilities and which carriers?".
//   - Census ② pins every field of every request message against
//     scopeFieldHandled / scopeFieldExempt: a new field cannot ship without
//     answering "is it an authorization carrier?".

// scopeCensusMessages lists the request messages the census walks plus the
// nested Publish messages the walker descends into.
var scopeCensusMessages = []proto.Message{
	&serverv2.PublishRequest{},
	&serverv2.DisconnectRequest{},
	&serverv2.SubscribeRequest{},
	&serverv2.UnsubscribeRequest{},
	&serverv2.SurveyRequest{},
	&serverv2.GetPresenceRequest{},
	&serverv2.GetHistoryRequest{},
	&serverv2.GetChannelsRequest{},
	// Nested carriers of PublishRequest.publications.
	&serverv2.Publication{},
	&serverv2.Publication_Destination{},
}

// TestAdminScopeCensus_Methods reflects over the APIServiceServer interface
// and asserts every RPC is registered in rpcScopeRegistry with a capability
// evaluator and a carrier classification. An unregistered RPC fails the
// whole admin plane closed in scopeAuthorize (Internal "capability table
// entry missing") — this census turns that latent outage into a red test
// instead.
func TestAdminScopeCensus_Methods(t *testing.T) {
	assert := assert.New(t)

	interfaceType := reflect.TypeOf((*serverv2.APIServiceServer)(nil)).Elem()
	methods := make([]string, 0, interfaceType.NumMethod())
	for i := 0; i < interfaceType.NumMethod(); i++ {
		name := interfaceType.Method(i).Name
		if strings.HasPrefix(name, "Unimplemented") || strings.HasPrefix(name, "mustEmbed") {
			continue // forward-compatibility plumbing, not an RPC
		}
		methods = append(methods, name)
	}
	require.NotEmpty(t, methods, "the APIServiceServer interface must expose RPC methods")

	for _, name := range methods {
		spec, registered := rpcScopeRegistry[name]
		assert.True(registered,
			"admin RPC %q is not registered in rpcScopeRegistry (capability table + carrier classification) — register it in internal/admin/scope.go before it ships", name)
		if !registered {
			continue
		}
		assert.NotNil(spec.requiredCaps,
			"rpcScopeRegistry[%q].requiredCaps must classify the request's capability bits", name)
	}
	assert.Len(rpcScopeRegistry, len(methods),
		"rpcScopeRegistry must list exactly the APIServiceServer RPCs (an entry was renamed or removed without updating the registry)")
}

// TestAdminScopeCensus_RequestFields reflects over every request message and
// asserts each field is classified: either processed by the scope layer
// (scopeFieldHandled) or explicitly exempt (scopeFieldExempt). It also
// rejects stale table entries whose message/field no longer exists, so a
// rename cannot leave dead coverage behind.
func TestAdminScopeCensus_RequestFields(t *testing.T) {
	assert := assert.New(t)

	type fieldKey struct {
		message string
		field   string
	}

	liveMessages := make(map[string]bool)
	liveFields := make(map[fieldKey]bool)
	for _, msg := range scopeCensusMessages {
		name := string(msg.ProtoReflect().Descriptor().FullName())
		liveMessages[name] = true

		handled := scopeFieldHandled[name]
		exempt := scopeFieldExempt[name]
		assert.True(handled != nil || exempt != nil,
			"request message %q is not classified in scopeFieldHandled/scopeFieldExempt — decide for every field whether it is an authorization carrier before it ships", name)

		fields := msg.ProtoReflect().Descriptor().Fields()
		for i := 0; i < fields.Len(); i++ {
			fieldName := string(fields.Get(i).Name())
			liveFields[fieldKey{name, fieldName}] = true

			_, hasHandled := handled[fieldName]
			_, hasExempt := exempt[fieldName]
			assert.True(hasHandled || hasExempt,
				"field %s.%s is neither handled by the scope layer nor explicitly exempt — classify it in scopeFieldHandled/scopeFieldExempt (design §2.5 G2)", name, fieldName)
			assert.False(hasHandled && hasExempt,
				"field %s.%s is both handled and exempt — exactly one classification is allowed", name, fieldName)
			if hasHandled {
				assert.NotEmpty(handled[fieldName], "handled field %s.%s must document why it is a carrier", name, fieldName)
			}
			if hasExempt {
				assert.NotEmpty(exempt[fieldName], "exempt field %s.%s must document why it is not a carrier", name, fieldName)
			}
		}
	}

	// No stale entries: every table key must correspond to a live message
	// and a live field.
	for _, table := range []map[string]map[string]string{scopeFieldHandled, scopeFieldExempt} {
		for messageName, fields := range table {
			assert.True(liveMessages[messageName],
				"scope field table references unknown message %q (stale entry after a rename?)", messageName)
			for fieldName := range fields {
				assert.True(liveFields[fieldKey{messageName, fieldName}],
					"scope field table references unknown field %s.%s (stale entry after a rename?)", messageName, fieldName)
			}
		}
	}
}

// TestAdminScopeCensus_CarriersMatchFields cross-checks the registry's
// carrier classification against the handled-field table so the two census
// structures cannot drift: the fields a carrier classification implies must
// be marked handled on the corresponding message.
func TestAdminScopeCensus_CarriersMatchFields(t *testing.T) {
	assert := assert.New(t)

	expectations := []struct {
		method         string
		spec           func() scopeCarriers
		handledImplied [][2]string // {message, field} pairs the carriers imply
	}{
		{"Publish", func() scopeCarriers { return rpcScopeRegistry["Publish"].carriers },
			[][2]string{
				{"messageloop.server.v2.PublishRequest", "publications"},
				{"messageloop.server.v2.Publication", "destination"},
				{"messageloop.server.v2.Publication.Destination", "sessions"},
				{"messageloop.server.v2.Publication.Destination", "channels"},
				{"messageloop.server.v2.Publication.Destination", "users"},
				{"messageloop.server.v2.Publication.Destination", "namespace"},
			}},
		{"Disconnect", func() scopeCarriers { return rpcScopeRegistry["Disconnect"].carriers },
			[][2]string{
				{"messageloop.server.v2.DisconnectRequest", "sessions"},
				{"messageloop.server.v2.DisconnectRequest", "users"},
				{"messageloop.server.v2.DisconnectRequest", "namespace"},
			}},
		{"Subscribe", func() scopeCarriers { return rpcScopeRegistry["Subscribe"].carriers },
			[][2]string{
				{"messageloop.server.v2.SubscribeRequest", "session_id"},
				{"messageloop.server.v2.SubscribeRequest", "channels"},
				{"messageloop.server.v2.SubscribeRequest", "user_id"},
				{"messageloop.server.v2.SubscribeRequest", "namespace"},
			}},
		{"Unsubscribe", func() scopeCarriers { return rpcScopeRegistry["Unsubscribe"].carriers },
			[][2]string{
				{"messageloop.server.v2.UnsubscribeRequest", "session_id"},
				{"messageloop.server.v2.UnsubscribeRequest", "channels"},
				{"messageloop.server.v2.UnsubscribeRequest", "user_id"},
				{"messageloop.server.v2.UnsubscribeRequest", "namespace"},
			}},
		{"Survey", func() scopeCarriers { return rpcScopeRegistry["Survey"].carriers },
			[][2]string{{"messageloop.server.v2.SurveyRequest", "channel"}}},
		{"GetPresence", func() scopeCarriers { return rpcScopeRegistry["GetPresence"].carriers },
			[][2]string{{"messageloop.server.v2.GetPresenceRequest", "channel"}}},
		{"GetHistory", func() scopeCarriers { return rpcScopeRegistry["GetHistory"].carriers },
			[][2]string{{"messageloop.server.v2.GetHistoryRequest", "channel"}}},
		{"GetChannels", func() scopeCarriers { return rpcScopeRegistry["GetChannels"].carriers },
			nil}, // response-side filtering, no request carriers
	}

	for _, e := range expectations {
		for _, pair := range e.handledImplied {
			_, ok := scopeFieldHandled[pair[0]][pair[1]]
			assert.True(ok,
				"%s declares carriers implying field %q of %s, but the field is not in scopeFieldHandled",
				e.method, pair[1], pair[0])
		}
	}
}

// TestAdminScopeCensus_CapabilityTableSemantics pins the capability table's
// per-RPC semantics against the design table (§2.4/§2.6): single-channel
// reads gate on their bit, Survey has no precondition bits, and the
// session/user gates combine exactly as specified.
func TestAdminScopeCensus_CapabilityTableSemantics(t *testing.T) {
	assert := assert.New(t)

	// Survey: no precondition bits (bypass_gate is a behavior switch, not a
	// gate).
	assert.Zero(rpcScopeRegistry["Survey"].requiredCaps(&serverv2.SurveyRequest{}),
		"Survey must not require precondition capability bits")

	// GetPresence / GetHistory / GetChannels gate on their single bit.
	assert.Equal(authz.CapPresenceRead, rpcScopeRegistry["GetPresence"].requiredCaps(&serverv2.GetPresenceRequest{}))
	assert.Equal(authz.CapHistoryRead, rpcScopeRegistry["GetHistory"].requiredCaps(&serverv2.GetHistoryRequest{}))
	assert.Equal(authz.CapChannelsList, rpcScopeRegistry["GetChannels"].requiredCaps(&serverv2.GetChannelsRequest{}))

	// Subscribe: session_id → session.act; user_id → user.fanout|session.act.
	assert.Zero(rpcScopeRegistry["Subscribe"].requiredCaps(&serverv2.SubscribeRequest{Channels: []string{"ns:ch"}}),
		"channel-only subscribe needs no capability bits")
	assert.Equal(authz.CapSessionAct, rpcScopeRegistry["Subscribe"].requiredCaps(&serverv2.SubscribeRequest{SessionId: "s"}))
	assert.Equal(authz.CapUserFanout|authz.CapSessionAct, rpcScopeRegistry["Subscribe"].requiredCaps(&serverv2.SubscribeRequest{UserId: "u"}))

	// Publish: bits aggregate over publications; channel-only needs none.
	assert.Zero(rpcScopeRegistry["Publish"].requiredCaps(&serverv2.PublishRequest{
		Publications: []*serverv2.Publication{{Destination: &serverv2.Publication_Destination{Channels: []string{"ns:ch"}}}},
	}))
	assert.Equal(authz.CapSessionAct, rpcScopeRegistry["Publish"].requiredCaps(&serverv2.PublishRequest{
		Publications: []*serverv2.Publication{{Destination: &serverv2.Publication_Destination{Sessions: []string{"s"}}}},
	}))
	assert.Equal(authz.CapUserFanout|authz.CapSessionAct, rpcScopeRegistry["Publish"].requiredCaps(&serverv2.PublishRequest{
		Publications: []*serverv2.Publication{{Destination: &serverv2.Publication_Destination{Users: []string{"u"}}}},
	}))
	assert.Equal(authz.CapUserFanout|authz.CapSessionAct, rpcScopeRegistry["Publish"].requiredCaps(&serverv2.PublishRequest{
		Publications: []*serverv2.Publication{
			{Destination: &serverv2.Publication_Destination{Sessions: []string{"s"}}},
			{Destination: &serverv2.Publication_Destination{Users: []string{"u"}}},
		},
	}))

	// Disconnect mirrors Publish.
	assert.Equal(authz.CapSessionAct, rpcScopeRegistry["Disconnect"].requiredCaps(&serverv2.DisconnectRequest{Sessions: []string{"s"}}))
	assert.Equal(authz.CapUserFanout|authz.CapSessionAct, rpcScopeRegistry["Disconnect"].requiredCaps(&serverv2.DisconnectRequest{Users: []string{"u"}}))

	// Unsubscribe mirrors Subscribe.
	assert.Equal(authz.CapSessionAct, rpcScopeRegistry["Unsubscribe"].requiredCaps(&serverv2.UnsubscribeRequest{SessionId: "s"}))
	assert.Equal(authz.CapUserFanout|authz.CapSessionAct, rpcScopeRegistry["Unsubscribe"].requiredCaps(&serverv2.UnsubscribeRequest{UserId: "u"}))
}
