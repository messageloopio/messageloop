package authz

import (
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func policyBoolPtr(v bool) *bool { return &v }
func policyIntPtr(v int) *int    { return &v }

// Effects are resolved by the Authorizer (PR-KA-A4 §5.5): DefaultChannelPolicy
// overlaid by server.authorizer.default, then by every matching rule in table
// order (later overrides earlier). There is no first-match engine anymore.

func TestChannelPolicy_DefaultWhenEmpty(t *testing.T) {
	a, err := NewAuthorizer(config.AuthorizerConfig{})
	require.NoError(t, err)

	pol := a.Effects("any.channel")
	assert.True(t, pol.History)
	assert.True(t, pol.Presence)
	assert.True(t, pol.Recover)
	assert.False(t, pol.Survey)
	assert.False(t, pol.TransientOnly)
	assert.Equal(t, 256, pol.MaxSurveySubscribers)
	assert.Equal(t, 5*time.Second, pol.MaxSurveyTimeout)
	assert.Equal(t, 256, pol.PresenceSnapshotLimit)
}

// TestChannelPolicy_DefaultSpecOverridesDefault pins the authorizer default
// spec as the base overlay: e.g. default.history=false applies to every
// channel that matches no rule.
func TestChannelPolicy_DefaultSpecOverridesDefault(t *testing.T) {
	a, err := NewAuthorizer(config.AuthorizerConfig{
		Default: config.ChannelPolicySpec{History: policyBoolPtr(false), Survey: policyBoolPtr(true)},
	})
	require.NoError(t, err)

	pol := a.Effects("unmatched.channel")
	assert.False(t, pol.History)
	assert.True(t, pol.Survey)
}

// TestChannelPolicy_OverlayOrder verifies the overlay semantics: every
// matching rule contributes in table order and a later rule overrides an
// earlier one. The recommended ordering is generic rules first, specific
// rules last — the opposite of the old first-match engine.
func TestChannelPolicy_OverlayOrder(t *testing.T) {
	a, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "game.**", ChannelPolicySpec: config.ChannelPolicySpec{
				History: policyBoolPtr(true),
				Survey:  policyBoolPtr(true),
			}},
			{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
		},
	})
	require.NoError(t, err)

	// game.tick.fps matches both rules: the later transient_only rule wins
	// for its fields; the earlier survey:true overlay survives (overlay, not
	// first-match).
	tick := a.Effects("game.tick.fps")
	assert.True(t, tick.TransientOnly)
	assert.False(t, tick.History)
	assert.True(t, tick.Survey, "the game.** survey:true overlay survives the later transient_only rule")

	// game.room.1 matches only game.**.
	room := a.Effects("game.room.1")
	assert.False(t, room.TransientOnly)
	assert.True(t, room.History)
	assert.True(t, room.Survey)

	// No match at all resolves to the compiled default.
	other := a.Effects("im.room.1")
	assert.False(t, other.TransientOnly)
	assert.True(t, other.History)
	assert.False(t, other.Survey)
}

func TestChannelPolicy_OverlayNilKeepsDefault(t *testing.T) {
	a, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "im.**", ChannelPolicySpec: config.ChannelPolicySpec{HistorySize: policyIntPtr(5)}},
		},
	})
	require.NoError(t, err)

	pol := a.Effects("im.room.42")
	assert.Equal(t, 5, pol.HistorySize)
	// Unset fields must keep the compiled default.
	assert.True(t, pol.History, "unset history must keep default true")
	assert.True(t, pol.Presence, "unset presence must keep default true")
	assert.True(t, pol.Recover, "unset recover must keep default true")
	assert.False(t, pol.Survey, "unset survey must keep default false")
}

// TestChannelPolicy_ExplicitFalseOverridesDefault verifies an explicit false
// beats a default true (the overlay must not skip set-but-false values).
func TestChannelPolicy_ExplicitFalseOverridesDefault(t *testing.T) {
	a, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "iot.**", ChannelPolicySpec: config.ChannelPolicySpec{Presence: policyBoolPtr(false)}},
		},
	})
	require.NoError(t, err)

	pol := a.Effects("iot.device.1")
	assert.False(t, pol.Presence)
	assert.True(t, pol.History)
}

func TestChannelPolicy_TransientOnlyImpliesNoHistory(t *testing.T) {
	a, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
		},
	})
	require.NoError(t, err)

	pol := a.Effects("game.tick.fps")
	assert.True(t, pol.TransientOnly)
	assert.False(t, pol.History, "transient_only must force History=false")
	assert.False(t, pol.Recover, "transient_only must force Recover=false")

	// A rule that sets transient_only=true and history=true must still end
	// up with History=false.
	a2, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{
			{Pattern: "tick.**", ChannelPolicySpec: config.ChannelPolicySpec{
				TransientOnly: policyBoolPtr(true),
				History:       policyBoolPtr(true),
			}},
		},
	})
	require.NoError(t, err)
	pol2 := a2.Effects("tick.1")
	assert.True(t, pol2.TransientOnly)
	assert.False(t, pol2.History)
	assert.False(t, pol2.Recover)
}

// TestChannelPolicy_InvalidPatternErrors verifies the authorizer rejects
// invalid rule patterns (defense in depth; config.Validate rejects them
// earlier).
func TestChannelPolicy_InvalidPatternErrors(t *testing.T) {
	_, err := NewAuthorizer(config.AuthorizerConfig{
		Rules: []config.AuthorizerRule{{Pattern: "a.**.b"}},
	})
	require.Error(t, err)
}
