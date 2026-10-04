package shared

import (
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// PositionFrom builds the client-wire Position for the recover/delivery
// contract: the offset is only set when set == true; otherwise it stays
// unset (transient / fresh / unknown), never 0-means-unset (KD-K22).
//
// This is the single implementation. The recover module, the session
// delivery paths and the hub's broadcast frames all construct Positions
// through it — before it existed, byte-identical copies lived in
// internal/runtime/recover.go and internal/session/runtime.go and had to be
// kept in sync by comment.
func PositionFrom(epoch string, offset uint64, set bool) *sharedv2.Position {
	p := &sharedv2.Position{StreamEpoch: epoch}
	if set {
		off := offset
		p.Offset = &off
	}
	return p
}
