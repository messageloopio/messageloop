package grpcstream

import (
	"context"
	"sort"
	"time"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type apiServiceHandler struct {
	serverpb.UnimplementedAPIServiceServer
	node *messageloop.Node
}

func NewAPIServiceHandler(node *messageloop.Node) serverpb.APIServiceServer {
	return &apiServiceHandler{node: node}
}

func (h *apiServiceHandler) Publish(ctx context.Context, req *serverpb.PublishRequest) (*serverpb.PublishResponse, error) {
	log.InfoContext(ctx, "server side API Publish", "request_id", req.RequestId)

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
		brokerPub, err := messageloop.PublicationFromPayload(pub.Id, pub.GetMetadata().GetEntries(), pub.GetPayload())
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
			// Create OutboundMessage with Payload
			msg := &clientpb.Message{
				Channel: "", // Session-based, no channel
				Id:      pub.Id,
				Payload: pub.Payload, // sharedpb.Payload is same type
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
	return &serverpb.PublishResponse{}, nil
}

func (h *apiServiceHandler) Survey(ctx context.Context, req *serverpb.SurveyRequest) (*serverpb.SurveyResponse, error) {
	log.InfoContext(ctx, "server side API Survey", "channel", req.Channel, "request_id", req.RequestId)

	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	payload, err := payloadBytes(req.Payload)
	if err != nil {
		return nil, err
	}
	results, err := h.node.Survey(ctx, req.Channel, payload, timeout)
	if err != nil {
		return nil, err
	}

	response := &serverpb.SurveyResponse{
		RequestId: req.RequestId,
		Results:   make([]*serverpb.SurveyResult, 0, len(results)),
	}
	for _, result := range results {
		item := &serverpb.SurveyResult{SessionId: result.SessionID}
		if len(result.Payload) > 0 {
			item.Payload = &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: result.Payload}}
		}
		metadata := make(map[string]string)
		if result.NodeID != "" {
			metadata["node_id"] = result.NodeID
		}
		if result.IncarnationID != "" {
			metadata["incarnation_id"] = result.IncarnationID
		}
		if len(metadata) > 0 {
			item.Metadata = &sharedpb.Metadata{Entries: metadata}
		}
		if result.Error != nil {
			item.Error = &sharedpb.Error{Code: "SURVEY_FAILED", Message: result.Error.Error()}
		}
		response.Results = append(response.Results, item)
	}

	return response, nil
}

func (h *apiServiceHandler) Disconnect(ctx context.Context, req *serverpb.DisconnectRequest) (*serverpb.DisconnectResponse, error) {
	log.InfoContext(ctx, "server side API Disconnect", "sessions", req.Sessions, "users", req.Users, "code", req.Code, "reason", req.Reason)

	for _, userID := range req.Users {
		if userID == "" {
			return nil, status.Error(codes.InvalidArgument, "users must not contain an empty user_id")
		}
	}

	results := make(map[string]bool)

	for _, sessionID := range h.unionSessions(ctx, req.Sessions, req.Users, "disconnect") {
		// Close the client with disconnect reason
		disconnect := messageloop.Disconnect{
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

	return &serverpb.DisconnectResponse{Results: results}, nil
}

func (h *apiServiceHandler) Subscribe(ctx context.Context, req *serverpb.SubscribeRequest) (*serverpb.SubscribeResponse, error) {
	log.InfoContext(ctx, "server side API Subscribe", "session_id", req.SessionId, "user_id", req.UserId, "channels", req.Channels)

	if req.SessionId == "" && req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id must not both be empty")
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

	return &serverpb.SubscribeResponse{Results: results}, nil
}

func (h *apiServiceHandler) Unsubscribe(ctx context.Context, req *serverpb.UnsubscribeRequest) (*serverpb.UnsubscribeResponse, error) {
	log.InfoContext(ctx, "server side API Unsubscribe", "session_id", req.SessionId, "user_id", req.UserId, "channels", req.Channels)

	if req.SessionId == "" && req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id and user_id must not both be empty")
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

	return &serverpb.UnsubscribeResponse{Results: results}, nil
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

func (h *apiServiceHandler) GetPresence(ctx context.Context, req *serverpb.GetPresenceRequest) (*serverpb.GetPresenceResponse, error) {
	log.InfoContext(ctx, "server side API GetPresence", "channel", req.Channel)

	presenceMap, err := h.node.Presence(ctx, req.Channel)
	if err != nil {
		return nil, err
	}

	clients := make(map[string]*serverpb.PresenceInfo, len(presenceMap))
	for id, info := range presenceMap {
		clients[id] = &serverpb.PresenceInfo{
			ClientId:        info.ClientID,
			UserId:          info.UserID,
			ConnectedAt:     info.ConnectedAt,
			// SessionId falls back to the legacy client_id key so old
			// Redis records without the new field still report it.
			SessionId:       firstNonEmpty(info.SessionID, info.ClientID),
			ConnectClientId: info.ConnectClientID,
		}
	}

	return &serverpb.GetPresenceResponse{Clients: clients}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (h *apiServiceHandler) GetHistory(ctx context.Context, req *serverpb.GetHistoryRequest) (*serverpb.GetHistoryResponse, error) {
	log.InfoContext(ctx, "server side API GetHistory", "channel", req.Channel, "since_offset", req.SinceOffset, "limit", req.Limit)

	page, err := h.node.Broker().History(req.Channel, req.SinceOffset, int(req.Limit))
	if err != nil {
		return nil, err
	}
	pubs := page.Pubs()

	result := make([]*serverpb.HistoryPublication, 0, len(pubs))
	for _, pub := range pubs {
		metadata := pub.Metadata
		if len(metadata) == 0 {
			metadata = nil
		}
		result = append(result, &serverpb.HistoryPublication{
			Offset:   pub.Offset,
			Payload:  pub.PayloadProto(),
			IsText:   pub.Kind == messageloop.PayloadKindText || pub.Kind == messageloop.PayloadKindJSON,
			Time:     pub.Time,
			Id:       pub.Id,
			Metadata: metadata,
		})
	}

	return &serverpb.GetHistoryResponse{Publications: result}, nil
}

func (h *apiServiceHandler) GetChannels(ctx context.Context, req *serverpb.GetChannelsRequest) (*serverpb.GetChannelsResponse, error) {
	log.InfoContext(ctx, "server side API GetChannels")

	activeChannels, err := h.node.Channels(ctx)
	if err != nil {
		return nil, err
	}
	channels := make([]*serverpb.ChannelInfo, 0, len(activeChannels))
	for _, ch := range activeChannels {
		channels = append(channels, &serverpb.ChannelInfo{
			Name:        ch.Name,
			Subscribers: int32(ch.Subscribers),
		})
	}

	return &serverpb.GetChannelsResponse{Channels: channels}, nil
}

func payloadBytes(payload *sharedpb.Payload) ([]byte, error) {
	pub, err := messageloop.PublicationFromPayload("", nil, payload)
	if err != nil {
		return nil, err
	}
	return pub.Payload, nil
}
