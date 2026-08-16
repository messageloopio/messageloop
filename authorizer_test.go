package messageloop

import (
	"context"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAuthorizer(t *testing.T, cfg config.AuthorizerConfig) *Authorizer {
	t.Helper()
	a, err := NewAuthorizer(cfg)
	require.NoError(t, err)
	return a
}

func userPrincipal(userID string) Principal {
	return Principal{Kind: PrincipalUser, UserID: userID}
}

func adminPrincipalWith(caps Capability) Principal {
	return Principal{Kind: PrincipalAdmin, UserID: "admin", Caps: caps}
}

func authzBoolPtr(v bool) *bool { return &v }

func denyAllRule(pattern string) config.AuthorizerRule {
	return config.AuthorizerRule{Pattern: pattern, DenyAll: true}
}

// TestAuthorizer_LanguageInclusionTable locks the §5.2 intersection table:
// the channel column is the subscription key p, the deny column is one
// deny_all rule.
func TestAuthorizer_LanguageInclusionTable(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		deny    string
		want    bool
		reason  string
	}{
		{"disjoint dstar", "im.**", "secret.**", true, "default"},
		{"bare dstar not routable", "**", "secret.**", false, "not_routable"},
		{"star covers exact", "im.*", "im.secret", false, "language"},
		{"dstar subsumes star", "im.room.*", "im.**", false, "language"},
		{"default allow", "chat.**", "", true, "default"},
		{"middle star not routable", "*.room", "", false, "not_routable"},
		{"dstar covers exact", "im.**", "im.secret", false, "language"},
		{"deep dstar disjoint from short star", "im.room.a.**", "im.*", true, "default"},
		{"dstar intersects star at exact name", "im.room.**", "im.*", false, "language"},
		{"exact deeper than star", "a.b.c", "a.*", true, "default"},
		{"exact at star depth", "a.b", "a.*", false, "language"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.AuthorizerConfig{}
			if tt.deny != "" {
				cfg.Rules = []config.AuthorizerRule{denyAllRule(tt.deny)}
			}
			a := newTestAuthorizer(t, cfg)
			dec := a.Decide(userPrincipal("user-1"), ActionSubscribePattern, tt.pattern)
			assert.Equal(t, tt.want, dec.Allow, "Decide(%q)", tt.pattern)
			if tt.reason != "" {
				assert.Equal(t, tt.reason, dec.Reason, "Decide(%q).Reason", tt.pattern)
			}
		})
	}
}

// TestAuthorizer_DenyNotPunchable verifies a deny cannot be opened by a more
// specific allow: secret.** DenyAll + secret.lobby allow alice still denies
// alice on secret.lobby, while disjoint subscriptions stay allowed.
func TestAuthorizer_DenyNotPunchable(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			denyAllRule("secret.**"),
			{Pattern: "secret.lobby", AllowSubscribe: []string{"alice"}},
		},
	})
	dec := a.Decide(userPrincipal("alice"), ActionSubscribePattern, "secret.lobby")
	assert.False(t, dec.Allow, "a more specific allow must not punch a hole in deny_all")
	assert.Equal(t, "language", dec.Reason)

	dec = a.Decide(userPrincipal("alice"), ActionSubscribePattern, "im.**")
	assert.True(t, dec.Allow, "disjoint patterns stay allowed")

	// Publish is denied on the deny_all pattern too, and allowed elsewhere.
	assert.False(t, a.Decide(userPrincipal("alice"), ActionPublish, "secret.lobby").Allow)
	assert.True(t, a.Decide(userPrincipal("alice"), ActionPublish, "im.room.1").Allow)
}

// TestAuthorizer_OmittedVsEmptyList verifies §5.3: a rule with only Effects
// (allow_subscribe omitted) does not constrain subscribe, while an explicit
// empty list denies it.
func TestAuthorizer_OmittedVsEmptyList(t *testing.T) {
	omitted := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: authzBoolPtr(true)}},
		},
	})
	assert.True(t, omitted.Decide(userPrincipal("anyone"), ActionSubscribePattern, "game.tick.1").Allow,
		"an effects-only rule must not reject subscribe")
	assert.True(t, omitted.Decide(userPrincipal("anyone"), ActionSubscribePattern, "game.tick.**").Allow)

	empty := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "private.*", AllowSubscribe: []string{}},
		},
	})
	dec := empty.Decide(userPrincipal("alice"), ActionSubscribePattern, "private.room")
	assert.False(t, dec.Allow, "an explicit empty allow list denies everyone")
	assert.Equal(t, "language", dec.Reason)
}

// TestAuthorizer_AllowListMatching verifies the allow list matching for
// subscribe and publish: "*" and the exact user ID allow; other users deny.
func TestAuthorizer_AllowListMatching(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "chat.public.*", AllowSubscribe: []string{"*"}},
			{Pattern: "chat.private.*", AllowSubscribe: []string{"alice"}},
			{Pattern: "readonly.*", AllowPublish: []string{"admin"}},
		},
	})
	assert.True(t, a.Decide(userPrincipal("bob"), ActionSubscribePattern, "chat.public.1").Allow)
	assert.True(t, a.Decide(userPrincipal("alice"), ActionSubscribePattern, "chat.private.1").Allow)
	assert.False(t, a.Decide(userPrincipal("bob"), ActionSubscribePattern, "chat.private.1").Allow)
	assert.True(t, a.Decide(userPrincipal("admin"), ActionPublish, "readonly.data").Allow)
	assert.False(t, a.Decide(userPrincipal("alice"), ActionPublish, "readonly.data").Allow)

	// The admin principal matches allow lists by its "admin" user ID.
	admin := adminPrincipalWith(0)
	assert.True(t, a.Decide(admin, ActionPublish, "readonly.data").Allow)
}

// TestAuthorizer_SurveyDefaultDeny verifies §5.4 Survey: default deny; an
// allow_survey ["*"] plus Effects.survey=true opens it; deny_all wins.
func TestAuthorizer_SurveyDefaultDeny(t *testing.T) {
	closed := newTestAuthorizer(t, config.AuthorizerConfig{})
	dec := closed.Decide(userPrincipal("user-1"), ActionSurvey, "any.ch")
	assert.False(t, dec.Allow, "survey defaults to deny")
	assert.Equal(t, "default", dec.Reason)

	effectsOnly := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "csurvey.**", ChannelPolicySpec: config.ChannelPolicySpec{Survey: authzBoolPtr(true)}},
		},
	})
	assert.False(t, effectsOnly.Decide(userPrincipal("user-1"), ActionSurvey, "csurvey.1").Allow,
		"Effects.survey alone must not open survey without allow_survey")

	open := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "csurvey.**", AllowSurvey: []string{"*"}, ChannelPolicySpec: config.ChannelPolicySpec{Survey: authzBoolPtr(true)}},
		},
	})
	assert.True(t, open.Decide(userPrincipal("user-1"), ActionSurvey, "csurvey.1").Allow)

	gated := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "csurvey.**", AllowSurvey: []string{"alice"}, ChannelPolicySpec: config.ChannelPolicySpec{Survey: authzBoolPtr(true)}},
		},
	})
	assert.True(t, gated.Decide(userPrincipal("alice"), ActionSurvey, "csurvey.1").Allow)
	assert.False(t, gated.Decide(userPrincipal("bob"), ActionSurvey, "csurvey.1").Allow)

	denied := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{
				Pattern:           "csurvey.**",
				DenyAll:           true,
				AllowSurvey:       []string{"*"},
				ChannelPolicySpec: config.ChannelPolicySpec{Survey: authzBoolPtr(true)},
			},
		},
	})
	dec = denied.Decide(userPrincipal("user-1"), ActionSurvey, "csurvey.1")
	assert.False(t, dec.Allow, "deny_all must win over allow_survey")
	assert.Equal(t, "deny_all", dec.Reason)
}

// TestAuthorizer_PublishNoCoverage verifies KD-K21 at the Decide level: an
// unsubscribed principal may still publish.
func TestAuthorizer_PublishNoCoverage(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{denyAllRule("secret.**")},
	})
	assert.True(t, a.Decide(userPrincipal("user-1"), ActionPublish, "open.ch").Allow)
	assert.False(t, a.Decide(userPrincipal("user-1"), ActionPublish, "secret.ch").Allow)
}

// TestAuthorizer_RecoverAndPresence verifies §5.4 Recover / Presence:
// default follows Effects, deny_all wins, wildcard channels are skipped.
func TestAuthorizer_RecoverAndPresence(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			denyAllRule("secret.**"),
			{Pattern: "nopres.**", ChannelPolicySpec: config.ChannelPolicySpec{Presence: authzBoolPtr(false)}},
			{Pattern: "norecover.**", ChannelPolicySpec: config.ChannelPolicySpec{Recover: authzBoolPtr(false)}},
		},
	})
	u := userPrincipal("user-1")
	assert.True(t, a.Decide(u, ActionRecover, "im.room.1").Allow)
	assert.True(t, a.Decide(u, ActionPresence, "im.room.1").Allow)
	assert.False(t, a.Decide(u, ActionRecover, "secret.1").Allow)
	assert.Equal(t, "deny_all", a.Decide(u, ActionRecover, "secret.1").Reason)
	assert.False(t, a.Decide(u, ActionPresence, "secret.1").Allow)
	assert.False(t, a.Decide(u, ActionPresence, "nopres.1").Allow)
	assert.Equal(t, "effects", a.Decide(u, ActionPresence, "nopres.1").Reason)
	assert.False(t, a.Decide(u, ActionRecover, "norecover.1").Allow)
	// Wildcard channels are never recoverable / presentable.
	assert.False(t, a.Decide(u, ActionRecover, "im.**").Allow)
	assert.False(t, a.Decide(u, ActionPresence, "im.*").Allow)
}

// TestAuthorizer_EffectsOverlay verifies §9.10: game.** history=true first,
// game.tick.** transient_only later → game.tick.1 is transient with
// Recover=false. Unlike the old first-match policy, generic rules before
// specific rules is the correct order.
func TestAuthorizer_EffectsOverlay(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "game.**", ChannelPolicySpec: config.ChannelPolicySpec{
				History: authzBoolPtr(true),
				Survey:  authzBoolPtr(true),
			}},
			{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: authzBoolPtr(true)}},
		},
	})
	tick := a.Effects("game.tick.fps")
	assert.True(t, tick.TransientOnly)
	assert.False(t, tick.History, "transient_only must force History=false")
	assert.False(t, tick.Recover, "transient_only must force Recover=false")
	// The generic rule still applied to other channels.
	room := a.Effects("game.room.1")
	assert.False(t, room.TransientOnly)
	assert.True(t, room.History)
	assert.True(t, room.Survey)
	other := a.Effects("im.room.1")
	assert.False(t, other.TransientOnly)
	assert.True(t, other.History)
	assert.False(t, other.Survey)
}

// TestAuthorizer_DefaultSpecOverlay verifies server.authorizer.default acts
// as the base overlay for every channel.
func TestAuthorizer_DefaultSpecOverlay(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Default: config.ChannelPolicySpec{History: authzBoolPtr(false), Survey: authzBoolPtr(true)},
	})
	pol := a.Effects("unmatched.channel")
	assert.False(t, pol.History)
	assert.True(t, pol.Survey)
}

// TestAuthorizer_ZeroConfig verifies the zero AuthorizerConfig: no rules,
// default effects, subscribe/publish open, survey off.
func TestAuthorizer_ZeroConfig(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{})
	u := userPrincipal("user-1")
	assert.True(t, a.Decide(u, ActionSubscribePattern, "chat.**").Allow)
	assert.True(t, a.Decide(u, ActionPublish, "chat.room.1").Allow)
	assert.False(t, a.Decide(u, ActionSurvey, "chat.room.1").Allow)
	assert.True(t, a.Decide(u, ActionRecover, "chat.room.1").Allow)
	assert.True(t, a.Decide(u, ActionPresence, "chat.room.1").Allow)
	pol := a.Effects("any.channel")
	assert.True(t, pol.History)
	assert.True(t, pol.Presence)
	assert.True(t, pol.Recover)
	assert.False(t, pol.Survey)
	assert.Equal(t, 256, pol.MaxSurveySubscribers)
	assert.Equal(t, 5*time.Second, pol.MaxSurveyTimeout)
	assert.Equal(t, 256, pol.PresenceSnapshotLimit)
}

// TestAuthorizer_InvalidRulePatterns verifies §5.1 rule patterns: middle
// "**", non-final wildcards, and bare "*" / "**" are rejected.
func TestAuthorizer_InvalidRulePatterns(t *testing.T) {
	for _, pattern := range []string{"", "a.**.b", "*.room", "im.*.tick", "*", "**", "a.b*", "a..b", "a.b."} {
		t.Run(pattern, func(t *testing.T) {
			_, err := NewAuthorizer(config.AuthorizerConfig{
				Rules: []config.AuthorizerRule{{Pattern: pattern}},
			})
			assert.Error(t, err, "pattern %q must be rejected", pattern)
		})
	}
}

// TestAuthorizer_AdminSubscribe verifies the admin subscribe path: without
// subscribe.any the admin must appear in allow lists (as "admin"); with the
// bit the allow list is skipped; bare "*" / "**" always fail.
func TestAuthorizer_AdminSubscribe(t *testing.T) {
	a := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "private.*", AllowSubscribe: []string{"alice", "admin"}},
		},
	})
	adminNoCaps := adminPrincipalWith(0)
	assert.True(t, a.Decide(adminNoCaps, ActionSubscribePattern, "private.room").Allow)
	adminWithAny := adminPrincipalWith(CapSubscribeAny)
	assert.True(t, a.Decide(adminWithAny, ActionSubscribePattern, "private.room").Allow)

	locked := newTestAuthorizer(t, config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "private.*", AllowSubscribe: []string{"alice"}},
		},
	})
	assert.False(t, locked.Decide(adminNoCaps, ActionSubscribePattern, "private.room").Allow)
	// subscribe.any skips the static allow list (Node.AdminCanSubscribe
	// encodes this; Decide itself still consults the list).
	assert.False(t, locked.Decide(adminWithAny, ActionSubscribePattern, "private.room").Allow)

	// pattern.global does not unlock bare "**" (A3 / KD-K13 stay).
	global := adminPrincipalWith(CapSubscribeAny | CapPatternGlobal)
	dec := a.Decide(global, ActionSubscribePattern, "**")
	assert.False(t, dec.Allow)
	assert.Equal(t, "not_routable", dec.Reason)
	dec = a.Decide(global, ActionSubscribePattern, "*")
	assert.False(t, dec.Allow)
	assert.Equal(t, "not_routable", dec.Reason)
}

// TestDefaultAdminCapabilities verifies the default bits: every closed bit
// except CapPatternGlobal.
func TestDefaultAdminCapabilities(t *testing.T) {
	expected := Capability(0)
	for name, bit := range ClosedCapabilityNames {
		assert.NotZero(t, bit, "capability %q must have a bit set", name)
		if name != "pattern.global" {
			expected |= bit
		}
	}
	assert.Equal(t, expected, DefaultAdminCapabilities)
	assert.NotZero(t, DefaultAdminCapabilities&CapHistoryRead)
	assert.NotZero(t, DefaultAdminCapabilities&CapPresenceRead)
	assert.Zero(t, DefaultAdminCapabilities&CapPatternGlobal)
}

// TestNode_AdminCapabilitiesConfig verifies the capabilities wiring:
// omitted → DefaultAdminCapabilities; explicit [] → zero bits; explicit list
// → only those bits.
func TestNode_AdminCapabilitiesConfig(t *testing.T) {
	omitted := NewNode(nil)
	assert.Equal(t, DefaultAdminCapabilities, omitted.AdminCapabilities())

	empty := NewNode(&config.Server{GRPCAdmin: config.GRPCAdmin{Capabilities: []string{}}})
	assert.Zero(t, empty.AdminCapabilities(), "an explicit empty list locks the admin data plane")

	partial := NewNode(&config.Server{GRPCAdmin: config.GRPCAdmin{
		Capabilities: []string{"history.read", "channels.list"},
	}})
	assert.Equal(t, CapHistoryRead|CapChannelsList, partial.AdminCapabilities())
}

// TestNode_AdminCanSubscribeAndPublish verifies §8.4 through the Node.
func TestNode_AdminCanSubscribeAndPublish(t *testing.T) {
	// Explicit capabilities without subscribe.any: the admin must appear in
	// allow lists like any user.
	node := NewNode(&config.Server{
		GRPCAdmin: config.GRPCAdmin{Capabilities: []string{"history.read"}},
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", AllowSubscribe: []string{"alice"}},
				denyAllRule("secret.**"),
			},
		},
	})
	// Without subscribe.any the admin cannot subscribe to an allow-listed
	// channel it is not on.
	assert.False(t, node.AdminCanSubscribe("private.room"))
	// Pattern.compile failures fail admin subscribe too.
	assert.False(t, node.AdminCanSubscribe("**"))
	assert.False(t, node.AdminCanSubscribe("*.room"))
	// deny_all blocks admin publish.
	assert.False(t, node.AdminCanPublish("secret.1"))
	assert.True(t, node.AdminCanPublish("private.room"))

	// With subscribe.any the static allow list is skipped.
	anyNode := NewNode(&config.Server{
		GRPCAdmin: config.GRPCAdmin{Capabilities: []string{"subscribe.any"}},
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", AllowSubscribe: []string{"alice"}},
				denyAllRule("secret.**"),
			},
		},
	})
	assert.True(t, anyNode.AdminCanSubscribe("private.room"))
	assert.False(t, anyNode.AdminCanSubscribe("**"), "subscribe.any must not unlock bare ** (A3)")
	assert.False(t, anyNode.AdminCanSubscribe("secret.1"),
		"subscribe.any must not punch a hole in a deny_all rule")
	assert.False(t, anyNode.AdminCanPublish("secret.1"), "subscribe.any must not bypass publish deny_all")
}

// TestNode_ReplaceRulesRevokesSubscriptions verifies §9.11: after replacing
// the rules with chat.** DenyAll, PatternsToRevoke reports chat.** and the
// hub no longer carries the subscription; unaffected subscriptions stay.
func TestNode_ReplaceRulesRevokesSubscriptions(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	transport := &capturingTransport{}
	client, _, err := NewClient(ctx, node, transport, JSONMarshaler{})
	require.NoError(t, err)
	client.ForceTestIDs("sess-rr", "user-rr", "client-rr")
	require.NoError(t, node.AddClient(client))
	require.NoError(t, node.AddSubscription(ctx, "chat.**", NewSubscriber(client, false)))
	require.NoError(t, node.AddSubscription(ctx, "im.room.1", NewSubscriber(client, false)))

	require.NoError(t, node.ReplaceRules(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{denyAllRule("chat.**")},
	}))

	revoked := node.authorizer.PatternsToRevoke(
		Principal{Kind: PrincipalUser, UserID: "user-rr"},
		[]string{"chat.**", "im.room.1"},
	)
	assert.Equal(t, []string{"chat.**"}, revoked)

	_, ok := node.hub.LookupSubscriber("chat.**", client)
	assert.False(t, ok, "the revoked pattern must be removed from the hub")
	_, ok = node.hub.LookupSubscriber("im.room.1", client)
	assert.True(t, ok, "unaffected subscriptions stay")
}
