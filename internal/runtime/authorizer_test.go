package runtime

import (
	"context"
	"testing"

	"github.com/messageloopio/messageloop/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func denyAllRule(pattern string) config.AuthorizerRule {
	return config.AuthorizerRule{Pattern: pattern, DenyAll: true}
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
