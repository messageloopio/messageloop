package websocket_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebSocket_MultiClientBroadcast verifies that a message published to a
// channel is delivered to all subscribers on that channel.
func TestWebSocket_MultiClientBroadcast(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)

	const numSubscribers = 5
	conns := make([]*websocket.Conn, numSubscribers)

	// Connect and subscribe all clients.
	for i := 0; i < numSubscribers; i++ {
		c := dialWS(t, server)
		conns[i] = c
		sendJSON(t, c, map[string]any{
			"id":      "conn",
			"connect": map[string]any{"client_id": "sub-" + strings.Repeat("x", i)},
		})
		resp := readJSON(t, c, 2*time.Second)
		require.NotNil(t, resp["connected"])

		sendJSON(t, c, map[string]any{
			"id":        "sub",
			"subscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "broadcast"}}},
		})
		ack := readJSON(t, c, 2*time.Second)
		require.NotNil(t, ack["subscribe_ack"])
	}

	// Publisher connects and publishes.
	pub := dialWS(t, server)
	sendJSON(t, pub, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "publisher"},
	})
	_ = readJSON(t, pub, 2*time.Second)

	sendJSON(t, pub, map[string]any{
		"id":      "pub1",
		"publish": map[string]any{"channel": "broadcast", "payload": map[string]any{"text": "hello all"}},
	})
	pubAck := readJSON(t, pub, 2*time.Second)
	require.NotNil(t, pubAck["publish_ack"])

	// All subscribers should receive the publication.
	for i := 0; i < numSubscribers; i++ {
		msg := readJSON(t, conns[i], 2*time.Second)
		require.NotNil(t, msg["publication"], "subscriber %d did not receive publication", i)
	}
}

// TestWebSocket_DisconnectCleansUpSubscriptions verifies that when a client
// disconnects, its subscriptions are removed and the channel subscriber count
// drops to zero.
func TestWebSocket_DisconnectCleansUpSubscriptions(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)

	conn := dialWS(t, server)
	sendJSON(t, conn, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "cleanup-client"},
	})
	resp := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, resp["connected"])

	sendJSON(t, conn, map[string]any{
		"id":        "sub",
		"subscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "ephemeral-ch"}}},
	})
	ack := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, ack["subscribe_ack"])

	// Verify subscriber is present.
	require.Equal(t, 1, node.Hub().NumSubscribers("ephemeral-ch"))

	// Close the connection.
	require.NoError(t, conn.Close())

	// Wait for cleanup.
	require.Eventually(t, func() bool {
		return node.Hub().NumSubscribers("ephemeral-ch") == 0
	}, 2*time.Second, 50*time.Millisecond)
}

// TestWebSocket_ACL_SubscribeDenied verifies built-in ACL enforcement during
// subscribe. A denied subscribe should return an error but not disconnect.
func TestWebSocket_ACL_SubscribeDenied(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(&config.Server{
		ACL: config.ACLConfig{
			Rules: []config.ACLRule{
				{ChannelPattern: "private.*", AllowSubscribe: []string{"admin"}},
			},
		},
	})
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)
	conn := dialWS(t, server)

	sendJSON(t, conn, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "non-admin"},
	})
	resp := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, resp["connected"])

	// Try to subscribe to a restricted channel.
	sendJSON(t, conn, map[string]any{
		"id":        "sub",
		"subscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "private.secret"}}},
	})
	// Expect an ACL error followed by a subscribe_ack (possibly empty).
	msg := readJSON(t, conn, 2*time.Second)
	if errObj, ok := msg["error"]; ok {
		errMap := errObj.(map[string]any)
		assert.Equal(t, "ACL_DENIED", errMap["code"])
		// Read the subscribe_ack that follows.
		msg = readJSON(t, conn, 2*time.Second)
	}
	// The subscribe_ack should not include the denied channel.
	if subAck, ok := msg["subscribe_ack"]; ok {
		ackMap := subAck.(map[string]any)
		if subs, ok := ackMap["subscriptions"]; ok && subs != nil {
			subList := subs.([]any)
			for _, s := range subList {
				sm := s.(map[string]any)
				assert.NotEqual(t, "private.secret", sm["channel"])
			}
		}
	}

	// The connection should still be alive — send a ping equivalent.
	sendJSON(t, conn, map[string]any{
		"id":   "alive",
		"ping": map[string]any{},
	})
	pong := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, pong["pong"], "connection should still be alive after ACL denial")
}

// TestWebSocket_ACL_PublishDenied verifies that a publish to a channel blocked
// by ACL returns an error without disconnecting.
func TestWebSocket_ACL_PublishDenied(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(&config.Server{
		ACL: config.ACLConfig{
			Rules: []config.ACLRule{
				{ChannelPattern: "readonly.*", AllowPublish: []string{"admin"}},
			},
		},
	})
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)
	conn := dialWS(t, server)

	sendJSON(t, conn, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "regular-user"},
	})
	resp := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, resp["connected"])

	// Try to publish to the restricted channel.
	sendJSON(t, conn, map[string]any{
		"id":      "pub",
		"publish": map[string]any{"channel": "readonly.data", "payload": map[string]any{"text": "blocked"}},
	})
	msg := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, msg["error"], "expected ACL error, got: %v", msg)
	errMap := msg["error"].(map[string]any)
	assert.Equal(t, "ACL_DENIED", errMap["code"])

	// Connection should remain alive.
	sendJSON(t, conn, map[string]any{
		"id":   "alive",
		"ping": map[string]any{},
	})
	pong := readJSON(t, conn, 2*time.Second)
	require.NotNil(t, pong["pong"])
}

// TestWebSocket_UnsubscribeStopsDelivery verifies that after unsubscribing,
// a client no longer receives messages on that channel.
func TestWebSocket_UnsubscribeStopsDelivery(t *testing.T) {
	ctx := t.Context()
	node := messageloop.NewNode(nil)
	require.NoError(t, node.Run(ctx))

	server := startTestWSServer(t, node)

	// Subscriber
	sub := dialWS(t, server)
	sendJSON(t, sub, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "unsub-test"},
	})
	_ = readJSON(t, sub, 2*time.Second)

	sendJSON(t, sub, map[string]any{
		"id":        "sub",
		"subscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "temp"}}},
	})
	_ = readJSON(t, sub, 2*time.Second)

	// Unsubscribe
	sendJSON(t, sub, map[string]any{
		"id":          "unsub",
		"unsubscribe": map[string]any{"subscriptions": []map[string]any{{"channel": "temp"}}},
	})
	unsubAck := readJSON(t, sub, 2*time.Second)
	require.NotNil(t, unsubAck["unsubscribe_ack"])

	// Verify subscriber count dropped before publishing.
	require.Eventually(t, func() bool {
		return node.Hub().NumSubscribers("temp") == 0
	}, 2*time.Second, 50*time.Millisecond)

	// Publish from another client
	pub := dialWS(t, server)
	sendJSON(t, pub, map[string]any{
		"id":      "conn",
		"connect": map[string]any{"client_id": "pub-after-unsub"},
	})
	_ = readJSON(t, pub, 2*time.Second)
	sendJSON(t, pub, map[string]any{
		"id":      "pub1",
		"publish": map[string]any{"channel": "temp", "payload": map[string]any{"text": "after unsub"}},
	})
	_ = readJSON(t, pub, 2*time.Second) // publish_ack

	// Subscriber should NOT receive a publication (timeout expected).
	_ = sub.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err := sub.ReadMessage()
	require.Error(t, err, "should not receive message after unsubscribe")
}
