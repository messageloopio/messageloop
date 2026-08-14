package messageloopgo

import (
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
)

// PresenceInfo describes a single presence entry on a channel.
type PresenceInfo struct {
	// SessionID is the session ID of the present client.
	SessionID string
	// UserID is the authenticated user ID, empty when the server did not
	// resolve one.
	UserID string
	// ClientID is the Connect client_id (device/app instance), not the
	// session ID.
	ClientID string
	// ConnectedAt is the Unix millisecond timestamp of the client's connect.
	ConnectedAt int64
}

// PresenceEvent is a join/leave notification for a channel.
type PresenceEvent struct {
	// Channel is always the exact channel the event belongs to.
	Channel string
	// Action is "join" or "leave". Unknown actions are still delivered.
	Action string
	// Info carries the presence entry of the joining/leaving client.
	Info PresenceInfo
}

// PresenceSnapshot is a point-in-time presence view of a channel.
type PresenceSnapshot struct {
	// Channel is the exact channel the snapshot belongs to.
	Channel string
	// Clients lists the present clients, capped by the server policy; when
	// the cap was hit, Truncated is true and Occupancy still counts everyone.
	Clients []PresenceInfo
	// Truncated reports whether the clients list was capped.
	Truncated bool
	// Occupancy is the full member count of the channel.
	Occupancy int32
}

// presenceInfoFromPB converts a protocol PresenceInfo to the SDK type.
func presenceInfoFromPB(info *clientpb.PresenceInfo) PresenceInfo {
	if info == nil {
		return PresenceInfo{}
	}
	return PresenceInfo{
		SessionID:   info.GetSessionId(),
		UserID:      info.GetUserId(),
		ClientID:    info.GetClientId(),
		ConnectedAt: info.GetConnectedAt(),
	}
}

// presenceEventFromPB converts a protocol PresenceEvent to the SDK type.
func presenceEventFromPB(ev *clientpb.PresenceEvent) PresenceEvent {
	out := PresenceEvent{
		Channel: ev.GetChannel(),
		Action:  ev.GetAction(),
	}
	if ev.GetInfo() != nil {
		out.Info = presenceInfoFromPB(ev.GetInfo())
	}
	return out
}

// presenceSnapshotFromPB converts a protocol PresenceSnapshot to the SDK type.
func presenceSnapshotFromPB(snap *clientpb.PresenceSnapshot) PresenceSnapshot {
	out := PresenceSnapshot{
		Channel:   snap.GetChannel(),
		Truncated: snap.GetTruncated(),
		Occupancy: snap.GetOccupancy(),
		Clients:   make([]PresenceInfo, 0, len(snap.GetClients())),
	}
	for _, c := range snap.GetClients() {
		out.Clients = append(out.Clients, presenceInfoFromPB(c))
	}
	return out
}
