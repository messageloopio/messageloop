package session

import (
	"context"
	"fmt"

	"github.com/lynx-go/x/log"

	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
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

// precheckNamespace is the central namespace gate at the single inbound
// dispatch point (handleMessage), directly after the central auth gate
// (mechanism gap G1, review #4): every envelope that carries channel
// references is namespace-scoped here, so scoping no longer depends on each
// handler remembering to call checkNamespace. The per-handler checks stay in
// place as defense in depth; the precheck replicates each covered type's
// existing outward rejection so client-visible behavior does not change, and
// defines it for the types that had no check (Unsubscribe, SubRefresh).
//
// It returns the inbound message to dispatch — for Subscribe the message with
// the violating channels filtered out — or nil when the message was rejected
// and already answered with an error envelope. The returned error is only a
// send failure of that rejection envelope.
//
// Covered types and their rejection behavior:
//
//   - Subscribe: per channel, exactly like checkSubscribeACL step 0: one
//     NAMESPACE_MISMATCH error envelope per violating channel, violating
//     channels dropped, the in-namespace remainder dispatched (the ack and
//     subscription semantics stay with the handler).
//   - Publish: one NAMESPACE_MISMATCH error envelope, request not dispatched.
//     Empty and wildcard channels pass through: the handler's BAD_REQUEST
//     checks run before its namespace check today, and the precheck must not
//     reorder that.
//   - Unsubscribe: one NAMESPACE_MISMATCH error envelope, request not
//     dispatched. New check: the handler had none, and its unconditional
//     proxy notification leaks any requested channel name to backend routes.
//     Wildcards are checked (a pattern still names foreign channels); an
//     empty channel names nothing and stays with the handler.
//   - SubRefresh: one NAMESPACE_MISMATCH error envelope, request not
//     dispatched — subscribe-class semantics (top-level error envelope,
//     connection stays up, no SubRefreshAck). This is the review-#4 fix:
//     SubRefresh was the only channel handler without any namespace guard,
//     letting a client forge presence leaves on cross-namespace channels and
//     leak channel names to proxy backends through the subscribe-ACL route.
//   - SurveyRequest: one NAMESPACE_MISMATCH survey error envelope, sent
//     through sendSurveyError so survey_client_total stays in sync with the
//     handler's own rejection path. Empty and wildcard channels pass through
//     for the handler's BAD_REQUEST, as with Publish.
//   - PresenceQuery: one NAMESPACE_MISMATCH error envelope, request not
//     dispatched. Empty and wildcard channels pass through, as with Publish.
//
// Exemptions (each verified against the handler, not assumed):
//
//   - Connect: resolves the session namespace itself and runs before
//     authentication; its initial subscriptions already pass checkSubscribeACL
//     step 0.
//   - Ping / Pong: carry no channel reference.
//   - RpcRequest: carries a channel, but handleRPC enforces checkNamespace
//     itself before proxy routing (the channel participates in the proxy
//     route match), so the guard already holds on that path; the entry would
//     only double-enforce. Unlike the covered types, a regression there
//     relies on the handler — the census test in dispatch_precheck_test.go
//     keeps this classification from drifting.
//   - SurveyReply: carries no channel — it is routed by request id to an
//     in-flight survey whose channel passed the namespace check at
//     initiation, and Node.AddSurveyResponse drops replies from sessions the
//     survey was not sent to (internal/runtime/node.go), so no
//     cross-namespace data can enter through it.
func (c *Session) precheckNamespace(ctx context.Context, in *clientpb.InboundMessage) (*clientpb.InboundMessage, error) {
	switch msg := in.Envelope.(type) {
	case *clientpb.InboundMessage_Subscribe:
		filtered := false
		kept := make([]*clientpb.Subscription, 0, len(msg.Subscribe.Subscriptions))
		for _, sub := range msg.Subscribe.Subscriptions {
			if nsErr := c.precheckChannel(sub.Channel); nsErr != nil {
				filtered = true
				// The exact envelope checkSubscribeACL step 0 emits for this
				// channel, so the per-channel rejection is identical whether
				// it comes from here or from the handler.
				if err := c.sendNamespaceMismatch(ctx, in, sub.Channel, nsErr); err != nil {
					return nil, err
				}
				continue
			}
			kept = append(kept, sub)
		}
		if !filtered {
			return in, nil
		}
		// Dispatch the in-namespace remainder: the handler still runs its own
		// checks on it and owns the SubscribeAck.
		return &clientpb.InboundMessage{
			Id:   in.Id,
			Time: in.Time,
			Envelope: &clientpb.InboundMessage_Subscribe{
				Subscribe: &clientpb.Subscribe{Subscriptions: kept},
			},
		}, nil
	case *clientpb.InboundMessage_Publish:
		if nsErr := c.precheckExactChannel(msg.Publish.Channel); nsErr != nil {
			return nil, c.sendNamespaceMismatch(ctx, in, msg.Publish.Channel, nsErr)
		}
	case *clientpb.InboundMessage_Unsubscribe:
		for _, sub := range msg.Unsubscribe.Subscriptions {
			if nsErr := c.precheckChannel(sub.Channel); nsErr != nil {
				return nil, c.sendNamespaceMismatch(ctx, in, sub.Channel, nsErr)
			}
		}
	case *clientpb.InboundMessage_SubRefresh:
		for _, ch := range msg.SubRefresh.Channels {
			if nsErr := c.precheckChannel(ch); nsErr != nil {
				return nil, c.sendNamespaceMismatch(ctx, in, ch, nsErr)
			}
		}
	case *clientpb.InboundMessage_SurveyRequest:
		ch := msg.SurveyRequest.Channel
		if nsErr := c.precheckExactChannel(ch); nsErr != nil {
			// Route through sendSurveyError: it is the same top-level
			// NAMESPACE_MISMATCH envelope the handler sends, plus the
			// survey_client_total counter its rejection path increments.
			return nil, c.sendSurveyError(ctx, in, nsErr.Code, nsErr.Type, nsErr.Message)
		}
	case *clientpb.InboundMessage_PresenceQuery:
		ch := msg.PresenceQuery.Channel
		if nsErr := c.precheckExactChannel(ch); nsErr != nil {
			return nil, c.sendNamespaceMismatch(ctx, in, ch, nsErr)
		}
	default:
		// Connect, Ping, Pong, RpcRequest, SurveyReply: exempt (see the doc
		// comment) or channel-less; nothing to scope at the entry.
	}
	return in, nil
}

// precheckChannel applies the namespace scope to one channel reference at the
// dispatch entry. An empty channel names nothing: it is left to the handlers,
// which reject it as BAD_REQUEST (or, for Subscribe, exactly as their own
// checkNamespace always has).
func (c *Session) precheckChannel(channel string) *sharedv2.Error {
	if channel == "" {
		return nil
	}
	return c.checkNamespace(channel)
}

// precheckExactChannel is precheckChannel for the handlers whose BAD_REQUEST
// checks for empty and wildcard channels run before their namespace check
// (publish, survey request, presence query): the precheck must not reject
// those channels first, or it would reorder the client-visible errors.
func (c *Session) precheckExactChannel(channel string) *sharedv2.Error {
	if channel == "" || isWildcard(channel) {
		return nil
	}
	return c.checkNamespace(channel)
}

// sendNamespaceMismatch answers a precheck rejection with the same top-level
// NAMESPACE_MISMATCH envelope the handlers send, and keeps the connection up:
// only the request is denied, never the session.
func (c *Session) sendNamespaceMismatch(ctx context.Context, in *clientpb.InboundMessage, channel string, nsErr *sharedv2.Error) error {
	log.WarnContext(ctx, "namespace denied request at dispatch precheck",
		"channel", channel, "namespace", c.Namespace())
	return c.Send(ctx, MakeOutboundMessage(in, func(out *clientpb.OutboundMessage) {
		out.Envelope = &clientpb.OutboundMessage_Error{Error: nsErr}
	}))
}
