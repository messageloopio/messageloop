package redisbroker

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/stream"
	"github.com/messageloopio/messageloop/pkg/topics"
)

// TestRedisBroker_Publish_RejectsMalformedChannel pins B1: the Redis broker's
// publish entry must reject channels with explicit empty segments ("a.",
// ".a", "a..b") and the empty channel with ErrBadTopic before any Redis
// interaction (no live Redis required for this test).
func TestRedisBroker_Publish_RejectsMalformedChannel(t *testing.T) {
	broker := New(config.RedisConfig{}).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	for _, ch := range []string{"a.", ".a", "a..b", ""} {
		_, err := broker.Publish(ch, &stream.Publication{Payload: []byte("x"), Kind: stream.PayloadKindText})
		assert.ErrorIs(t, err, topics.ErrBadTopic, "Publish(%q)", ch)
		err = broker.PublishTransient(ch, &stream.Publication{Payload: []byte("x"), Kind: stream.PayloadKindText})
		assert.ErrorIs(t, err, topics.ErrBadTopic, "PublishTransient(%q)", ch)
	}
}
