package messageloopgo

import (
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// GapNotice is a server notification that reconnect catch-up detected a hole
// in a channel's delivery (C6): a middle hole (dense-seq discontinuity) or a
// truncated replay tail. It is a notification only — it never enters the
// message stream, never advances the per-channel cursor, and carries the
// last known safe position so the client can recover on its own.
type GapNotice struct {
	// Channel is the exact channel the gap was detected on.
	Channel string
	// GapReason is the wire gap reason (e.g. GAP_REASON_MIDDLE,
	// GAP_REASON_REPLAY_TRUNCATED).
	GapReason sharedv2.GapReason
	// StreamEpoch is the broker epoch of Position.
	StreamEpoch string
	// Offset is the last known safe position; OffsetSet reports whether the
	// server knew one (unknown positions carry no offset, never 0).
	Offset    uint64
	OffsetSet bool
}

// gapNoticeFromPB converts a protocol GapNotice to the SDK type.
func gapNoticeFromPB(n *clientpb.GapNotice) GapNotice {
	out := GapNotice{
		Channel:     n.GetChannel(),
		GapReason:   n.GetGapReason(),
		StreamEpoch: n.GetPosition().GetStreamEpoch(),
	}
	if off, set := posOffset(n.GetPosition()); set {
		out.Offset = off
		out.OffsetSet = true
	}
	return out
}
