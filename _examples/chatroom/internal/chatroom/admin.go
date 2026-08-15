package chatroom

import (
	"context"
	"fmt"
	"time"

	serverpb "github.com/messageloopio/messageloop/shared/genproto/server/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// DefaultAdminAddr is where the demo server exposes its admin gRPC API.
const DefaultAdminAddr = "127.0.0.1:19091"

// AdminClient wraps the server-side gRPC admin API (server.grpc_admin.addr)
// with bearer-token authentication. The demo backend and the e2e runner use
// it to publish system messages, kick users, and inspect state.
type AdminClient struct {
	conn   *grpc.ClientConn
	client serverpb.APIServiceClient
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
		client: serverpb.NewAPIServiceClient(conn),
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
	req := &serverpb.PublishRequest{
		RequestId: "admin-" + id,
		Publications: []*serverpb.Publication{{
			Id: id,
			Destination: &serverpb.Publication_Destination{
				Channels: []string{channel},
			},
			Options: &serverpb.Publication_Options{AddHistory: addHistory},
			Payload: payload,
		}},
	}
	_, err = a.client.Publish(a.ctxAuth(ctx), req)
	return err
}

// DisconnectUser force-disconnects every session of a user (admin Disconnect).
func (a *AdminClient) DisconnectUser(ctx context.Context, userID string, code uint32, reason string) (map[string]bool, error) {
	resp, err := a.client.Disconnect(a.ctxAuth(ctx), &serverpb.DisconnectRequest{
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
func (a *AdminClient) Channels(ctx context.Context) ([]*serverpb.ChannelInfo, error) {
	resp, err := a.client.GetChannels(a.ctxAuth(ctx), &serverpb.GetChannelsRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Channels, nil
}

// Presence returns the presence snapshot of a channel (admin GetPresence).
func (a *AdminClient) Presence(ctx context.Context, channel string) (map[string]*serverpb.PresenceInfo, error) {
	resp, err := a.client.GetPresence(a.ctxAuth(ctx), &serverpb.GetPresenceRequest{Channel: channel})
	if err != nil {
		return nil, err
	}
	return resp.Clients, nil
}

// History returns the persisted history of a channel (admin GetHistory).
func (a *AdminClient) History(ctx context.Context, channel string, since uint64, limit int) ([]*serverpb.HistoryPublication, error) {
	resp, err := a.client.GetHistory(a.ctxAuth(ctx), &serverpb.GetHistoryRequest{
		Channel:    channel,
		SinceOffset: since,
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	return resp.Publications, nil
}

// JSONPayload builds a shared Payload holding the JSON-encoded chat message.
func JSONPayload(msg *ChatMessage) (*sharedpb.Payload, error) {
	jsonBytes, err := marshalJSON(msg)
	if err != nil {
		return nil, err
	}
	return &sharedpb.Payload{
		ContentType: "application/json",
		Data:        &sharedpb.Payload_Json{Json: mustStruct(jsonBytes)},
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
