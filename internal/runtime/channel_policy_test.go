package runtime

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v2"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func policyBoolPtr(v bool) *bool { return &v }
func policyIntPtr(v int) *int    { return &v }

// TestNodePublish_HistoryDisabled verifies Node.Publish refuses channels
// whose Effects disable history, for both transient_only and history=false.
func TestNodePublish_HistoryDisabled(t *testing.T) {
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
				{Pattern: "no-history.**", ChannelPolicySpec: config.ChannelPolicySpec{History: policyBoolPtr(false)}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))

	_, err := node.Publish("game.tick.fps", publishPub([]byte("tick"), false))
	require.ErrorIs(t, err, ErrHistoryDisabled)

	_, err = node.Publish("no-history.ch", publishPub([]byte("nope"), false))
	require.ErrorIs(t, err, ErrHistoryDisabled)

	// A channel without the restriction still publishes.
	offset, err := node.Publish("im.room.1", publishPub([]byte("hello"), false))
	require.NoError(t, err)
	require.NotZero(t, offset)
}

// TestNodePublish_HistorySizeInjected verifies Node.Publish fills the
// publication's HistorySize/HistoryTTL from the Effects when the caller left
// them zero.
func TestNodePublish_HistorySizeInjected(t *testing.T) {
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "im.**", ChannelPolicySpec: config.ChannelPolicySpec{
					HistorySize: policyIntPtr(5000),
					HistoryTTL:  "48h",
				}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))

	pub := publishPub([]byte("hello"), false)
	_, err := node.Publish("im.room.1", pub)
	require.NoError(t, err)
	require.Equal(t, 5000, pub.HistorySize)
	require.Equal(t, 48*time.Hour, pub.HistoryTTL)

	// A caller-supplied value wins over the policy.
	pub2 := publishPub([]byte("hello"), false)
	pub2.HistorySize = 7
	pub2.HistoryTTL = 7 * time.Hour
	_, err = node.Publish("im.room.1", pub2)
	require.NoError(t, err)
	require.Equal(t, 7, pub2.HistorySize)
	require.Equal(t, 7*time.Hour, pub2.HistoryTTL)
}

// TestHandlePublish_PolicyForcesTransient verifies the client publish path
// on a policy-forced-transient channel: the client sends a normal publish
// (no transient flag), receives an ack with offset 0, nothing is written to
// history, subscribers still receive the message in real time, and the
// transient-forced metric is incremented.
func TestHandlePublish_PolicyForcesTransient(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))

	// A subscriber on the tick channel.
	subTransport := &capturingTransport{}
	subscriber, _, err := NewClient(ctx, node, subTransport, JSONMarshaler{})
	require.NoError(t, err)
	subscriber.MarkAuthenticated()
	require.NoError(t, subscriber.Attach(subscriber.Attachment()))
	require.NoError(t, node.AddClient(subscriber))
	require.NoError(t, node.AddSubscription(ctx, "game.tick.fps", Subscriber{Session: subscriber, Ephemeral: false}))
	subTransport.messages = nil

	// A publisher client, authenticated via Connect.
	pubTransport := &capturingTransport{}
	publisher, _, err := NewClient(ctx, node, pubTransport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, publisher.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: testProtocolVersion}},
	}))
	pubTransport.messages = nil

	// Normal publish, no transient flag.
	require.NoError(t, publisher.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "pub-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel: "game.tick.fps",
				Payload: &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "tick payload"}},
			},
		},
	}))

	// Ack must carry offset 0 (no history entry).
	require.Equal(t, 1, pubTransport.getMessageCount(), "exactly one ack expected")
	var ack clientpb.OutboundMessage
	require.NoError(t, JSONMarshaler{}.Unmarshal(pubTransport.getLastMessage(), &ack))
	pubAck := ack.GetPublishAck()
	require.NotNil(t, pubAck, "envelope must be PublishAck")
	assert.Equal(t, "pub-1", pubAck.GetId())
	off, set := posOffset(pubAck.GetPosition())
	assert.False(t, set, "transient publish must ack with an unset position")
	assert.Equal(t, uint64(0), off)

	// Nothing written to history.
	page, err := node.Broker().History("game.tick.fps", 0, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs(), "policy-forced transient must not write history")

	// The subscriber still receives the tick in real time.
	require.Eventually(t, func() bool {
		subTransport.mu.Lock()
		defer subTransport.mu.Unlock()
		for _, raw := range subTransport.messages {
			if bytes.Contains(raw, []byte("tick payload")) {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond, "subscriber must receive the forced-transient tick")

	// The metric must count the forced conversion.
	assert.Equal(t, float64(1), testutil.ToFloat64(metrics.ChannelPolicyTransientForced))
}

// TestHandlePublish_PolicyForcedNoMetricForDeclaredTransient verifies the
// metric only counts publications the client did NOT declare transient.
func TestHandlePublish_PolicyForcedNoMetricForDeclaredTransient(t *testing.T) {
	ctx := context.Background()
	metrics := NewMetrics(prometheus.NewRegistry())
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	node.SetMetrics(metrics)
	require.NoError(t, node.Run(ctx))

	pubTransport := &capturingTransport{}
	publisher, _, err := NewClient(ctx, node, pubTransport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, publisher.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{Version: testProtocolVersion}},
	}))

	require.NoError(t, publisher.HandleMessage(ctx, &clientpb.InboundMessage{
		Id: "pub-1",
		Envelope: &clientpb.InboundMessage_Publish{
			Publish: &clientpb.Publish{
				Channel:   "game.tick.fps",
				Transient: true,
				Payload:   &sharedpb.Payload{Data: &sharedpb.Payload_Text{Text: "tick"}},
			},
		},
	}))

	assert.Equal(t, float64(0), testutil.ToFloat64(metrics.ChannelPolicyTransientForced),
		"a client-declared transient publish is not 'forced'")
}

// TestNodePublish_PolicyHistorySizeCapsRing verifies the §9 acceptance item:
// with im.** history_size=5 and 10 publishes on a fresh channel, History
// keeps exactly the last 5 entries.
func TestNodePublish_PolicyHistorySizeCapsRing(t *testing.T) {
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "im.**", ChannelPolicySpec: config.ChannelPolicySpec{HistorySize: policyIntPtr(5)}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))

	for i := 0; i < 10; i++ {
		offset, err := node.Publish("im.room.1", publishPub([]byte(fmt.Sprintf("m-%d", i)), false))
		require.NoError(t, err)
		require.Equal(t, uint64(i+1), offset)
	}
	page, err := node.Broker().History("im.room.1", 0, 0)
	require.NoError(t, err)
	history := page.Pubs()
	require.Len(t, history, 5, "policy history_size=5 must cap the fresh channel ring")
	require.Equal(t, "m-5", string(history[0].Payload))
	require.Equal(t, "m-9", string(history[4].Payload))
}

// TestNodePublish_PolicyDisabledHistoryStillTransientable verifies that
// ErrHistoryDisabled only affects Node.Publish: PublishTransient remains
// available on the same channel.
func TestNodePublish_PolicyDisabledHistoryStillTransientable(t *testing.T) {
	node := NewNode(&config.Server{
		Authorizer: config.AuthorizerConfig{
			Rules: []config.AuthorizerRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))

	require.NoError(t, node.PublishTransient("game.tick.fps", publishPub([]byte("tick"), false)))
}
