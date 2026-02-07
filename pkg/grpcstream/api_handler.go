package grpcstream

import (
	"context"

	cloudevents "github.com/cloudevents/sdk-go/binding/format/protobuf/v2/pb"
	"github.com/lynx-go/x/log"
	"github.com/messageloopio/messageloop"
	serverpb "github.com/messageloopio/messageloop/genproto/server/v1"
	clientpb "github.com/messageloopio/messageloop/genproto/v1"
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
		// Extract data from CloudEvent payload
		var data []byte
		if pub.Payload != nil {
			if binaryData := pub.Payload.GetBinaryData(); len(binaryData) > 0 {
				data = binaryData
			} else if textData := pub.Payload.GetTextData(); textData != "" {
				data = []byte(textData)
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

				// Create OutboundMessage with CloudEvent payload
				msg := &clientpb.Message{
					Channel: "", // Session-based, no channel
					Id:      pub.Id,
				}

				if pub.Payload != nil {
					msg.Payload = pub.Payload
				}

				out := messageloop.MakeOutboundMessage(nil, func(out *clientpb.OutboundMessage) {
					out.Envelope = &clientpb.OutboundMessage_Publication{
						Publication: &clientpb.Publication{
							Envelopes: []*clientpb.Message{msg},
						},
					}
				})

				if err := client.Send(ctx, out); err != nil {
					log.ErrorContext(ctx, "failed to send to session", "session_id", sessionID, "error", err)
				}
			}
		}

		// Channel-based publication
		if len(dest.Channels) > 0 {
			for _, channel := range dest.Channels {
				var publishOpts []messageloop.PublishOption
				// TODO: add_history option not yet implemented in broker
				if addHistory {
					log.DebugContext(ctx, "add_history option set but not yet implemented", "channel", channel)
				}
				if err := h.node.Publish(channel, data, publishOpts...); err != nil {
					log.ErrorContext(ctx, "failed to publish to channel", "channel", channel, "error", err)
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
			log.ErrorContext(ctx, "failed to disconnect session", "session_id", sessionID, "error", err)
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
		sub := messageloop.Subscriber{
			client:    client,
			ephemeral: false,
		}

		if err := h.node.AddSubscription(ctx, ch, sub); err != nil {
			results[ch] = false
			log.ErrorContext(ctx, "failed to subscribe to channel", "channel", ch, "error", err)
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
			log.ErrorContext(ctx, "failed to unsubscribe from channel", "channel", ch, "error", err)
		} else {
			results[ch] = true
		}
	}

	return &serverpb.UnsubscribeResponse{Results: results}, nil
}
