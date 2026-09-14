package session

import (
	"fmt"

	"github.com/messageloopio/messageloop/pkg/topics"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// Namespace returns the session's multi-tenant namespace, empty before the
// connect path resolved one.
func (c *Session) Namespace() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.namespace
}

// checkNamespace enforces the session's namespace scope at the session
// boundary: a client-visible channel must be a namespaced channel
// ("ns:topic", topics.ValidateChannel grammar) whose namespace equals the
// session's. It runs as step zero before the Authorizer and the proxy on
// every channel entry point (subscribe, publish, RPC, survey, presence
// query), so neither an ACL rule nor a proxy approval can leak a
// cross-namespace channel.
//
// A session without a namespace (test harnesses, sessions created before the
// cluster carried namespaces) skips the check; production sessions always
// carry one — handleConnect resolves it fail-closed from the auth proxy or
// the static server.namespace.
func (c *Session) checkNamespace(channel string) *sharedv2.Error {
	ns := c.Namespace()
	if ns == "" {
		return nil
	}
	chNs, err := topics.NamespaceOf(channel)
	if err != nil || chNs != ns {
		return &sharedv2.Error{
			Code:    "NAMESPACE_MISMATCH",
			Type:    "acl_error",
			Message: fmt.Sprintf("channel %q is outside the session namespace %q", channel, ns),
		}
	}
	return nil
}

// namespaceRequiredError is the error envelope for a connect that resolved no
// namespace (require_auth servers whose auth proxy returns none and with no
// static server.namespace fallback).
func namespaceRequiredError() *sharedv2.Error {
	return &sharedv2.Error{
		Code:    "NAMESPACE_REQUIRED",
		Type:    "auth_error",
		Message: "no namespace resolved: the authentication response carries no namespace and no static server.namespace is configured",
	}
}
