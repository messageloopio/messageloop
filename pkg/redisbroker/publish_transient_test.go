package redisbroker

import (
	"testing"

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

	offset, err := broker.PublishTransient(ch, []byte("join"), true)
	require.NoError(t, err)
	require.Zero(t, offset, "transient publications have no history offset")

	pubs, err := broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.Empty(t, pubs, "transient publications must not appear in history")

	offset, err = broker.Publish(ch, []byte("normal"), true)
	require.NoError(t, err)
	require.NotZero(t, offset)
	pubs, err = broker.History(ch, 0, 0)
	require.NoError(t, err)
	require.Len(t, pubs, 1, "regular publications on the same channel must still be recorded")
}
