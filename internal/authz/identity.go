package authz

import (
	"slices"

	"github.com/messageloopio/messageloop/pkg/topics"
)

// AdminIdentity is the authenticated identity of one admin API call
// (design §2.1). It is produced by the admin auth chain: the static
// auth_tokens list, the allow_insecure escape hatch, or a proxy-verified
// API key (clamped by the node capability ceiling).
type AdminIdentity struct {
	// KeyID identifies the credential: "static-token" for the static
	// auth_tokens list, "insecure" for allow_insecure, and the backend's
	// unique Key ID for proxy-verified keys (design D26: the unique ID, not
	// the display name, so allow lists and logs stay attributable).
	KeyID string
	// Namespaces is the namespace scope: ["*"] (all) or an exact list. An
	// empty list rejects everything (fail-closed, design D5).
	Namespaces []string
	// Caps carries the effective capability bits, already clamped by the
	// node capability ceiling (design D17).
	Caps Capability
}

// AdminIdentity key IDs. The static token list and the allow_insecure
// escape hatch share the fixed "admin" principal; proxy keys are namespaced
// under "key:" so per-Key allow lists (allow_publish etc.) can match them.
const (
	adminIdentityKeyIDStatic   = "static-token"
	adminIdentityKeyIDInsecure = "insecure"
	adminPrincipalUserID       = "admin"
)

// Principal returns the authorization subject for the identity. Static
// tokens and allow_insecure keep the historical fixed "admin" principal;
// a proxy-verified key maps to "key:"+KeyID. Kind is always PrincipalAdmin
// and the (already clamped) capability bits pass through.
func (i AdminIdentity) Principal() Principal {
	userID := adminPrincipalUserID
	if i.KeyID != adminIdentityKeyIDStatic && i.KeyID != adminIdentityKeyIDInsecure {
		userID = "key:" + i.KeyID
	}
	return Principal{Kind: PrincipalAdmin, UserID: userID, Caps: i.Caps}
}

// AllowsNamespace reports whether ns is inside the identity's namespace
// scope: ["*"] allows everything, otherwise the exact list decides (an
// empty list allows nothing).
func (i AdminIdentity) AllowsNamespace(ns string) bool {
	if slices.Contains(i.Namespaces, "*") {
		return true
	}
	return slices.Contains(i.Namespaces, ns)
}

// AllowsChannel reports whether the identity's namespace scope covers the
// namespaced channel ch. A channel that does not parse under the
// ns:topic grammar is rejected (fail-closed), regardless of scope.
func (i AdminIdentity) AllowsChannel(ch string) bool {
	ns, err := topics.NamespaceOf(ch)
	if err != nil {
		return false
	}
	return i.AllowsNamespace(ns)
}
