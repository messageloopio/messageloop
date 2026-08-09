package grpcstream

import (
	"context"
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

	// add_history is not implemented; surface it explicitly instead of
	// silently ignoring the option.
	for _, pub := range req.Publications {
		if opts := pub.GetOptions(); opts != nil && opts.AddHistory {
			return nil, status.Error(codes.Unimplemented, "add_history is not implemented")
		}
	}

	// PublishResponse has no per-publication result fields, so failures are
	// reported with partial-success semantics: every failure is logged, and
	// when all publications fail the RPC returns an error.
	attempted := 0
	failed := 0
	for _, pub := range req.Publications {
		// Extract data from Payload
		var data []byte
		var isText bool
		if pub.Payload != nil {
			switch p := pub.Payload.Data.(type) {
			case *sharedpb.Payload_Binary:
				data = p.Binary
			case *sharedpb.Payload_Json:
				jsonData, err := messageloop.MarshalJSONStruct(p.Json)
				if err != nil {
					log.ErrorContext(ctx, "failed to marshal JSON payload", err, "publication_id", pub.Id)
					attempted++
					failed++
					continue
				}
				data = jsonData
				isText = true
			case *sharedpb.Payload_Text:
				data = []byte(p.Text)
				isText = true
			}
		}

		// Get destination
		dest := pub.GetDestination()
		if dest == nil || (len(dest.Sessions) == 0 && len(dest.Channels) == 0) {
			log.WarnContext(ctx, "publication has no destination", "publication_id", pub.Id)
			attempted++
			failed++
			continue
		}

		// Session-based publication
		for _, sessionID := range dest.Sessions {
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
			if _, err := h.node.Publish(channel, data, isText); err != nil {
				log.ErrorContext(ctx, "failed to publish to channel", err, "channel", channel)
				failed++
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
	log.InfoContext(ctx, "server side API Disconnect", "sessions", req.Sessions, "code", req.Code, "reason", req.Reason)

	results := make(map[string]bool)

	for _, sessionID := range req.Sessions {
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
	log.InfoContext(ctx, "server side API Subscribe", "session_id", req.SessionId, "channels", req.Channels)

	results := make(map[string]bool)

	for _, ch := range req.Channels {
		ok, err := h.node.SubscribeSession(ctx, req.SessionId, ch)
		if err != nil {
			results[ch] = false
			log.ErrorContext(ctx, "failed to subscribe to channel", err)
		} else {
			results[ch] = ok
		}
	}

	return &serverpb.SubscribeResponse{Results: results}, nil
}

func (h *apiServiceHandler) Unsubscribe(ctx context.Context, req *serverpb.UnsubscribeRequest) (*serverpb.UnsubscribeResponse, error) {
	log.InfoContext(ctx, "server side API Unsubscribe", "session_id", req.SessionId, "channels", req.Channels)

	results := make(map[string]bool)

	for _, ch := range req.Channels {
		ok, err := h.node.UnsubscribeSession(ctx, req.SessionId, ch)
		if err != nil {
			results[ch] = false
			log.ErrorContext(ctx, "failed to unsubscribe from channel", err)
		} else {
			results[ch] = ok
		}
	}

	return &serverpb.UnsubscribeResponse{Results: results}, nil
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
			ClientId:    info.ClientID,
			UserId:      info.UserID,
			ConnectedAt: info.ConnectedAt,
		}
	}

	return &serverpb.GetPresenceResponse{Clients: clients}, nil
}

func (h *apiServiceHandler) GetHistory(ctx context.Context, req *serverpb.GetHistoryRequest) (*serverpb.GetHistoryResponse, error) {
	log.InfoContext(ctx, "server side API GetHistory", "channel", req.Channel, "since_offset", req.SinceOffset, "limit", req.Limit)

	pubs, err := h.node.Broker().History(req.Channel, req.SinceOffset, int(req.Limit))
	if err != nil {
		return nil, err
	}

	result := make([]*serverpb.HistoryPublication, 0, len(pubs))
	for _, pub := range pubs {
		var payload *sharedpb.Payload
		if len(pub.Payload) > 0 {
			if pub.IsText {
				payload = &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: string(pub.Payload)}}
			} else {
				payload = &sharedpb.Payload{Data: &sharedpb.Payload_Binary{Binary: pub.Payload}}
			}
		}
		result = append(result, &serverpb.HistoryPublication{
			Offset:  pub.Offset,
			Payload: payload,
			IsText:  pub.IsText,
			Time:    pub.Time,
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
	if payload == nil {
		return nil, nil
	}
	switch data := payload.Data.(type) {
	case *sharedpb.Payload_Binary:
		return data.Binary, nil
	case *sharedpb.Payload_Json:
		return messageloop.MarshalJSONStruct(data.Json)
	case *sharedpb.Payload_Text:
		return []byte(data.Text), nil
	default:
		return nil, nil
	}
}
