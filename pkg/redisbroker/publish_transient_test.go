package redisbroker

import (
	"testing"

	"github.com/messageloopio/messageloop"
	"github.com/stretchr/testify/require"
)

// TestRedisBroker_PublishTransientSkipsHistory verifies P2-19: transient
// publications are broadcast in real time but never written to the stream,
// so they do not appear in History, while regular publications on the same
// channel still do.
func TestRedisBroker_PublishTransientSkipsHistory(t *testing.T) {
	redisCfg := requireCommandBusRedis(t)
	broker := New(redisCfg).(*redisBroker)
	t.Cleanup(func() { _ = broker.client.Close() })

	ch := "transient-hist"

	err := broker.PublishTransient(ch, &messageloop.Publication{Payload: []byte("join"), Kind: messageloop.PayloadKindText})
	require.NoError(t, err)

	page, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.Empty(t, page.Pubs(), "transient publications must not appear in history")

	offset, err := broker.Publish(ch, &messageloop.Publication{Payload: []byte("normal"), Kind: messageloop.PayloadKindText})
	require.NoError(t, err)
	require.NotZero(t, offset)
	page, err = broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.Len(t, page.Pubs(), 1, "regular publications on the same channel must still be recorded")
}
