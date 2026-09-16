package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAPIIdentity_Principal pins the principal mapping (design §2.1 /
// D26): static token and allow_insecure keep the fixed "admin" principal,
// proxy keys get "key:"+KeyID, Kind is always PrincipalServer, and the
// (already clamped) capability bits pass through untouched.
func TestAPIIdentity_Principal(t *testing.T) {
	assert := assert.New(t)

	static := APIIdentity{KeyID: "static-token", Namespaces: []string{"*"}, Caps: CapHistoryRead | CapSessionAct}
	p := static.Principal()
	assert.Equal(PrincipalServer, p.Kind)
	assert.Equal("admin", p.UserID)
	assert.Equal(static.Caps, p.Caps)

	insecure := APIIdentity{KeyID: "insecure", Namespaces: []string{"*"}, Caps: CapPresenceRead}
	p = insecure.Principal()
	assert.Equal(PrincipalServer, p.Kind)
	assert.Equal("admin", p.UserID)
	assert.Equal(insecure.Caps, p.Caps)

	key := APIIdentity{KeyID: "key-42", Namespaces: []string{"acme"}, Caps: CapHistoryRead}
	p = key.Principal()
	assert.Equal(PrincipalServer, p.Kind)
	assert.Equal("key:key-42", p.UserID)
	assert.Equal(key.Caps, p.Caps)
}

// TestAPIIdentity_AllowsNamespace covers the namespace scope semantics:
// ["*"] allows everything, an exact list decides membership, and the empty
// list rejects everything (fail-closed, D5).
func TestAPIIdentity_AllowsNamespace(t *testing.T) {
	assert := assert.New(t)

	all := APIIdentity{Namespaces: []string{"*"}}
	assert.True(all.AllowsNamespace("acme"))
	assert.True(all.AllowsNamespace("beta"))

	scoped := APIIdentity{Namespaces: []string{"acme", "beta"}}
	assert.True(scoped.AllowsNamespace("acme"))
	assert.True(scoped.AllowsNamespace("beta"))
	assert.False(scoped.AllowsNamespace("gamma"))

	// Empty list = deny everything.
	empty := APIIdentity{Namespaces: nil}
	assert.False(empty.AllowsNamespace("acme"))
	empty = APIIdentity{Namespaces: []string{}}
	assert.False(empty.AllowsNamespace("acme"))
}

// TestAPIIdentity_AllowsChannel is table-driven over the channel grammar
// boundary: valid namespaced channels resolve via topics.NamespaceOf and
// follow AllowsNamespace; malformed channels (no colon, multiple colons,
// invalid namespace characters, empty namespace) are rejected regardless of
// scope — even for the ["*"] identity.
func TestAPIIdentity_AllowsChannel(t *testing.T) {
	assert := assert.New(t)
	all := APIIdentity{Namespaces: []string{"*"}}
	scoped := APIIdentity{Namespaces: []string{"acme"}}
	empty := APIIdentity{Namespaces: nil}

	cases := []struct {
		channel string
		all     bool
		scoped  bool
		empty   bool
		note    string
	}{
		{"acme:chat.room1", true, true, false, "valid channel inside scope"},
		{"beta:chat.room1", true, false, false, "valid channel outside scope"},
		{"acme:chat", true, true, false, "single-segment topic"},
		{"chat.room1", false, false, false, "no namespace delimiter"},
		{"acme:chat:room1", false, false, false, "double namespace delimiter"},
		{"acme_chat:room", false, false, false, "invalid namespace character (underscore)"},
		{"ACME:chat", false, false, false, "uppercase namespace rejected"},
		{"-acme:chat", false, false, false, "leading dash namespace rejected"},
		{":chat", false, false, false, "empty namespace rejected"},
		{"acme:", true, true, false, "empty topic still resolves to the namespace (NamespaceOf is namespace-scoped)"},
		{"", false, false, false, "empty channel rejected"},
	}
	for _, tc := range cases {
		assert.Equal(tc.all, all.AllowsChannel(tc.channel), "all-identity, channel %q (%s)", tc.channel, tc.note)
		assert.Equal(tc.scoped, scoped.AllowsChannel(tc.channel), "scoped-identity, channel %q (%s)", tc.channel, tc.note)
		assert.Equal(tc.empty, empty.AllowsChannel(tc.channel), "empty-identity, channel %q (%s)", tc.channel, tc.note)
	}

	// Wildcards in the topic part do not bypass the namespace guard.
	assert.True(all.AllowsChannel("acme:*"), "topic wildcard under covered namespace")
	assert.False(scoped.AllowsChannel("beta:*"), "topic wildcard outside scope")
}
