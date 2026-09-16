package serverapi

import (
	"context"
	"sort"
	"time"

	"github.com/lynx-go/x/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/messageloopio/messageloop/internal/authz"
	"github.com/messageloopio/messageloop/internal/occupancy"
	"github.com/messageloopio/messageloop/internal/protocol"
	"github.com/messageloopio/messageloop/internal/runtime"
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/pkg/topics"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

type apiServiceHandler struct {
	serverv2.UnimplementedAPIServiceServer
	node *runtime.Node
}

func NewAPIServiceHandler(node *runtime.Node) serverv2.APIServiceServer {
	return &apiServiceHandler{node: node}
}

// Every RPC method body starts with its scope wrapper (internal/serverapi/scope.go,
// design §2.4): capability table + channel grammar gate + namespace matrix
// rewrite. After the wrapper returns nil error the request context carries a
// verified identity and the request holds only in-scope targets — the
// handlers below are scope-free.

func (h *apiServiceHandler) Publish(ctx context.Context, req *serverv2.PublishRequest) (*serverv2.PublishResponse, error) {
	scope, err := h.scopePublish(ctx, req)
	if err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "server side API Publish", "request_id", req.RequestId)

	// Empty user IDs inside destination.users are a client error: reject the
	// whole request before any scanning happens (anonymous connections are
	// never addressable by the user-based API). A user expansion also needs a
	// namespace: sessions are scoped per namespace.
	for _, pub := range req.Publications {
		dest := pub.GetDestination()
		for _, userID := range dest.GetUsers() {
			if userID == "" {
				return nil, status.Errorf(codes.InvalidArgument, "destination.users must not contain an empty user_id (publication %q)", pub.GetId())
			}
			if dest.GetNamespace() == "" {
				return nil, status.Errorf(codes.InvalidArgument, "destination.namespace is required when destination.users is set (publication %q)", pub.GetId())
			}
		}
	}

	// PublishResponse has no per-publication result fields, so failures are
	// reported with partial-success semantics: every failure is logged, and
	// when all publications fail the RPC returns an error. The scope layer's
	// erasures join the counters up front: erased sessions behave exactly
	// like not-found sessions (attempted, skipped), removed channels like
	// failed deliveries.
	principal := identityFromScope(ctx).Principal()
	attempted := scope.erasedSessions + scope.removedChannels
	failed := scope.removedChannels
	for _, pub := range req.Publications {
		// Extract data from Payload, preserving the original oneof variant.
		brokerPub, err := stream.PublicationFromPayloadV2(pub.Id, pub.GetMetadata().GetEntries(), pub.GetPayload())
		if err != nil {
			log.ErrorContext(ctx, "failed to marshal JSON payload", err, "publication_id", pub.Id)
			attempted++
			failed++
			continue
		}

		// Get destination: sessions, channels, and users may be combined;
		// a destination with only users is valid.
		dest := pub.GetDestination()
		if dest == nil || (len(dest.Sessions) == 0 && len(dest.Channels) == 0 && len(dest.Users) == 0) {
			log.WarnContext(ctx, "publication has no destination", "publication_id", pub.Id)
			attempted++
			failed++
			continue
		}

		// Session-based publication: explicit sessions unioned (deduplicated)
		// with every user's expanded sessions.
		for _, sessionID := range h.unionSessions(ctx, dest.Sessions, dest.Users, dest.GetNamespace(), "publish") {
			attempted++
			// The Server API wire payload is already shared.v2 — the same shape
			// the client.v2 session consumes, so it passes through directly.
			msg := &clientpb.Message{
				Channel: "", // Session-based, no channel
				Id:      pub.Id,
				Payload: pub.Payload,
			}

			ok, err := h.node.PublishToSession(ctx, sessionID, msg)
			if err != nil {
				log.ErrorContext(ctx, "failed to send to session", err, "session_id", sessionID)
				failed++
			} else if !ok {
				log.DebugContext(ctx, "session not found, skipping", "session_id", sessionID)
			}
		}

		// Channel-based publication
		for _, channel := range dest.Channels {
			attempted++
			if !h.node.APICanPublish(principal, channel) {
				log.WarnContext(ctx, "server API publish denied by ACL rule", "channel", channel)
				failed++
				continue
			}
			opts := pub.GetOptions()
			pol := h.node.ChannelPolicy(channel)
			if pol.TransientOnly || !pol.History {
				// Channel policy disables history: add_history cannot be
				// honored. Count the failure and do not publish at all so
				// the caller does not assume the message was written.
				// Transient delivery is still allowed.
				if opts != nil && opts.AddHistory {
					log.WarnContext(ctx, "server API add_history denied by channel policy", "channel", channel)
					failed++
					continue
				}
				if err := h.node.PublishTransient(channel, brokerPub); err != nil {
					log.ErrorContext(ctx, "failed to publish transient to channel", err, "channel", channel)
					failed++
				}
				continue
			}
			if opts != nil && opts.AddHistory {
				if _, err := h.node.Publish(channel, brokerPub); err != nil {
					log.ErrorContext(ctx, "failed to publish to channel", err, "channel", channel)
					failed++
				}
			} else {
				if err := h.node.PublishTransient(channel, brokerPub); err != nil {
					log.ErrorContext(ctx, "failed to publish transient to channel", err, "channel", channel)
					failed++
				}
			}
		}
	}

	if attempted > 0 && failed == attempted {
		return nil, status.Errorf(codes.Internal, "all %d delivery attempt(s) failed", failed)
	}
	return &serverv2.PublishResponse{}, nil
}

func (h *apiServiceHandler) Survey(ctx context.Context, req *serverv2.SurveyRequest) (*serverv2.SurveyResponse, error) {
	if err := h.scopeSurvey(ctx, req); err != nil {
		return nil, err
	}
	id := identityFromScope(ctx)
	log.InfoContext(ctx, "server side API Survey", "channel", req.Channel, "request_id", req.RequestId)

	// Without survey.bypass_gate the Server API survey runs through the same
	// gates as a client survey: the Survey decision (Effects.Survey +
	// allow_survey / deny_all) and the population cap (PR-KA-A4 §7). With
	// the bit, today's gate-free behavior is preserved. The bit follows the
	// caller's identity (static tokens/insecure hold the node ceiling; keys
	// carry their own clamped bits).
	if id.Caps&authz.CapSurveyBypassGate == 0 {
		if !h.node.APIDecide(id.Principal(), authz.ActionSurvey, req.Channel).Allow {
			return nil, status.Error(codes.PermissionDenied, "survey denied by ACL rule")
		}
		total, err := h.node.CountMatchingSubscribers(ctx, req.Channel)
		if err != nil {
			return nil, err
		}
		if limit := h.node.ChannelPolicy(req.Channel).MaxSurveySubscribers; limit > 0 && total > limit {
			return nil, status.Error(codes.ResourceExhausted, "survey refused: too many subscribers")
		}
	}

	// Clamp the requested timeout exactly like the client survey path
	// (client.go: policy cap with a 5s default, a 10s hard ceiling, and a
	// 100ms floor) so a Server API request cannot pin survey slots for an
	// unbounded time.
	timeout := h.node.ChannelPolicy(req.Channel).MaxSurveyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if timeout > 10*time.Second {
		timeout = 10 * time.Second
	}
	if req.TimeoutMs > 0 {
		requested := time.Duration(req.TimeoutMs) * time.Millisecond
		if requested > timeout {
			requested = timeout
		}
		if requested < 100*time.Millisecond {
			requested = 100 * time.Millisecond
		}
		timeout = requested
	}
	payload, err := payloadBytes(req.Payload)
	if err != nil {
		return nil, err
	}
	results, err := h.node.Survey(ctx, req.Channel, payload, timeout)
	if err != nil {
		return nil, err
	}

	response := &serverv2.SurveyResponse{
		RequestId: req.RequestId,
		Results:   make([]*serverv2.SurveyResult, 0, len(results)),
	}
	for _, result := range results {
		item := &serverv2.SurveyResult{SessionId: result.SessionID}
		if len(result.Payload) > 0 {
			item.Payload = &sharedv2.Payload{Data: &sharedv2.Payload_Binary{Binary: result.Payload}}
		}
		metadata := make(map[string]string)
		if result.NodeID != "" {
			metadata["node_id"] = result.NodeID
		}
		if result.IncarnationID != "" {
			metadata["incarnation_id"] = result.IncarnationID
		}
		if len(metadata) > 0 {
			item.Metadata = &sharedv2.Metadata{Entries: metadata}
		}
		if result.Error != nil {
			item.Error = &sharedv2.Error{Code: "SURVEY_FAILED", Message: result.Error.Error()}
		}
		response.Results = append(response.Results, item)
	}

	return response, nil
}

func (h *apiServiceHandler) Disconnect(ctx context.Context, req *serverv2.DisconnectRequest) (*serverv2.DisconnectResponse, error) {
	if err := h.scopeDisconnect(ctx, req); err != nil {
		return nil, err
	}
	log.InfoContext(ctx, "server side API Disconnect", "sessions", req.Sessions, "users", req.Users, "code", req.Code, "reason", req.Reason)

	for _, userID := range req.Users {
		if userID == "" {
			return nil, status.Error(codes.InvalidArgument, "users must not contain an empty user_id")
		}
	}
	if len(req.Users) > 0 && req.Namespace == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required when users is set")
	}

	results := make(map[string]bool)

	for _, sessionID := range h.unionSessions(ctx, req.Sessions, req.Users, req.Namespace, "disconnect") {
		// Close the client with disconnect reason
		disconnect := protocol.Disconnect{
			Code:   req.Code,
			Reason: req.Reason,
		}

		ok, err := h.node.DisconnectSession(ctx, sessionID, disconnect)
		if err != nil {
			results[sessionID] = false
			log.ErrorContext(ctx, "failed to disconnect session", err)
		} else {
			results[sessionID] = ok
		}
	}

	return &serverv2.DisconnectResponse{Results: results}, nil
}

func (h *apiServiceHandler) Subscribe(ctx context.Context, req *serverv2.SubscribeRequest) (*serverv2.SubscribeResponse, error) {
	scope, err := h.scopeSubscribe(ctx, req)
	if err != nil {
		return nil, err
	}
	// Scope short-circuit: the only addressing target was erased (invisible
	// session and no user left). Synthesize the not-found response here —
	// falling through would hit the "session_id and user_id must not both
	// be empty" InvalidArgument below, breaking the not-found semantics and
	// leaking that the request was special-cased (design §2.4 matrix).
	if scope.shortCircuit {
		results := make(map[string]bool, len(req.GetChannels())+len(scope.erasedChannels))
		for channel := range scope.erasedChannels {
			results[channel] = false
		}
		for _, channel := range req.GetChannels() {
			results[channel] = false
		}
		return &serverv2.SubscribeResponse{Results: results}, nil
	}
	log.InfoContext(ctx, "server side API Subscribe", "session_id", req.SessionId, "user_id", req.UserId, "channels", req.Channels)

	if req.SessionId == "" && req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id must not both be empty")
	}
	if req.UserId != "" && req.Namespace == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required when user_id is set")
	}

	sessions := h.unionSessions(ctx, []string{req.SessionId}, []string{req.UserId}, req.Namespace, "subscribe")
	principal := identityFromScope(ctx).Principal()
	results := make(map[string]bool, len(req.Channels)+len(scope.erasedChannels))

	// Out-of-namespace channels were removed by the scope layer: they report
	// false like any other failed subscribe (design §2.4 matrix).
	for channel := range scope.erasedChannels {
		results[channel] = false
	}
	for _, ch := range req.Channels {
		// With multiple sessions (user fan-out), any successful session wins
		// the channel's result: false only when every session failed.
		ok := false
		for _, sessionID := range sessions {
			subscribed, err := h.node.SubscribeSession(ctx, principal, sessionID, ch)
			if err != nil {
				log.ErrorContext(ctx, "failed to subscribe to channel", err, "channel", ch, "session_id", sessionID)
				continue
			}
			if subscribed {
				ok = true
				break
			}
		}
		results[ch] = ok
	}

	return &serverv2.SubscribeResponse{Results: results}, nil
}

func (h *apiServiceHandler) Unsubscribe(ctx context.Context, req *serverv2.UnsubscribeRequest) (*serverv2.UnsubscribeResponse, error) {
	scope, err := h.scopeUnsubscribe(ctx, req)
	if err != nil {
		return nil, err
	}
	// Scope short-circuit, mirroring Subscribe (see there).
	if scope.shortCircuit {
		results := make(map[string]bool, len(req.GetChannels())+len(scope.erasedChannels))
		for channel := range scope.erasedChannels {
			results[channel] = false
		}
		for _, channel := range req.GetChannels() {
			results[channel] = false
		}
		return &serverv2.UnsubscribeResponse{Results: results}, nil
	}
	log.InfoContext(ctx, "server side API Unsubscribe", "session_id", req.SessionId, "user_id", req.UserId, "channels", req.Channels)

	if req.SessionId == "" && req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id must not both be empty")
	}
	if req.UserId != "" && req.Namespace == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required when user_id is set")
	}

	sessions := h.unionSessions(ctx, []string{req.SessionId}, []string{req.UserId}, req.Namespace, "unsubscribe")
	principal := identityFromScope(ctx).Principal()
	results := make(map[string]bool, len(req.Channels)+len(scope.erasedChannels))

	for channel := range scope.erasedChannels {
		results[channel] = false
	}
	for _, ch := range req.Channels {
		// With multiple sessions (user fan-out), any successful session wins
		// the channel's result: false only when every session failed.
		ok := false
		for _, sessionID := range sessions {
			unsubscribed, err := h.node.UnsubscribeSession(ctx, principal, sessionID, ch)
			if err != nil {
				log.ErrorContext(ctx, "failed to unsubscribe from channel", err, "channel", ch, "session_id", sessionID)
				continue
			}
			if unsubscribed {
				ok = true
				break
			}
		}
		results[ch] = ok
	}

	return &serverv2.UnsubscribeResponse{Results: results}, nil
}

// unionSessions expands the users list into session IDs for the given
// namespace (via the node's user index plus the local hub) and unions them
// with the explicit session list, deduplicated and sorted for deterministic
// execution order. Empty user IDs must have been rejected by the caller. The
// per-user fan-out metric is observed with the given op label.
func (h *apiServiceHandler) unionSessions(ctx context.Context, explicit []string, users []string, namespace, op string) []string {
	seen := make(map[string]struct{}, len(explicit)+len(users))
	for _, sessionID := range explicit {
		if sessionID == "" {
			continue
		}
		seen[sessionID] = struct{}{}
	}
	for _, userID := range users {
		expanded := h.node.ExpandUserSessions(ctx, namespace, userID)
		h.node.ObserveAPIUserFanout(op, len(expanded))
		for _, sessionID := range expanded {
			seen[sessionID] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for sessionID := range seen {
		result = append(result, sessionID)
	}
	sort.Strings(result)
	return result
}

func (h *apiServiceHandler) GetPresence(ctx context.Context, req *serverv2.GetPresenceRequest) (*serverv2.GetPresenceResponse, error) {
	if err := h.scopeGetPresence(ctx, req); err != nil {
		return nil, err
	}
	id := identityFromScope(ctx)
	log.InfoContext(ctx, "server side API GetPresence", "channel", req.Channel)

	// The channel must be allowed for the caller's principal (presence.read
	// is already enforced by the scope layer's capability table).
	if !h.node.APIDecide(id.Principal(), authz.ActionPresence, req.Channel).Allow {
		return nil, status.Error(codes.PermissionDenied, "presence denied by ACL rule")
	}

	presenceMap, err := h.node.Presence(ctx, req.Channel)
	if err != nil {
		return nil, err
	}

	clients := make(map[string]*serverv2.PresenceInfo, len(presenceMap))
	for id, info := range presenceMap {
		clients[id] = &serverv2.PresenceInfo{
			// SessionId falls back to the legacy client_id key so old
			// Redis records without the new field still report it.
			SessionId: firstNonEmpty(info.SessionID, info.ClientID),
			UserId:    info.UserID,
			// ClientId is the Connect.client_id (device endpoint), not the
			// session ID (D6 semantic fix).
			ClientId:    info.ConnectClientID,
			ConnectedAt: info.ConnectedAt,
		}
	}

	// Without presence.large_snapshot the Server API snapshot is truncated to the
	// channel policy cap like the client path; with the bit it stays full
	// (PR-KA-A4 §7). The bit follows the caller's identity.
	if id.Caps&authz.CapPresenceLargeSnapshot == 0 {
		limit := occupancy.MaxPresenceSnapshotClients
		if pol := h.node.ChannelPolicy(req.Channel); pol.PresenceSnapshotLimit > 0 {
			limit = pol.PresenceSnapshotLimit
		}
		if len(clients) > limit {
			keys := make([]string, 0, len(clients))
			for id := range clients {
				keys = append(keys, id)
			}
			sort.Strings(keys)
			for _, id := range keys[limit:] {
				delete(clients, id)
			}
		}
	}

	return &serverv2.GetPresenceResponse{Clients: clients}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (h *apiServiceHandler) GetHistory(ctx context.Context, req *serverv2.GetHistoryRequest) (*serverv2.GetHistoryResponse, error) {
	if err := h.scopeGetHistory(ctx, req); err != nil {
		return nil, err
	}
	id := identityFromScope(ctx)
	log.InfoContext(ctx, "server side API GetHistory", "channel", req.Channel, "since", req.Since, "limit", req.Limit)

	// The channel must allow Recover for the caller's principal (history.read
	// is already enforced by the scope layer's capability table): deny_all
	// and transient channels are rejected before the broker is touched.
	if !h.node.APIDecide(id.Principal(), authz.ActionRecover, req.Channel).Allow {
		return nil, status.Error(codes.PermissionDenied, "history denied by ACL rule")
	}

	// since is the resume position: nil reads from the head (within limit);
	// an offset-only position resumes from that offset. A non-empty
	// stream_epoch must match the broker's current epoch — a mismatch means
	// the caller's cursor belongs to a previous log generation.
	var sinceOffset uint64
	if since := req.Since; since != nil {
		if epoch := since.GetStreamEpoch(); epoch != "" {
			current := ""
			if epocher, ok := h.node.Broker().(interface{ Epoch() string }); ok {
				current = epocher.Epoch()
			}
			if current != epoch {
				return nil, status.Error(codes.FailedPrecondition, "stream epoch mismatch: history belongs to a previous log generation")
			}
		}
		sinceOffset = since.GetOffset()
	}

	page, err := h.node.Broker().History(req.Channel, sinceOffset, int(req.Limit))
	if err != nil {
		return nil, err
	}
	pubs := page.Pubs()

	result := make([]*serverv2.HistoryPublication, 0, len(pubs))
	for _, pub := range pubs {
		var metadata *sharedv2.Metadata
		if len(pub.Metadata) > 0 {
			metadata = &sharedv2.Metadata{Entries: pub.Metadata}
		}
		result = append(result, &serverv2.HistoryPublication{
			Position: &sharedv2.Position{StreamEpoch: pub.Epoch, Offset: &pub.Offset},
			Payload:  pub.PayloadProtoV2(),
			Time:     pub.Time,
			Id:       pub.Id,
			Metadata: metadata,
		})
	}

	return &serverv2.GetHistoryResponse{Publications: result}, nil
}

func (h *apiServiceHandler) GetChannels(ctx context.Context, req *serverv2.GetChannelsRequest) (*serverv2.GetChannelsResponse, error) {
	if err := h.scopeGetChannels(ctx, req); err != nil {
		return nil, err
	}
	id := identityFromScope(ctx)
	log.InfoContext(ctx, "server side API GetChannels")

	activeChannels, err := h.node.Channels(ctx)
	if err != nil {
		return nil, err
	}
	// Scoped identities only see channels inside their namespace scope;
	// channels that do not parse under the ns:topic grammar are hidden from
	// them too (fail-closed). Global identities see everything.
	filtered := identityIsGlobal(id)
	channels := make([]*serverv2.ChannelInfo, 0, len(activeChannels))
	for _, ch := range activeChannels {
		if !filtered {
			ns, err := topics.NamespaceOf(ch.Name)
			if err != nil || !id.AllowsNamespace(ns) {
				continue
			}
		}
		channels = append(channels, &serverv2.ChannelInfo{
			Name:        ch.Name,
			Subscribers: int32(ch.Subscribers),
		})
	}

	return &serverv2.GetChannelsResponse{Channels: channels}, nil
}

func payloadBytes(payload *sharedv2.Payload) ([]byte, error) {
	pub, err := stream.PublicationFromPayloadV2("", nil, payload)
	if err != nil {
		return nil, err
	}
	return pub.Payload, nil
}
