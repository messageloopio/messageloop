package registryhttp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
	registryredis "github.com/lynx-go/lynx/contrib/registry-redis"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/redistest"
	"github.com/messageloopio/messageloop/protocol/registrant"
)

// TestClosedLoopWithRedis pins the full registrant contract in one run: the
// real protocol/registrant client talks to the real handler over HTTP, the
// handler writes through to registry-redis, and a Resolver built on the
// same directory sees the instance — the exact view the ServiceProxy routes
// against. Skipped when Redis is absent; MESSAGELOOP_TEST_REDIS_REQUIRED
// (set in CI) turns the absence into a failure.
func TestClosedLoopWithRedis(t *testing.T) {
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	if err := rdb.Ping(ctx).Err(); err != nil {
		redistest.SkipOrFatal(t, "registryhttp", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	// Unique prefix per run keeps parallel runs isolated; the instance key
	// carries a 1s TTL so heartbeat renewal is observable on test timescales.
	prefix := fmt.Sprintf("ml2:registryhttp-it-%d/", time.Now().UnixNano())
	disc := registryredis.New(rdb,
		registryredis.WithKeyPrefix(prefix),
		registryredis.WithTTL(time.Second),
	)
	srv := httptest.NewServer(New(disc, disc))
	t.Cleanup(srv.Close)

	reg, err := registrant.Register(ctx, registrant.Options{
		ServerURL:         srv.URL,
		Service:           "e2e-battle",
		ID:                "i-1",
		Address:           "127.0.0.1:9001",
		Version:           "v9",
		Tags:              []string{"it"},
		HeartbeatInterval: 200 * time.Millisecond,
	})
	require.NoError(t, err)

	resolver := registry.NewResolver(disc)
	t.Cleanup(func() { _ = resolver.Close() })

	// The routing view sees the instance with its grpc endpoint; the first
	// Get fills the snapshot synchronously.
	instances, err := resolver.GetAll(ctx, "e2e-battle", registry.Filter{Protocol: registry.ProtocolGRPC})
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, "i-1", instances[0].ID)
	assert.Equal(t, "v9", instances[0].Version)
	ep, err := registry.EndpointOf(instances[0], registry.ProtocolGRPC)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9001", ep.Address)

	// Heartbeats must carry the entry past a full TTL.
	time.Sleep(1500 * time.Millisecond)
	instances, err = resolver.GetAll(ctx, "e2e-battle", registry.Filter{Protocol: registry.ProtocolGRPC})
	require.NoError(t, err)
	assert.Len(t, instances, 1, "entry must survive past one TTL via heartbeats")

	// Expiring the key server-side is answered by an automatic
	// re-register under the same ID.
	require.NoError(t, rdb.Del(ctx, prefix+"instance/e2e-battle/i-1").Err())
	require.Eventually(t, func() bool {
		return rdb.Exists(ctx, prefix+"instance/e2e-battle/i-1").Val() == 1
	}, 5*time.Second, 50*time.Millisecond, "expired entry must be re-registered")

	// Close deregisters through the HTTP API; the Pub/Sub event drops the
	// instance from the routing view.
	require.NoError(t, reg.Close())
	require.Eventually(t, func() bool {
		instances, err := resolver.GetAll(ctx, "e2e-battle", registry.Filter{Protocol: registry.ProtocolGRPC})
		return err != nil || len(instances) == 0
	}, 5*time.Second, 50*time.Millisecond, "instance must leave the routing view after Close")
}
