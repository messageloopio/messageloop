package serverapi

import (
	"context"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/pkg/topics"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
)

// The Server API scope layer (design §2.4, mechanism gaps G1/G4/G6): the single
// choke point every APIService RPC passes before its handler. It evaluates
// the declarative capability table (G6), enforces the global channel grammar
// gate (G4), and rewrites the request per the rejection semantics matrix so
// the handler never sees an out-of-scope target (G1) — invisible targets are
// erased, not flagged, so handler code stays scope-free.
//
// Handler contract: every RPC method body starts with its scope wrapper, so
// after it returns nil error the request context carries a verified identity
// and the request only contains in-scope targets (or client errors that the
// handler's own validation rejects).

// rpcScopeSpec is the registry entry of one Server API RPC: the capability bits a
// request needs (G6, evaluated from the request) and the authorization
// carrier classification the request carries (G1). Census test ① pins every
// APIServiceServer method against this registry; an unregistered method
// fails closed with Internal in scopeAuthorize.
type rpcScopeSpec struct {
	// requiredCaps evaluates the capability bits this request must hold.
	// Missing bits fail the whole RPC with PermissionDenied before any
	// traversal (same request-level denial shape as the old per-handler
	// requireAdminCaps calls).
	requiredCaps func(req any) authz.Capability
	// carriers classifies the request's authorization carriers. It is the
	// census-facing half of the registration: the traversal wrappers must
	// implement exactly these carriers.
	carriers scopeCarriers
}

// scopeCarriers records which carrier classes an RPC's request carries
// (matrix §2.4: named parameters reject, ID-addressed data becomes
// invisible, channels are per-item).
type scopeCarriers struct {
	namespaceParam bool // req.namespace is checked against the identity when users/user_id is non-empty
	channelLists   int  // repeated channel fields traversed per-item (publish: filtered; subscribe: results=false)
	singleChannel  bool // one channel field gated whole-RPC (survey/get_presence/get_history)
	sessionLists   int  // repeated session fields erased when invisible
	singleSession  bool // one session_id field erased when invisible
	userList       bool // repeated users field (namespace-checked, never erased)
	userID         bool // single user_id field (namespace-checked, never erased)
}

// rpcScopeRegistry is the declarative capability table + carrier
// classification for every APIService RPC (G6). Semantics per RPC:
//
//   - Publish: any publication with sessions needs session.act; any
//     publication with users needs user.fanout|session.act. Channel-only
//     publications are capability-free (D23: publishing is scoped by
//     namespace + Authorizer rules, not by capability bits).
//   - Disconnect: sessions → session.act; users → user.fanout|session.act.
//   - Subscribe/Unsubscribe: session_id → session.act; user_id →
//     user.fanout|session.act.
//   - Survey: no precondition bits (survey.bypass_gate is a behavior switch
//     evaluated inside the handler, not a gate).
//   - GetPresence/GetHistory/GetChannels: presence.read/history.read/
//     channels.list.
var rpcScopeRegistry = map[string]*rpcScopeSpec{
	"Publish": {
		requiredCaps: func(req any) authz.Capability {
			var caps authz.Capability
			for _, pub := range req.(*serverv2.PublishRequest).GetPublications() {
				dest := pub.GetDestination()
				if dest == nil {
					continue
				}
				if len(dest.GetSessions()) > 0 {
					caps |= authz.CapSessionAct
				}
				if len(dest.GetUsers()) > 0 {
					caps |= authz.CapUserFanout | authz.CapSessionAct
				}
			}
			return caps
		},
		carriers: scopeCarriers{namespaceParam: true, channelLists: 1, sessionLists: 1, userList: true},
	},
	"Disconnect": {
		requiredCaps: func(req any) authz.Capability {
			r := req.(*serverv2.DisconnectRequest)
			var caps authz.Capability
			if len(r.GetSessions()) > 0 {
				caps |= authz.CapSessionAct
			}
			if len(r.GetUsers()) > 0 {
				caps |= authz.CapUserFanout | authz.CapSessionAct
			}
			return caps
		},
		carriers: scopeCarriers{namespaceParam: true, sessionLists: 1, userList: true},
	},
	"Subscribe": {
		requiredCaps: func(req any) authz.Capability {
			r := req.(*serverv2.SubscribeRequest)
			var caps authz.Capability
			if r.GetSessionId() != "" {
				caps |= authz.CapSessionAct
			}
			if r.GetUserId() != "" {
				caps |= authz.CapUserFanout | authz.CapSessionAct
			}
			return caps
		},
		carriers: scopeCarriers{namespaceParam: true, channelLists: 1, singleSession: true, userID: true},
	},
	"Unsubscribe": {
		requiredCaps: func(req any) authz.Capability {
			r := req.(*serverv2.UnsubscribeRequest)
			var caps authz.Capability
			if r.GetSessionId() != "" {
				caps |= authz.CapSessionAct
			}
			if r.GetUserId() != "" {
				caps |= authz.CapUserFanout | authz.CapSessionAct
			}
			return caps
		},
		carriers: scopeCarriers{namespaceParam: true, channelLists: 1, singleSession: true, userID: true},
	},
	"Survey": {
		requiredCaps: func(any) authz.Capability { return 0 },
		carriers:     scopeCarriers{singleChannel: true},
	},
	"GetPresence": {
		requiredCaps: func(any) authz.Capability { return authz.CapPresenceRead },
		carriers:     scopeCarriers{singleChannel: true},
	},
	"GetHistory": {
		requiredCaps: func(any) authz.Capability { return authz.CapHistoryRead },
		carriers:     scopeCarriers{singleChannel: true},
	},
	"GetChannels": {
		requiredCaps: func(any) authz.Capability { return authz.CapChannelsList },
		// No request carriers: the namespace filtering happens on the
		// response (the handler drops channels outside the identity scope).
		carriers: scopeCarriers{},
	},
}

// scopeFieldHandled documents every request field the scope layer actively
// processes, keyed by proto full name and field name (G2). The value is the
// reason the field is an authorization carrier. Census test ② pins this
// table against the proto descriptors: a new request field must appear here
// or in scopeFieldExempt before it ships.
var scopeFieldHandled = map[string]map[string]string{
	"messageloop.server.v2.PublishRequest": {
		"publications": "traversed: every publication's destination is scoped (channels filtered, sessions erased, namespace checked)",
	},
	"messageloop.server.v2.Publication": {
		"destination": "traversed by the Publish scope walker",
	},
	"messageloop.server.v2.Publication.Destination": {
		"sessions":  "session IDs erased when invisible to the identity (matrix: ID-addressed data is not visible)",
		"channels":  "grammar gate for all identities; out-of-namespace channels filtered (counted failed)",
		"users":     "expansion targets stay; destination.namespace is checked before expansion",
		"namespace": "namespace parameter checked against the identity (users non-empty); mismatch rejects the whole RPC",
	},
	"messageloop.server.v2.DisconnectRequest": {
		"sessions":  "session IDs erased when invisible to the identity",
		"users":     "expansion targets stay; namespace is checked before expansion",
		"namespace": "namespace parameter checked against the identity (users non-empty)",
	},
	"messageloop.server.v2.SubscribeRequest": {
		"session_id": "erased when invisible; full erasure short-circuits a synthesized not-found response",
		"channels":   "grammar gate for all identities; out-of-namespace channels report results=false",
		"user_id":    "expansion target stays; namespace is checked before expansion",
		"namespace":  "namespace parameter checked against the identity (user_id set)",
	},
	"messageloop.server.v2.UnsubscribeRequest": {
		"session_id": "erased when invisible; full erasure short-circuits a synthesized not-found response",
		"channels":   "grammar gate for all identities; out-of-namespace channels report results=false",
		"user_id":    "expansion target stays; namespace is checked before expansion",
		"namespace":  "namespace parameter checked against the identity (user_id set)",
	},
	"messageloop.server.v2.SurveyRequest": {
		"channel": "single-channel grammar gate (all identities) + namespace check (scoped identities)",
	},
	"messageloop.server.v2.GetPresenceRequest": {
		"channel": "single-channel grammar gate (all identities) + namespace check (scoped identities)",
	},
	"messageloop.server.v2.GetHistoryRequest": {
		"channel": "single-channel grammar gate (all identities) + namespace check (scoped identities)",
	},
	// GetChannelsRequest carries no authorization carriers; its scope
	// behavior (response filtering by identity namespace) needs no field.
	"messageloop.server.v2.GetChannelsRequest": {},
}

// scopeFieldExempt documents every request field that is not an
// authorization carrier and therefore passes through the scope layer
// untouched. Keys follow scopeFieldHandled.
var scopeFieldExempt = map[string]map[string]string{
	"messageloop.server.v2.PublishRequest": {
		"request_id": "correlation id, not an authorization carrier",
	},
	"messageloop.server.v2.Publication": {
		"id":       "publication id, not an authorization carrier",
		"options":  "delivery options (add_history), enforced by channel policy in the handler",
		"payload":  "message data",
		"metadata": "message data",
	},
	"messageloop.server.v2.DisconnectRequest": {
		"code":   "disconnect code delivered to the session",
		"reason": "disconnect reason delivered to the session",
	},
	"messageloop.server.v2.SurveyRequest": {
		"request_id": "correlation id",
		"payload":    "survey question data",
		"metadata":   "survey metadata",
		"timeout_ms": "survey timeout, clamped by channel policy in the handler",
	},
	"messageloop.server.v2.GetHistoryRequest": {
		"since": "resume position",
		"limit": "page size",
	},
	"messageloop.server.v2.GetPresenceRequest":      {},
	"messageloop.server.v2.GetChannelsRequest":      {},
	"messageloop.server.v2.SubscribeRequest":        {},
	"messageloop.server.v2.UnsubscribeRequest":      {},
	"messageloop.server.v2.Publication.Destination": {},
}

// publishScopeResult carries the Publish scope outcome into the handler so
// erased targets keep the exact counting shape of unreachable targets
// (matrix: a publication to a non-existent session counts the attempt and
// moves on; a removed channel counts as failed).
type publishScopeResult struct {
	// erasedSessions counts invisible session IDs removed from the
	// destinations. The handler adds them to its attempted counter (they
	// behave exactly like not-found sessions).
	erasedSessions int
	// removedChannels counts out-of-namespace channels removed from the
	// destinations. The handler adds them to attempted AND failed (matrix:
	// the item counts failed, partial-success continues).
	removedChannels int
}

// subscribeScopeResult carries the Subscribe/Unsubscribe scope outcome into
// the handler.
type subscribeScopeResult struct {
	// erasedChannels holds the out-of-namespace channels removed from the
	// request; the handler reports results[ch]=false for each (matrix row:
	// channel references report false, like any failed subscribe).
	erasedChannels map[string]bool
	// shortCircuit is set when the only addressing target (session_id) was
	// erased and no user_id remains: the handler synthesizes a not-found
	// response (results[ch]=false for every requested channel) instead of
	// running into its "session_id and user_id must not both be empty"
	// InvalidArgument — which would both break the not-found semantics and
	// leak that the request was special-cased (design §2.4 matrix, rethink
	// fix 2).
	shortCircuit bool
}

// scopeAuthorize is the common prologue of every Server API RPC: it returns the
// verified identity after failing the RPC closed when the identity is
// missing (defensive — the auth interceptor always attaches one) or when the
// capability table denies a required bit (G6). An RPC missing from the
// registry fails with Internal ("capability table entry missing"); census
// test ① makes that unreachable.
func (h *apiServiceHandler) scopeAuthorize(ctx context.Context, method string, req any) (authz.APIIdentity, error) {
	id, ok := APIIdentityFromContext(ctx)
	if !ok {
		return authz.APIIdentity{}, status.Error(codes.Internal, "server API identity missing from request context")
	}
	spec, ok := rpcScopeRegistry[method]
	if !ok {
		return authz.APIIdentity{}, status.Errorf(codes.Internal, "capability table entry missing for Server API RPC %s", method)
	}
	if required := spec.requiredCaps(req); id.Caps&required != required {
		return authz.APIIdentity{}, status.Errorf(codes.PermissionDenied, "%s requires server API capability", method)
	}
	return id, nil
}

// identityFromScope returns the Server API identity the scope layer validated for
// this call. Every handler runs its scope wrapper first, so the identity is
// guaranteed to be present; a zero identity fails closed downstream (its
// Caps miss every bit and its namespace scope is empty).
func identityFromScope(ctx context.Context) authz.APIIdentity {
	id, _ := APIIdentityFromContext(ctx)
	return id
}

// identityIsGlobal reports whether the identity holds the ["*"] scope:
// namespace checks are skipped while the global channel grammar gate (G4)
// stays on for every identity.
func identityIsGlobal(id authz.APIIdentity) bool {
	return slices.Contains(id.Namespaces, "*")
}

// sessionVisibleTo reports whether the identity may address the session:
// global identities see everything (session addressing is only narrowed for
// scoped keys); a scoped identity sees the session when its lease namespace
// resolves and falls inside the identity scope. Unresolvable namespaces
// report invisible (fail-closed, APISessionNamespace contract).
func (h *apiServiceHandler) sessionVisibleTo(ctx context.Context, id authz.APIIdentity, sessionID string) bool {
	if identityIsGlobal(id) {
		return true
	}
	ns, ok := h.node.APISessionNamespace(ctx, sessionID)
	return ok && id.AllowsNamespace(ns)
}

// scopeChannelReference enforces the G4 grammar gate plus the namespace
// scope on a single-channel request: a syntactically invalid channel is a
// client error (InvalidArgument) for every identity, an out-of-namespace
// channel is PermissionDenied for scoped identities (matrix row: single
// channel references reject the RPC).
func scopeChannelReference(id authz.APIIdentity, channel string) error {
	if err := topics.ValidateChannel(channel); err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid channel %q: %v", channel, err)
	}
	if !identityIsGlobal(id) && !id.AllowsChannel(channel) {
		return status.Error(codes.PermissionDenied, "channel is outside the caller's namespace scope")
	}
	return nil
}

// scopePublish rewrites the Publish request in place: out-of-namespace
// channels are removed (counted as failed), invisible sessions are erased
// (counted as not-found attempts), and destination namespaces are checked
// before any expansion. The G4 grammar gate rejects syntactically invalid
// channels for every identity.
func (h *apiServiceHandler) scopePublish(ctx context.Context, req *serverv2.PublishRequest) (publishScopeResult, error) {
	id, err := h.scopeAuthorize(ctx, "Publish", req)
	if err != nil {
		return publishScopeResult{}, err
	}
	scoped := !identityIsGlobal(id)
	var out publishScopeResult
	for _, pub := range req.GetPublications() {
		dest := pub.GetDestination()
		if dest == nil {
			continue
		}
		// Named namespace parameter: reject the whole RPC on mismatch
		// (matrix row 1) — explicit and diagnosable.
		if len(dest.GetUsers()) > 0 && dest.GetNamespace() != "" && !id.AllowsNamespace(dest.GetNamespace()) {
			return publishScopeResult{}, status.Error(codes.PermissionDenied, "destination.namespace is outside the caller's namespace scope")
		}
		kept := make([]string, 0, len(dest.GetChannels()))
		for _, channel := range dest.GetChannels() {
			if err := topics.ValidateChannel(channel); err != nil {
				return publishScopeResult{}, status.Errorf(codes.InvalidArgument, "invalid channel %q: %v", channel, err)
			}
			if scoped && !id.AllowsChannel(channel) {
				out.removedChannels++
				continue
			}
			kept = append(kept, channel)
		}
		dest.Channels = kept
		if scoped {
			keptSessions := make([]string, 0, len(dest.GetSessions()))
			for _, sessionID := range dest.GetSessions() {
				if !h.sessionVisibleTo(ctx, id, sessionID) {
					out.erasedSessions++
					continue
				}
				keptSessions = append(keptSessions, sessionID)
			}
			dest.Sessions = keptSessions
		}
	}
	return out, nil
}

// scopeDisconnect checks the Disconnect namespace parameter and erases
// invisible sessions in place. A fully-erased request degrades to an empty
// results map inside the handler — the not-found shape (matrix: disconnect
// results report false / absent, never an error).
func (h *apiServiceHandler) scopeDisconnect(ctx context.Context, req *serverv2.DisconnectRequest) error {
	id, err := h.scopeAuthorize(ctx, "Disconnect", req)
	if err != nil {
		return err
	}
	if len(req.GetUsers()) > 0 && req.GetNamespace() != "" && !id.AllowsNamespace(req.GetNamespace()) {
		return status.Error(codes.PermissionDenied, "namespace is outside the caller's namespace scope")
	}
	if identityIsGlobal(id) {
		return nil
	}
	kept := make([]string, 0, len(req.GetSessions()))
	for _, sessionID := range req.GetSessions() {
		if h.sessionVisibleTo(ctx, id, sessionID) {
			kept = append(kept, sessionID)
		}
	}
	req.Sessions = kept
	return nil
}

// scopeSubscribe rewrites the Subscribe request in place and reports the
// erased channels plus the not-found short-circuit (see
// subscribeScopeResult).
func (h *apiServiceHandler) scopeSubscribe(ctx context.Context, req *serverv2.SubscribeRequest) (subscribeScopeResult, error) {
	if _, err := h.scopeAuthorize(ctx, "Subscribe", req); err != nil {
		return subscribeScopeResult{}, err
	}
	sessionID, channels, out, err := h.scopeSubscribeTargets(ctx, req.GetSessionId(), req.GetChannels(), req.GetUserId(), req.GetNamespace())
	if err != nil {
		return subscribeScopeResult{}, err
	}
	req.SessionId = sessionID
	req.Channels = channels
	return out, nil
}

// scopeUnsubscribe rewrites the Unsubscribe request in place, exactly like
// scopeSubscribe.
func (h *apiServiceHandler) scopeUnsubscribe(ctx context.Context, req *serverv2.UnsubscribeRequest) (subscribeScopeResult, error) {
	if _, err := h.scopeAuthorize(ctx, "Unsubscribe", req); err != nil {
		return subscribeScopeResult{}, err
	}
	sessionID, channels, out, err := h.scopeSubscribeTargets(ctx, req.GetSessionId(), req.GetChannels(), req.GetUserId(), req.GetNamespace())
	if err != nil {
		return subscribeScopeResult{}, err
	}
	req.SessionId = sessionID
	req.Channels = channels
	return out, nil
}

// scopeSubscribeTargets is the shared Subscribe/Unsubscribe traversal: it
// checks the namespace parameter, filters the channel list (erased channels
// are reported for results[ch]=false), and erases an invisible session_id —
// short-circuiting when no addressing target remains. The rewritten
// sessionID/channels are returned for the caller to write back.
func (h *apiServiceHandler) scopeSubscribeTargets(ctx context.Context, sessionID string, channels []string, userID, namespace string) (string, []string, subscribeScopeResult, error) {
	id := identityFromScope(ctx)
	out := subscribeScopeResult{erasedChannels: make(map[string]bool)}
	scoped := !identityIsGlobal(id)

	if userID != "" && namespace != "" && !id.AllowsNamespace(namespace) {
		return sessionID, channels, out, status.Error(codes.PermissionDenied, "namespace is outside the caller's namespace scope")
	}
	kept := make([]string, 0, len(channels))
	for _, channel := range channels {
		if err := topics.ValidateChannel(channel); err != nil {
			return sessionID, channels, out, status.Errorf(codes.InvalidArgument, "invalid channel %q: %v", channel, err)
		}
		if scoped && !id.AllowsChannel(channel) {
			out.erasedChannels[channel] = true
			continue
		}
		kept = append(kept, channel)
	}
	channels = kept
	if scoped && sessionID != "" && !h.sessionVisibleTo(ctx, id, sessionID) {
		sessionID = ""
		if userID == "" {
			out.shortCircuit = true
		}
	}
	return sessionID, channels, out, nil
}

// scopeSurvey applies the single-channel scope to Survey.
func (h *apiServiceHandler) scopeSurvey(ctx context.Context, req *serverv2.SurveyRequest) error {
	id, err := h.scopeAuthorize(ctx, "Survey", req)
	if err != nil {
		return err
	}
	return scopeChannelReference(id, req.GetChannel())
}

// scopeGetPresence applies the single-channel scope to GetPresence.
func (h *apiServiceHandler) scopeGetPresence(ctx context.Context, req *serverv2.GetPresenceRequest) error {
	id, err := h.scopeAuthorize(ctx, "GetPresence", req)
	if err != nil {
		return err
	}
	return scopeChannelReference(id, req.GetChannel())
}

// scopeGetHistory applies the single-channel scope to GetHistory.
func (h *apiServiceHandler) scopeGetHistory(ctx context.Context, req *serverv2.GetHistoryRequest) error {
	id, err := h.scopeAuthorize(ctx, "GetHistory", req)
	if err != nil {
		return err
	}
	return scopeChannelReference(id, req.GetChannel())
}

// scopeGetChannels applies the capability gate to GetChannels. The response
// filtering by identity namespace happens in the handler (no request
// carriers).
func (h *apiServiceHandler) scopeGetChannels(ctx context.Context, req *serverv2.GetChannelsRequest) error {
	_, err := h.scopeAuthorize(ctx, "GetChannels", req)
	return err
}
