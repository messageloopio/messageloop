package runtime

import (
	"context"
	"testing"

	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// gapNoticesOf decodes every gap_notice envelope captured by the transport.
func gapNoticesOf(t *testing.T, transport *capturingTransport) []*clientpb.GapNotice {
	t.Helper()
	var notices []*clientpb.GapNotice
	for i := 0; i < transport.getMessageCount(); i++ {
		var out clientpb.OutboundMessage
		require.NoError(t, JSONMarshaler{}.Unmarshal(transport.getMessage(i), &out))
		if notice := out.GetGapNotice(); notice != nil {
			notices = append(notices, notice)
		}
	}
	return notices
}

// TestNode_OnGapFansOutGapNotice verifies C6: a catch-up gap on a channel is
// fanned out to every local session covered by it — exact subscribers and
// matching wildcard subscribers — as a GapNotice envelope carrying the
// channel, the wire gap reason, and the last known safe position. Unrelated
// subscribers receive nothing.
func TestNode_OnGapFansOutGapNotice(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(nil)
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))

	const ch = "gap.room.1"
	_, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	_, transportW := connectAndSubscribe(t, node, "client-w", &clientpb.Subscription{Channel: "gap.**"})
	_, transportX := connectAndSubscribe(t, node, "client-x", &clientpb.Subscription{Channel: "other.ch"})
	transportA.messages = nil
	transportW.messages = nil
	transportX.messages = nil

	node.onGap(CatchUpGap{Channel: ch, Reason: HistoryGapMiddle, LastGoodSeq: 2, LastGoodOffset: 12345})

	notices := gapNoticesOf(t, transportA)
	require.Len(t, notices, 1, "the exact subscriber must receive exactly one GapNotice")
	require.Equal(t, ch, notices[0].GetChannel())
	require.Equal(t, sharedv2.GapReason_GAP_REASON_MIDDLE, notices[0].GetGapReason())
	require.Equal(t, uint64(12345), notices[0].GetPosition().GetOffset())
	require.Equal(t, node.streamEpoch(), notices[0].GetPosition().GetStreamEpoch())

	require.Len(t, gapNoticesOf(t, transportW), 1, "a wildcard subscriber covering the channel must receive the notice")
	require.Empty(t, gapNoticesOf(t, transportX), "an unrelated subscriber must not receive the notice")
	require.Zero(t, publicationsOf(t, transportA), "a gap notice is never a publication")
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.LiveGapNoticeTotal.WithLabelValues("middle")))
}

// TestNode_OnGapReplayTruncated verifies C6: a truncated catch-up tail maps
// to GAP_REASON_REPLAY_TRUNCATED, and an unknown last-good offset leaves the
// position offset unset (never 0-means-unknown).
func TestNode_OnGapReplayTruncated(t *testing.T) {
	ctx := context.Background()
	node := NewNode(nil)
	require.NoError(t, node.Run(ctx))

	const ch = "gap.trunc"
	_, transportA := connectAndSubscribe(t, node, "client-a", &clientpb.Subscription{Channel: ch})
	transportA.messages = nil

	node.onGap(CatchUpGap{Channel: ch, Reason: HistoryGapReplayTruncated, LastGoodOffset: 777})

	notices := gapNoticesOf(t, transportA)
	require.Len(t, notices, 1)
	require.Equal(t, sharedv2.GapReason_GAP_REASON_REPLAY_TRUNCATED, notices[0].GetGapReason())
	require.Equal(t, uint64(777), notices[0].GetPosition().GetOffset())

	// An unknown last-good offset stays unset on the wire.
	transportA.messages = nil
	node.onGap(CatchUpGap{Channel: ch, Reason: HistoryGapReplayTruncated})
	notices = gapNoticesOf(t, transportA)
	require.Len(t, notices, 1)
	require.Nil(t, notices[0].GetPosition().Offset, "offset 0 = unknown must stay unset, not wire 0")
}

// TestNode_OnGapNoLocalSubscribers verifies C6: with no local subscribers the
// notice is neither sent nor counted (the broker's internal gap counter is
// unaffected and already ran).
func TestNode_OnGapNoLocalSubscribers(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(nil)
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))

	_, transportX := connectAndSubscribe(t, node, "client-x", &clientpb.Subscription{Channel: "other.ch"})
	transportX.messages = nil

	node.onGap(CatchUpGap{Channel: "gap.nobody", Reason: HistoryGapMiddle, LastGoodSeq: 1, LastGoodOffset: 1})

	require.Empty(t, gapNoticesOf(t, transportX))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.LiveGapNoticeTotal.WithLabelValues("middle")),
		"no fan-out, no notice metric")
}
