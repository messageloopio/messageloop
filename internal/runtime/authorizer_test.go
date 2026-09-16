package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"
)

func denyAllRule(pattern string) config.AuthorizerRule {
	return config.AuthorizerRule{Pattern: pattern, DenyAll: true}
}

// TestNode_APICapabilitiesConfig verifies the capabilities wiring:
// omitted → DefaultCapabilityCeiling; explicit [] → zero bits; explicit list
// → only those bits.
func TestNode_APICapabilitiesConfig(t *testing.T) {
	omitted := NewNode(nil)
	assert.Equal(t, DefaultCapabilityCeiling, omitted.APICapabilities())

	empty := NewNode(&config.Server{API: config.ServerAPI{Capabilities: []string{}}})
	assert.Zero(t, empty.APICapabilities(), "an explicit empty list locks the Server API data plane")

	partial := NewNode(&config.Server{API: config.ServerAPI{
		Capabilities: []string{"history.read", "channels.list"},
	}})
	assert.Equal(t, CapHistoryRead|CapChannelsList, partial.APICapabilities())
}

// TestNode_APICanSubscribeAndPublish verifies §8.4 through the Node.
func TestNode_APICanSubscribeAndPublish(t *testing.T) {
	// Explicit capabilities without subscribe.any: the caller must appear in
	// allow lists like any user.
	node := NewNode(&config.Server{
		API: config.ServerAPI{Capabilities: []string{"history.read"}},
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", AllowSubscribe: []string{"alice"}},
				denyAllRule("secret.**"),
			},
		},
	})
	// Without subscribe.any the caller cannot subscribe to an allow-listed
	// channel it is not on.
	admin := node.apiPrincipal()
	assert.False(t, node.APICanSubscribe(admin, "private.room"))
	// Pattern.compile failures fail Server API subscribe too.
	assert.False(t, node.APICanSubscribe(admin, "**"))
	assert.False(t, node.APICanSubscribe(admin, "*.room"))
	// deny_all blocks Server API publish.
	assert.False(t, node.APICanPublish(admin, "secret.1"))
	assert.True(t, node.APICanPublish(admin, "private.room"))

	// With subscribe.any the static allow list is skipped.
	anyNode := NewNode(&config.Server{
		API: config.ServerAPI{Capabilities: []string{"subscribe.any"}},
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "private.*", AllowSubscribe: []string{"alice"}},
				denyAllRule("secret.**"),
			},
		},
	})
	anyAdmin := anyNode.apiPrincipal()
	assert.True(t, anyNode.APICanSubscribe(anyAdmin, "private.room"))
	assert.False(t, anyNode.APICanSubscribe(anyAdmin, "**"), "subscribe.any must not unlock bare ** (A3)")
	assert.False(t, anyNode.APICanSubscribe(anyAdmin, "secret.1"),
		"subscribe.any must not punch a hole in a deny_all rule")
	assert.False(t, anyNode.APICanPublish(anyAdmin, "secret.1"), "subscribe.any must not bypass publish deny_all")
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
