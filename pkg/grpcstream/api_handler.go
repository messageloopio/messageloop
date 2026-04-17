package grpcstream

import (
	"context"

	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
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

	for _, pub := range req.Publications {
		// Extract data from Payload
		var data []byte
		var isText bool
		if pub.Payload != nil {
			switch p := pub.Payload.Data.(type) {
			case *sharedpb.Payload_Binary:
				data = p.Binary
			case *sharedpb.Payload_Json:
				data = []byte(p.Json.String())
				isText = true
			case *sharedpb.Payload_Text:
				data = []byte(p.Text)
				isText = true
			}
		}

		// Get destination
		dest := pub.GetDestination()
		if dest == nil {
			continue
		}

		// Get options
		opts := pub.GetOptions()
		addHistory := false
		if opts != nil {
			addHistory = opts.AddHistory
		}

		// Session-based publication
		if len(dest.Sessions) > 0 {
			for _, sessionID := range dest.Sessions {
				client := h.node.Hub().LookupSession(sessionID)
				if client == nil {
					log.DebugContext(ctx, "session not found, skipping", "session_id", sessionID)
					continue
				}

				// Create OutboundMessage with Payload
				msg := &clientpb.Message{
					Channel: "", // Session-based, no channel
					Id:      pub.Id,
					Payload: pub.Payload, // sharedpb.Payload is same type
				}

				out := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
					out.Envelope = &clientpb.OutboundMessage_Publication{
						Publication: &clientpb.Publication{
							Messages: []*clientpb.Message{msg},
						},
					}
				})

				if err := client.Send(ctx, out); err != nil {
					log.ErrorContext(ctx, "failed to send to session", err)
				}
			}
		}

		// Channel-based publication
		if len(dest.Channels) > 0 {
			for _, channel := range dest.Channels {
				if addHistory {
					log.DebugContext(ctx, "add_history option set but not yet implemented", "channel", channel)
				}
				if err := h.node.Publish(channel, data, isText); err != nil {
					log.ErrorContext(ctx, "failed to publish to channel", err)
				}
			}
		}
	}

	return &serverpb.PublishResponse{}, nil
}

func (h *apiServiceHandler) Disconnect(ctx context.Context, req *serverpb.DisconnectRequest) (*serverpb.DisconnectResponse, error) {
	log.InfoContext(ctx, "server side API Disconnect", "sessions", req.Sessions, "code", req.Code, "reason", req.Reason)

	results := make(map[string]bool)

	for _, sessionID := range req.Sessions {
		client := h.node.Hub().LookupSession(sessionID)
		if client == nil {
			results[sessionID] = false
			log.DebugContext(ctx, "session not found", "session_id", sessionID)
			continue
		}

		// Close the client with disconnect reason
		disconnect := messageloop.Disconnect{
			Code:   req.Code,
			Reason: req.Reason,
		}

		if err := client.Close(disconnect); err != nil {
			results[sessionID] = false
			log.ErrorContext(ctx, "failed to disconnect session", err)
		} else {
			results[sessionID] = true
		}
	}

	return &serverpb.DisconnectResponse{Results: results}, nil
}

func (h *apiServiceHandler) Subscribe(ctx context.Context, req *serverpb.SubscribeRequest) (*serverpb.SubscribeResponse, error) {
	log.InfoContext(ctx, "server side API Subscribe", "session_id", req.SessionId, "channels", req.Channels)

	results := make(map[string]bool)

	client := h.node.Hub().LookupSession(req.SessionId)
	if client == nil {
		// Session not found, all channels fail
		for _, ch := range req.Channels {
			results[ch] = false
		}
		return &serverpb.SubscribeResponse{Results: results}, nil
	}

	for _, ch := range req.Channels {
		// Create subscriber (non-ephemeral for server-side subscriptions)
		sub := messageloop.NewSubscriber(client, false)

		if err := h.node.AddSubscription(ctx, ch, sub); err != nil {
			results[ch] = false
			log.ErrorContext(ctx, "failed to subscribe to channel", err)
		} else {
			results[ch] = true
		}
	}

	return &serverpb.SubscribeResponse{Results: results}, nil
}

func (h *apiServiceHandler) Unsubscribe(ctx context.Context, req *serverpb.UnsubscribeRequest) (*serverpb.UnsubscribeResponse, error) {
	log.InfoContext(ctx, "server side API Unsubscribe", "session_id", req.SessionId, "channels", req.Channels)

	results := make(map[string]bool)

	client := h.node.Hub().LookupSession(req.SessionId)
	if client == nil {
		// Session not found, all channels fail
		for _, ch := range req.Channels {
			results[ch] = false
		}
		return &serverpb.UnsubscribeResponse{Results: results}, nil
	}

	for _, ch := range req.Channels {
		if err := h.node.RemoveSubscription(ch, client); err != nil {
			results[ch] = false
			log.ErrorContext(ctx, "failed to unsubscribe from channel", err)
		} else {
			results[ch] = true
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

	activeChannels := h.node.Hub().GetActiveChannels()
	channels := make([]*serverpb.ChannelInfo, 0, len(activeChannels))
	for _, ch := range activeChannels {
		channels = append(channels, &serverpb.ChannelInfo{
			Name:        ch.Name,
			Subscribers: int32(ch.Subscribers),
		})
	}

	return &serverpb.GetChannelsResponse{Channels: channels}, nil
}
