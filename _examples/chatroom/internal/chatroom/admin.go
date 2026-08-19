package chatroom

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	serverv2 "github.com/messageloopio/messageloop/shared/genproto/server/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// DefaultAdminAddr is where the demo server exposes its admin gRPC API.
const DefaultAdminAddr = "127.0.0.1:19091"

// AdminClient wraps the server-side gRPC admin API (server.grpc_admin.addr)
// with bearer-token authentication. The demo backend and the e2e runner use
// it to publish system messages, kick users, and inspect state.
type AdminClient struct {
	conn   *grpc.ClientConn
	client serverv2.APIServiceClient
	token  string
}

// NewAdminClient dials the admin API with the configured auth token.
func NewAdminClient(ctx context.Context, addr, token string) (*AdminClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial admin %s: %w", addr, err)
	}
	return &AdminClient{
		conn:   conn,
		client: serverv2.NewAPIServiceClient(conn),
		token:  token,
	}, nil
}

// Close releases the underlying connection.
func (a *AdminClient) Close() error {
	return a.conn.Close()
}

// ctxAuth returns a context carrying the admin bearer token.
func (a *AdminClient) ctxAuth(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+a.token)
}

// PublishToChannel publishes one JSON payload to a channel and, when
// addHistory is true, persists it into the channel history (admin Publish).
func (a *AdminClient) PublishToChannel(ctx context.Context, channel, id string, msg *ChatMessage, addHistory bool) error {
	payload, err := JSONPayload(msg)
	if err != nil {
		return err
	}
	req := &serverv2.PublishRequest{
		RequestId: "admin-" + id,
		Publications: []*serverv2.Publication{{
			Id: id,
			Destination: &serverv2.Publication_Destination{
				Channels: []string{channel},
			},
			Options: &serverv2.Publication_Options{AddHistory: addHistory},
			Payload: payload,
		}},
	}
	_, err = a.client.Publish(a.ctxAuth(ctx), req)
	return err
}

// DisconnectUser force-disconnects every session of a user (admin Disconnect).
func (a *AdminClient) DisconnectUser(ctx context.Context, userID string, code uint32, reason string) (map[string]bool, error) {
	resp, err := a.client.Disconnect(a.ctxAuth(ctx), &serverv2.DisconnectRequest{
		Users:  []string{userID},
		Code:   code,
		Reason: reason,
	})
	if err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// Channels lists the currently active channels (admin GetChannels).
func (a *AdminClient) Channels(ctx context.Context) ([]*serverv2.ChannelInfo, error) {
	resp, err := a.client.GetChannels(a.ctxAuth(ctx), &serverv2.GetChannelsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Channels, nil
}

// Presence returns the presence snapshot of a channel (admin GetPresence).
func (a *AdminClient) Presence(ctx context.Context, channel string) (map[string]*serverv2.PresenceInfo, error) {
	resp, err := a.client.GetPresence(a.ctxAuth(ctx), &serverv2.GetPresenceRequest{Channel: channel})
	if err != nil {
		return nil, err
	}
	return resp.Clients, nil
}

// History returns the persisted history of a channel (admin GetHistory).
// A since offset of 0 reads from the head; a positive offset resumes from it.
func (a *AdminClient) History(ctx context.Context, channel string, since uint64, limit int) ([]*serverv2.HistoryPublication, error) {
	var sincePos *sharedv2.Position
	if since > 0 {
		sincePos = &sharedv2.Position{Offset: &since}
	}
	resp, err := a.client.GetHistory(a.ctxAuth(ctx), &serverv2.GetHistoryRequest{
		Channel: channel,
		Since:   sincePos,
		Limit:   int32(limit),
	})
	if err != nil {
		return nil, err
	}
	return resp.Publications, nil
}

// JSONPayload builds a shared Payload holding the JSON-encoded chat message.
func JSONPayload(msg *ChatMessage) (*sharedv2.Payload, error) {
	jsonBytes, err := marshalJSON(msg)
	if err != nil {
		return nil, err
	}
	return &sharedv2.Payload{
		ContentType: "application/json",
		Data:        &sharedv2.Payload_Json{Json: mustStruct(jsonBytes)},
	}, nil
}

// WaitFor polls cond until it returns true or the timeout expires.
func WaitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}
