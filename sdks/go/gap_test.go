package messageloopgo

import (
	"context"
	"testing"
	"time"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
)

// TestSDK_GapNoticeDispatchesWithoutTouchingCursor verifies C6: an inbound
// GapNotice envelope reaches the OnGapNotice handler with the channel,
// reason, and last-known-safe position mapped, never triggers the error
// handler, and never advances the per-channel recovery cursor.
func TestSDK_GapNoticeDispatchesWithoutTouchingCursor(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	got := make(chan GapNotice, 1)
	c.OnGapNotice(func(n GapNotice) { got <- n })

	errs := make(chan error, 1)
	c.OnError(func(err error) { errs <- err })

	// Seed a cursor the notice must not touch.
	c.offsetMu.Lock()
	c.channelOffsets["gap.ch"] = 42
	c.offsetMu.Unlock()

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_GapNotice{
			GapNotice: &clientpb.GapNotice{
				Channel:   "gap.ch",
				GapReason: sharedv2.GapReason_GAP_REASON_MIDDLE,
				Position:  Position("ep", 41),
			},
		},
	}, 0)

	select {
	case n := <-got:
		if n.Channel != "gap.ch" {
			t.Fatalf("GapNotice.Channel = %q, want gap.ch", n.Channel)
		}
		if n.GapReason != sharedv2.GapReason_GAP_REASON_MIDDLE {
			t.Fatalf("GapNotice.GapReason = %v, want GAP_REASON_MIDDLE", n.GapReason)
		}
		if !n.OffsetSet || n.Offset != 41 || n.StreamEpoch != "ep" {
			t.Fatalf("GapNotice position = (%q, %d, %v), want (ep, 41, true)", n.StreamEpoch, n.Offset, n.OffsetSet)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnGapNotice was not called for the gap notice")
	}

	select {
	case err := <-errs:
		t.Fatalf("OnError must not fire for a gap notice, got %v", err)
	default:
	}

	c.offsetMu.RLock()
	off := c.channelOffsets["gap.ch"]
	c.offsetMu.RUnlock()
	if off != 42 {
		t.Fatalf("channel offset = %d, want 42 (a gap notice never advances the cursor)", off)
	}
}

// TestSDK_GapNoticeWithoutHandlerIsIgnored verifies C6: a gap notice with no
// registered handler is silently ignored — no error, no panic, and an unset
// position offset never becomes 0-means-set.
func TestSDK_GapNoticeWithoutHandlerIsIgnored(t *testing.T) {
	trans := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := newClient(ctx, cancel, trans, defaultOptions())

	errs := make(chan error, 1)
	c.OnError(func(err error) { errs <- err })

	c.handleMessage(&clientpb.OutboundMessage{
		Envelope: &clientpb.OutboundMessage_GapNotice{
			GapNotice: &clientpb.GapNotice{
				Channel:   "gap.ch",
				GapReason: sharedv2.GapReason_GAP_REASON_REPLAY_TRUNCATED,
				Position:  &sharedv2.Position{StreamEpoch: "ep"},
			},
		},
	}, 0)

	select {
	case err := <-errs:
		t.Fatalf("OnError must not fire for a gap notice, got %v", err)
	default:
	}

	// The converter maps an unset offset to OffsetSet=false.
	n := gapNoticeFromPB(&clientpb.GapNotice{Position: &sharedv2.Position{StreamEpoch: "ep"}})
	if n.OffsetSet || n.Offset != 0 {
		t.Fatalf("unset offset mapped to (%d, %v), want (0, false)", n.Offset, n.OffsetSet)
	}
}
