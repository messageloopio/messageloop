package admin

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

func (h *apiServiceHandler) Publish(ctx context.Context, req *serverv2.PublishRequest) (*serverv2.PublishResponse, error) {
	log.InfoContext(ctx, "server side API Publish", "request_id", req.RequestId)

	// Capability gates (PR-KA-A4 §7): per-session delivery needs session.act;
	// per-user expansion needs user.fanout on top. A missing bit fails the
	// whole RPC with PERMISSION_DENIED before any delivery.
	for _, pub := range req.Publications {
		dest := pub.GetDestination()
		if dest == nil {
			continue
		}
		if len(dest.Sessions) > 0 {
			if err := h.requireAdminCaps(authz.CapSessionAct, "publish to sessions"); err != nil {
				return nil, err
			}
		}
		if len(dest.Users) > 0 {
			if err := h.requireAdminCaps(authz.CapUserFanout|authz.CapSessionAct, "publish to users"); err != nil {
				return nil, err
			}
		}
	}

	// Empty user IDs inside destination.users are a client error: reject the
	// whole request before any scanning happens (anonymous connections are
	// never addressable by the user-based API).
	for _, pub := range req.Publications {
		for _, userID := range pub.GetDestination().GetUsers() {
			if userID == "" {
				return nil, status.Errorf(codes.InvalidArgument, "destination.users must not contain an empty user_id (publication %q)", pub.GetId())
			}
		}
	}

	// PublishResponse has no per-publication result fields, so failures are
	// reported with partial-success semantics: every failure is logged, and
	// when all publications fail the RPC returns an error.
	attempted := 0
	failed := 0
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
		for _, sessionID := range h.unionSessions(ctx, dest.Sessions, dest.Users, "publish") {
			attempted++
			// The admin wire payload is already shared.v2 — the same shape
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
			if !h.node.AdminCanPublish(channel) {
				log.WarnContext(ctx, "admin publish denied by ACL rule", "channel", channel)
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
					log.WarnContext(ctx, "admin add_history denied by channel policy", "channel", channel)
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
	log.InfoContext(ctx, "server side API Survey", "channel", req.Channel, "request_id", req.RequestId)

	// Without survey.bypass_gate the Admin survey runs through the same
	// gates as a client survey: the Survey decision (Effects.Survey +
	// allow_survey / deny_all) and the population cap (PR-KA-A4 §7). With
	// the bit, today's gate-free behavior is preserved.
	if h.node.AdminCapabilities()&authz.CapSurveyBypassGate == 0 {
		if !h.node.AdminDecide(authz.ActionSurvey, req.Channel).Allow {
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

	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
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
	log.InfoContext(ctx, "server side API Disconnect", "sessions", req.Sessions, "users", req.Users, "code", req.Code, "reason", req.Reason)

	// Capability gates (PR-KA-A4 §7).
	if len(req.Sessions) > 0 {
		if err := h.requireAdminCaps(authz.CapSessionAct, "disconnect sessions"); err != nil {
			return nil, err
		}
	}
	if len(req.Users) > 0 {
		if err := h.requireAdminCaps(authz.CapUserFanout|authz.CapSessionAct, "disconnect users"); err != nil {
			return nil, err
		}
	}

	for _, userID := range req.Users {
		if userID == "" {
			return nil, status.Error(codes.InvalidArgument, "users must not contain an empty user_id")
		}
	}

	results := make(map[string]bool)

	for _, sessionID := range h.unionSessions(ctx, req.Sessions, req.Users, "disconnect") {
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
	log.InfoContext(ctx, "server side API Subscribe", "session_id", req.SessionId, "user_id", req.UserId, "channels", req.Channels)

	if req.SessionId == "" && req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id must not both be empty")
	}
	// Capability gates (PR-KA-A4 §7): proxied subscription is a session act;
	// per-user expansion additionally needs user.fanout.
	if req.SessionId != "" {
		if err := h.requireAdminCaps(authz.CapSessionAct, "subscribe session"); err != nil {
			return nil, err
		}
	}
	if req.UserId != "" {
		if err := h.requireAdminCaps(authz.CapUserFanout|authz.CapSessionAct, "subscribe user"); err != nil {
			return nil, err
		}
	}

	sessions := h.unionSessions(ctx, []string{req.SessionId}, []string{req.UserId}, "subscribe")
	results := make(map[string]bool)

	for _, ch := range req.Channels {
		// With multiple sessions (user fan-out), any successful session wins
		// the channel's result: false only when every session failed.
		ok := false
		for _, sessionID := range sessions {
			subscribed, err := h.node.SubscribeSession(ctx, sessionID, ch)
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
	log.InfoContext(ctx, "server side API Unsubscribe", "session_id", req.SessionId, "user_id", req.UserId, "channels", req.Channels)

	if req.SessionId == "" && req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id must not both be empty")
	}
	// Capability gates (PR-KA-A4 §7).
	if req.SessionId != "" {
		if err := h.requireAdminCaps(authz.CapSessionAct, "unsubscribe session"); err != nil {
			return nil, err
		}
	}
	if req.UserId != "" {
		if err := h.requireAdminCaps(authz.CapUserFanout|authz.CapSessionAct, "unsubscribe user"); err != nil {
			return nil, err
		}
	}

	sessions := h.unionSessions(ctx, []string{req.SessionId}, []string{req.UserId}, "unsubscribe")
	results := make(map[string]bool)

	for _, ch := range req.Channels {
		// With multiple sessions (user fan-out), any successful session wins
		// the channel's result: false only when every session failed.
		ok := false
		for _, sessionID := range sessions {
			unsubscribed, err := h.node.UnsubscribeSession(ctx, sessionID, ch)
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

// unionSessions expands the users list into session IDs (via the node's
// user index plus the local hub) and unions them with the explicit session
// list, deduplicated and sorted for deterministic execution order. Empty
// user IDs must have been rejected by the caller. The per-user fan-out
// metric is observed with the given op label.
func (h *apiServiceHandler) unionSessions(ctx context.Context, explicit []string, users []string, op string) []string {
	seen := make(map[string]struct{}, len(explicit)+len(users))
	for _, sessionID := range explicit {
		if sessionID == "" {
			continue
		}
		seen[sessionID] = struct{}{}
	}
	for _, userID := range users {
		expanded := h.node.ExpandUserSessions(ctx, userID)
		h.node.ObserveAdminUserFanout(op, len(expanded))
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
	log.InfoContext(ctx, "server side API GetPresence", "channel", req.Channel)

	// Capability + Decide gates (PR-KA-A4 §7): presence.read is required and
	// the channel must be allowed for the admin principal.
	if err := h.requireAdminCaps(authz.CapPresenceRead, "GetPresence"); err != nil {
		return nil, err
	}
	if !h.node.AdminDecide(authz.ActionPresence, req.Channel).Allow {
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

	// Without presence.large_snapshot the admin snapshot is truncated to the
	// channel policy cap like the client path; with the bit it stays full
	// (PR-KA-A4 §7).
	if h.node.AdminCapabilities()&authz.CapPresenceLargeSnapshot == 0 {
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
	log.InfoContext(ctx, "server side API GetHistory", "channel", req.Channel, "since", req.Since, "limit", req.Limit)

	// Capability + Decide gates (PR-KA-A4 §7): history.read is required and
	// the channel must allow Recover for the admin principal (Effects.Recover
	// plus deny_all; transient channels are rejected). A missing bit must not
	// touch the broker at all.
	if err := h.requireAdminCaps(authz.CapHistoryRead, "GetHistory"); err != nil {
		return nil, err
	}
	if !h.node.AdminDecide(authz.ActionRecover, req.Channel).Allow {
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
	log.InfoContext(ctx, "server side API GetChannels")

	// Capability gate (PR-KA-A4 §7): channels.list is required.
	if err := h.requireAdminCaps(authz.CapChannelsList, "GetChannels"); err != nil {
		return nil, err
	}

	activeChannels, err := h.node.Channels(ctx)
	if err != nil {
		return nil, err
	}
	channels := make([]*serverv2.ChannelInfo, 0, len(activeChannels))
	for _, ch := range activeChannels {
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

// requireAdminCaps fails the RPC with PERMISSION_DENIED when the configured
// admin capabilities miss any of the required bits (PR-KA-A4 §7). Missing
// bits fail softly; nothing is read or written.
func (h *apiServiceHandler) requireAdminCaps(bits authz.Capability, what string) error {
	if h.node.AdminCapabilities()&bits != bits {
		return status.Errorf(codes.PermissionDenied, "%s requires admin capability", what)
	}
	return nil
}
