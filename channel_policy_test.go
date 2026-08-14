package messageloop

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/messageloopio/messageloop/config"
	clientpb "github.com/messageloopio/messageloop/shared/genproto/client/v1"
	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func policyBoolPtr(v bool) *bool { return &v }
func policyIntPtr(v int) *int    { return &v }

func TestChannelPolicy_DefaultWhenEmpty(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{})
	require.NoError(t, err)

	pol := engine.For("any.channel")
	assert.True(t, pol.History)
	assert.True(t, pol.Presence)
	assert.True(t, pol.Recover)
	assert.False(t, pol.Survey)
	assert.False(t, pol.TransientOnly)
	assert.Equal(t, 256, pol.MaxSurveySubscribers)
	assert.Equal(t, 5*time.Second, pol.MaxSurveyTimeout)
	assert.Equal(t, 256, pol.PresenceSnapshotLimit)
}

// TestChannelPolicy_DefaultSpecOverridesDefault pins the YAML default spec
// as the base overlay: e.g. default.history=false applies to every channel
// that matches no policy rule.
func TestChannelPolicy_DefaultSpecOverridesDefault(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{
		Default: config.ChannelPolicySpec{History: policyBoolPtr(false), Survey: policyBoolPtr(true)},
	})
	require.NoError(t, err)

	pol := engine.For("unmatched.channel")
	assert.False(t, pol.History)
	assert.True(t, pol.Survey)
}

func TestChannelPolicy_FirstMatchWins(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{
			{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			{Pattern: "game.**", ChannelPolicySpec: config.ChannelPolicySpec{History: policyBoolPtr(true), Survey: policyBoolPtr(true)}},
		},
	})
	require.NoError(t, err)

	// game.tick.fps must hit the first (more specific) rule, not the
	// later game.** rule.
	tick := engine.For("game.tick.fps")
	assert.True(t, tick.TransientOnly)
	assert.False(t, tick.History)
	assert.False(t, tick.Survey)

	// game.room.1 matches only game.**.
	room := engine.For("game.room.1")
	assert.False(t, room.TransientOnly)
	assert.True(t, room.History)
	assert.True(t, room.Survey)

	// No match at all resolves to the compiled default.
	other := engine.For("im.room.1")
	assert.False(t, other.TransientOnly)
	assert.True(t, other.History)
	assert.False(t, other.Survey)
}

func TestChannelPolicy_OverlayNilKeepsDefault(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{
			{Pattern: "im.**", ChannelPolicySpec: config.ChannelPolicySpec{HistorySize: policyIntPtr(5)}},
		},
	})
	require.NoError(t, err)

	pol := engine.For("im.room.42")
	assert.Equal(t, 5, pol.HistorySize)
	// Unset fields must keep the compiled default.
	assert.True(t, pol.History, "unset history must keep default true")
	assert.True(t, pol.Presence, "unset presence must keep default true")
	assert.True(t, pol.Recover, "unset recover must keep default true")
	assert.False(t, pol.Survey, "unset survey must keep default false")
}

// TestChannelPolicy_ExplicitFalseOverridesDefault verifies an explicit false
// beats a default true (the overlay must not skip set-but-false values).
func TestChannelPolicy_ExplicitFalseOverridesDefault(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{
			{Pattern: "iot.**", ChannelPolicySpec: config.ChannelPolicySpec{Presence: policyBoolPtr(false)}},
		},
	})
	require.NoError(t, err)

	pol := engine.For("iot.device.1")
	assert.False(t, pol.Presence)
	assert.True(t, pol.History)
}

func TestChannelPolicy_TransientOnlyImpliesNoHistory(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{
			{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
		},
	})
	require.NoError(t, err)

	pol := engine.For("game.tick.fps")
	assert.True(t, pol.TransientOnly)
	assert.False(t, pol.History, "transient_only must force History=false")
	assert.False(t, pol.Recover, "transient_only must force Recover=false")

	// A rule that sets transient_only=true and history=true must still end
	// up with History=false.
	engine2, err := NewChannelPolicyEngine(config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{
			{Pattern: "tick.**", ChannelPolicySpec: config.ChannelPolicySpec{
				TransientOnly: policyBoolPtr(true),
				History:       policyBoolPtr(true),
			}},
		},
	})
	require.NoError(t, err)
	pol2 := engine2.For("tick.1")
	assert.True(t, pol2.TransientOnly)
	assert.False(t, pol2.History)
	assert.False(t, pol2.Recover)
}

// TestChannelPolicy_EngineDurationParseFallback verifies that an unparsable
// duration in the engine (bypassing config.Validate) falls back to "not
// overridden" instead of failing.
func TestChannelPolicy_EngineDurationParseFallback(t *testing.T) {
	engine, err := NewChannelPolicyEngine(config.ChannelConfig{
		Default: config.ChannelPolicySpec{HistoryTTL: "not-a-duration"},
	})
	require.NoError(t, err)
	pol := engine.For("any.channel")
	assert.Zero(t, pol.HistoryTTL, "invalid duration must fall back to 0 (broker global)")
	assert.True(t, pol.History)
}

// TestChannelPolicy_InvalidPatternErrors verifies the engine rejects invalid
// policy patterns (defense in depth; config.Validate rejects them earlier).
func TestChannelPolicy_InvalidPatternErrors(t *testing.T) {
	_, err := NewChannelPolicyEngine(config.ChannelConfig{
		Policies: []config.ChannelPolicyRule{{Pattern: "a.**.b"}},
	})
	require.Error(t, err)
}

// TestNodePublish_HistoryDisabled verifies Node.Publish refuses channels
// whose policy disables history, for both transient_only and history=false.
func TestNodePublish_HistoryDisabled(t *testing.T) {
	node := NewNode(&config.Server{
		Channels: config.ChannelConfig{
			Policies: []config.ChannelPolicyRule{
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
// publication's HistorySize/HistoryTTL from the policy when the caller left
// them zero.
func TestNodePublish_HistorySizeInjected(t *testing.T) {
	node := NewNode(&config.Server{
		Channels: config.ChannelConfig{
			Policies: []config.ChannelPolicyRule{
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
		Channels: config.ChannelConfig{
			Policies: []config.ChannelPolicyRule{
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
	subscriber.mu.Lock()
	subscriber.authenticated = true
	subscriber.mu.Unlock()
	require.NoError(t, node.AddClient(subscriber))
	require.NoError(t, node.AddSubscription(ctx, "game.tick.fps", Subscriber{Client: subscriber, Ephemeral: false}))
	subTransport.messages = nil

	// A publisher client, authenticated via Connect.
	pubTransport := &capturingTransport{}
	publisher, _, err := NewClient(ctx, node, pubTransport, JSONMarshaler{})
	require.NoError(t, err)
	require.NoError(t, publisher.HandleMessage(ctx, &clientpb.InboundMessage{
		Id:       "connect-1",
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{}},
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
	assert.Equal(t, uint64(0), pubAck.GetOffset())

	// Nothing written to history.
	history, err := node.Broker().History("game.tick.fps", 0, 0)
	require.NoError(t, err)
	require.Empty(t, history, "policy-forced transient must not write history")

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
		Channels: config.ChannelConfig{
			Policies: []config.ChannelPolicyRule{
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
		Envelope: &clientpb.InboundMessage_Connect{Connect: &clientpb.Connect{}},
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
		Channels: config.ChannelConfig{
			Policies: []config.ChannelPolicyRule{
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
	history, err := node.Broker().History("im.room.1", 0, 0)
	require.NoError(t, err)
	require.Len(t, history, 5, "policy history_size=5 must cap the fresh channel ring")
	require.Equal(t, "m-5", string(history[0].Payload))
	require.Equal(t, "m-9", string(history[4].Payload))
}

// TestNodePublish_PolicyDisabledHistoryStillTransientable verifies that
// ErrHistoryDisabled only affects Node.Publish: PublishTransient remains
// available on the same channel.
func TestNodePublish_PolicyDisabledHistoryStillTransientable(t *testing.T) {
	node := NewNode(&config.Server{
		Channels: config.ChannelConfig{
			Policies: []config.ChannelPolicyRule{
				{Pattern: "game.tick.**", ChannelPolicySpec: config.ChannelPolicySpec{TransientOnly: policyBoolPtr(true)}},
			},
		},
	})
	require.NoError(t, node.Run(context.Background()))

	require.NoError(t, node.PublishTransient("game.tick.fps", publishPub([]byte("tick"), false)))
}
