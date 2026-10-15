package proxy

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
	registryredis "github.com/lynx-go/lynx/contrib/registry-redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mlmetrics "github.com/messageloopio/messageloop/internal/metrics"
	"github.com/messageloopio/messageloop/internal/redistest"
)

// The integration suites each own a dedicated Redis logical DB (12 command
// bus, 13 SDK e2e, 14 atomic writes, 15 cluster integration); this one takes
// 11 and flushes it before and after every test. A DB below the stock
// server's 16-database default keeps the suite runnable against a vanilla
// redis:7 container.
const serviceProxyRedisTestDB = 11

// TestServiceProxyRedis_ConsistentHashLifecycle walks the deployment
// lifecycle of a service fleet against a real registry-redis backend
// (design §6 integration scenario):
//
//  1. three instances: the same key always lands on the same instance
//     (consistent hash across resolver snapshots served from Redis);
//  2. scale-up to a fourth instance: only keys whose owner is the displaced
//     instance move, and they all move to the newcomer (Rendezvous minimal
//     disturbance, asserted against the Rendezvous oracle);
//  3. draining one instance: its keys drift to the deterministic new owner,
//     every other key keeps its owner, the stale pooled connection is pruned
//     (leave event);
//  4. empty fleet: every request fails with ErrNoHealthyInstance (the
//     session-visible PROXY_ERROR), proxy_service_no_instance_total counts
//     each rejection and the instances gauge reports 0.
//
// Every expected owner is computed with the Rendezvous oracle over the live
// instance set and cross-checked against the real per-instance hit counters,
// so both placement determinism and real traffic placement are pinned.
func TestServiceProxyRedis_ConsistentHashLifecycle(t *testing.T) {
	const service = "acme-battle"
	ids := []string{"inst-1", "inst-2", "inst-3", "inst-4"}
	keys := []string{
		"acme:battle.scene-1", "acme:battle.scene-2", "acme:battle.scene-3",
		"acme:battle.scene-4", "acme:battle.scene-5", "acme:battle.scene-6",
		"acme:battle.scene-7", "acme:battle.scene-8", "acme:battle.scene-9",
	}

	_, disc := newRedisRegistryBackend(t)
	p := newRedisServiceProxy(t, disc, &ProxyConfig{
		Name:    "battle",
		Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
	})
	metrics := newWiredMetrics(t, p)

	// ownersOf maps every key to its Rendezvous owner for the given live set.
	ownersOf := func(live []string) map[string]string {
		snapshot := make([]registry.Instance, 0, len(live))
		for _, id := range live {
			snapshot = append(snapshot, registry.Instance{
				Name: service, ID: id, Status: registry.StatusPassing,
				Endpoints: []registry.Endpoint{{Protocol: registry.ProtocolGRPC, Address: "127.0.0.1:1"}},
			})
		}
		pick := registry.RendezvousPicker()
		out := make(map[string]string, len(keys))
		for _, key := range keys {
			inst, err := pick.Pick(key, snapshot)
			require.NoError(t, err)
			out[key] = inst.ID
		}
		return out
	}
	// expected accumulates the hit count per instance implied by the oracle
	// across phases; driveKeys sends calls RPCs per key and adds to it.
	expected := make(map[string]int64)
	driveKeys := func(live []string, calls int) {
		oracle := ownersOf(live)
		for range calls {
			for _, key := range keys {
				resp, err := p.RPC(context.Background(), &RPCProxyRequest{
					ID: "req", Channel: key, Method: "echo",
					Payload: testPayload(t, map[string]any{"key": key}),
				})
				require.NoError(t, err, "key %s", key)
				require.NotNil(t, resp.Payload)
				expected[oracle[key]]++
			}
		}
	}
	// assertHits checks the real fake-instance counters against the
	// accumulated oracle expectation: any call that strayed from its owner
	// would unbalance some instance's total.
	assertHits := func(hits map[string]*atomic.Int64) {
		t.Helper()
		for id, h := range hits {
			assert.Equal(t, expected[id], h.Load(), "instance %s hit count", id)
		}
	}

	// --- Phase 1: three instances; repeated calls stick to one instance. ---
	insts := registerRedisInstances(t, disc, service, "inst-1", "inst-2", "inst-3")
	waitForSnapshot(t, p, 3)
	live3 := []string{"inst-1", "inst-2", "inst-3"}
	owners3 := ownersOf(live3)
	driveKeys(live3, 3)
	assertHits(insts)
	assert.InEpsilon(t, 3, testutil.ToFloat64(metrics.ProxyServiceInstances.WithLabelValues(service)), 0)

	// --- Phase 2: scale-up to four; minimal disturbance. ---
	inst4, hits4 := startFakeInstance(t, service, "inst-4")
	require.NoError(t, disc.Register(context.Background(), inst4))
	insts["inst-4"] = hits4
	waitForSnapshot(t, p, 4)

	owners4 := ownersOf(ids[:4])
	moved := 0
	for _, key := range keys {
		if owners3[key] != owners4[key] {
			moved++
			assert.Equal(t, "inst-4", owners4[key], "key %s must move to the newcomer only", key)
		}
	}
	assert.Greater(t, moved, 0, "a 4-instance Rendezvous set must displace at least one of nine keys")
	driveKeys(ids[:4], 1)
	assertHits(insts)
	assert.InEpsilon(t, 4, testutil.ToFloat64(metrics.ProxyServiceInstances.WithLabelValues(service)), 0)

	// --- Phase 3: drain one instance; its keys drift, others stay put. ---
	victim := owners3[keys[0]] // owns keys, hence holds a pooled connection
	require.NoError(t, disc.Deregister(context.Background(), service, victim))
	waitForSnapshot(t, p, 3)

	liveAfterDrain := make([]string, 0, 3)
	for _, id := range ids[:4] {
		if id != victim {
			liveAfterDrain = append(liveAfterDrain, id)
		}
	}
	ownersDrained := ownersOf(liveAfterDrain)
	for _, key := range keys {
		// The pre-drain baseline is owners4 (the state right before the
		// drain): keys the scale-up already moved to inst-4 must stay there.
		if owners4[key] == victim {
			assert.NotEqual(t, victim, ownersDrained[key], "key %s must drift off the drained instance", key)
		} else {
			assert.Equal(t, owners4[key], ownersDrained[key], "key %s must keep its owner", key)
		}
	}
	driveKeys(liveAfterDrain, 1)
	assertHits(insts)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ProxyServiceMembershipEventsTotal.WithLabelValues(service, "leave")),
		"exactly one pooled connection (the drained instance) must be pruned")

	// Joins cover every instance ever dialed — the distinct oracle owners
	// across the phases — not the raw RPC count.
	distinctDialed := map[string]bool{}
	for _, owners := range []map[string]string{owners3, owners4, ownersDrained} {
		for _, id := range owners {
			distinctDialed[id] = true
		}
	}
	assert.InEpsilon(t, float64(len(distinctDialed)),
		testutil.ToFloat64(metrics.ProxyServiceMembershipEventsTotal.WithLabelValues(service, "join")), 0)

	// --- Phase 4: empty fleet; fail-fast with the sentinel + counter. ---
	for _, id := range liveAfterDrain {
		require.NoError(t, disc.Deregister(context.Background(), service, id))
	}
	waitForEmptySnapshot(t, p)
	// waitForEmptySnapshot's own probe already observed the empty snapshot
	// once; count from that baseline.
	baseline := testutil.ToFloat64(metrics.ProxyServiceNoInstanceTotal.WithLabelValues(service))

	for range 2 {
		_, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID: "req", Channel: keys[0], Method: "echo",
			Payload: testPayload(t, map[string]any{}),
		})
		require.ErrorIs(t, err, ErrNoHealthyInstance)
		assert.NotErrorIs(t, err, ErrNoProxyFound, "no-instance must stay distinct from no-route")
	}
	assert.InEpsilon(t, baseline+2, testutil.ToFloat64(metrics.ProxyServiceNoInstanceTotal.WithLabelValues(service)), 0)
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ProxyServiceInstances.WithLabelValues(service)))
}

// TestServiceProxyRedis_SameKeyConvergesAcrossNodes asserts the cross-node
// contract over a real Redis directory: two independent ServiceProxy
// instances — each with its own redis connection, backend client and
// Resolver cache, i.e. everything a separate messageloop process would own —
// must resolve the same key to the same instance, stably across repeats and
// for both key syntaxes (channel and metadata).
//
// TODO(v1.1 follow-up): run this as a true two-process e2e — build the real
// cmd/server binary and spawn two nodes over one Redis via the sdks/go
// harness (e2e_process_test.go). That harness lives in a separate Go module
// which deliberately carries neither lynx nor a redis client (Q5 keeps SDK
// deps minimal), so registering fake backend instances there would mean a
// new module dependency or a hand-rolled RESP writer; the in-process
// dual-view below exercises the identical discovery stack (registry-redis
// JSON keys + Pub/Sub watch + Resolver caches) minus the process boundary.
func TestServiceProxyRedis_SameKeyConvergesAcrossNodes(t *testing.T) {
	const service = "acme-battle"
	const key = "acme:battle.scene-42"
	rdb, discA := newRedisRegistryBackend(t)
	discB := redisBackendOnSameServer(t, rdb)

	nodeA := newRedisServiceProxy(t, discA, &ProxyConfig{
		Name: "battle-a", Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
	})
	nodeB := newRedisServiceProxy(t, discB, &ProxyConfig{
		Name: "battle-b", Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:scene"},
	})

	insts := registerRedisInstances(t, discA, service, "inst-1", "inst-2", "inst-3")
	waitForSnapshot(t, nodeA, 3)
	waitForSnapshot(t, nodeB, 3)

	oracle := func(key string) string {
		snapshot, err := nodeA.snapshot(context.Background())
		require.NoError(t, err)
		inst, err := registry.RendezvousPicker().Pick(key, snapshot)
		require.NoError(t, err)
		return inst.ID
	}
	rpc := func(p *ServiceProxy, meta map[string]string) {
		t.Helper()
		resp, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID: "req", Channel: key, Method: "echo",
			Payload: testPayload(t, map[string]any{}),
			Meta:    meta,
		})
		require.NoError(t, err)
		require.NotNil(t, resp.Payload)
	}
	// countersBefore copies the per-instance hit counters.
	countersBefore := func() map[string]int64 {
		out := make(map[string]int64, len(insts))
		for id, h := range insts {
			out[id] = h.Load()
		}
		return out
	}

	// Channel-key routing: both views land every call on the same single
	// owner, stably across repeats.
	channelOwner := oracle(key)
	for range 3 {
		rpc(nodeA, nil)
		rpc(nodeB, nil)
	}
	assert.Equal(t, int64(6), insts[channelOwner].Load(),
		"both nodes must converge their calls for the key on the same instance")
	for id, h := range insts {
		if id != channelOwner {
			assert.Equal(t, int64(0), h.Load(), "instance %s must see no traffic", id)
		}
	}

	// Metadata-key routing converges across views too: the metadata value
	// picks its own owner and both nodes contribute exactly one call each,
	// whatever the relationship between the two keys' owners.
	metaOwner := oracle("scene-77")
	before := countersBefore()
	rpc(nodeA, map[string]string{"scene": "scene-77"})
	rpc(nodeB, map[string]string{"scene": "scene-77"})
	for id, h := range insts {
		want := before[id]
		if id == metaOwner {
			want += 2
		}
		assert.Equal(t, want, h.Load(), "instance %s delta after the metadata-keyed calls", id)
	}
}

// TestServiceProxyRedis_MetadataFallbackCounter drives metadata-keyed RPCs
// through a metrics-wired proxy over the real discovery stack and asserts
// proxy_service_key_fallback_total: only missing or empty entries count.
func TestServiceProxyRedis_MetadataFallbackCounter(t *testing.T) {
	const service = "acme-battle"
	_, disc := newRedisRegistryBackend(t)
	p := newRedisServiceProxy(t, disc, &ProxyConfig{
		Name: "battle", Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:scene"},
	})
	metrics := newWiredMetrics(t, p)

	registerRedisInstances(t, disc, service, "inst-1")
	waitForSnapshot(t, p, 1)

	rpc := func(meta map[string]string) {
		t.Helper()
		_, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID: "req", Channel: "acme:battle.scene-42", Method: "echo",
			Payload: testPayload(t, map[string]any{}),
			Meta:    meta,
		})
		require.NoError(t, err)
	}

	rpc(map[string]string{"scene": "scene-77"})
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ProxyServiceKeyFallbackTotal.WithLabelValues(service)))
	rpc(nil) // no metadata at all → fallback to the channel key
	rpc(map[string]string{"scene": ""})
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.ProxyServiceKeyFallbackTotal.WithLabelValues(service)))
}

// newRedisRegistryBackend probes the integration Redis and builds the
// registry-redis backend on a dedicated DB: PING with the redistest policy
// (skip locally, hard-fail when MESSAGELOOP_TEST_REDIS_REQUIRED is set),
// then flush before and after. The TTL is raised to a minute to stand in for
// the heartbeat loop the test does not run — instances are written once via
// Register (the same SET EX + SADD + PUBLISH pipeline the lynx Registrar
// issues) and must outlive the scenario.
func newRedisRegistryBackend(t *testing.T) (*redis.Client, *registryredis.Client) {
	t.Helper()

	addr := os.Getenv("MESSAGELOOP_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	password := os.Getenv("MESSAGELOOP_TEST_REDIS_PASSWORD")
	if password == "" {
		password = os.Getenv("REDIS_PASSWORD")
	}

	rdb := redis.NewClient(&redis.Options{Addr: addr, Password: password, DB: serviceProxyRedisTestDB})
	t.Cleanup(func() { _ = rdb.Close() })

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		redistest.SkipOrFatal(t, "service proxy redis integration tests", err)
	}
	require.NoError(t, rdb.FlushDB(pingCtx).Err())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = rdb.FlushDB(cleanupCtx).Err()
	})

	disc := registryredis.New(rdb, registryredis.WithTTL(time.Minute))
	t.Cleanup(func() { _ = disc.Close() })
	return rdb, disc
}

// redisBackendOnSameServer builds a second, fully independent discovery
// stack (own redis connection + own backend client) pointing at the same
// Redis server — the process-level isolation of a second messageloop node,
// minus the process.
func redisBackendOnSameServer(t *testing.T, template *redis.Client) *registryredis.Client {
	t.Helper()

	opts := template.Options()
	rdb := redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		Password:     opts.Password,
		DB:           opts.DB,
		DialTimeout:  opts.DialTimeout,
		ReadTimeout:  opts.ReadTimeout,
		WriteTimeout: opts.WriteTimeout,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, rdb.Ping(pingCtx).Err())

	disc := registryredis.New(rdb, registryredis.WithTTL(time.Minute))
	t.Cleanup(func() { _ = disc.Close() })
	return disc
}

// newRedisServiceProxy builds a ServiceProxy over a registry-redis backend.
// Cleanup closes the proxy first, then the resolver (the backend and the
// redis client are owned by the backend helpers).
func newRedisServiceProxy(t *testing.T, disc *registryredis.Client, cfg *ProxyConfig) *ServiceProxy {
	t.Helper()
	resolver := registry.NewResolver(disc)
	t.Cleanup(func() { _ = resolver.Close() })
	if cfg.GRPC == nil {
		cfg.GRPC = &GRPCProxyConfig{Insecure: true}
	}
	p, err := NewServiceProxy(cfg, resolver)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// newWiredMetrics creates a fresh Prometheus metrics object (own registry)
// and wires it into the proxy the way Node.SetupProxy does.
func newWiredMetrics(t *testing.T, p *ServiceProxy) *mlmetrics.Metrics {
	t.Helper()
	m := mlmetrics.NewMetrics(prometheus.NewRegistry())
	p.SetMetrics(m)
	return m
}

// registerRedisInstances is the registry-redis flavor of the memory-registry
// helper: it starts a fake gRPC echo instance per ID and registers it in the
// real backend (Register writes the instance/{service}/{id} JSON key with
// SET EX, SADDs the index and publishes the watch event). Returns the
// per-instance hit counters keyed by instance ID.
func registerRedisInstances(t *testing.T, disc *registryredis.Client, service string, ids ...string) map[string]*atomic.Int64 {
	t.Helper()
	hits := make(map[string]*atomic.Int64, len(ids))
	for _, id := range ids {
		inst, h := startFakeInstance(t, service, id)
		require.NoError(t, disc.Register(context.Background(), inst))
		hits[id] = h
	}
	return hits
}

// waitForEmptySnapshot polls until the proxy reports the no-healthy-instance
// sentinel, i.e. the resolver cache has drained to zero (waitForSnapshot
// cannot express that: an empty snapshot surfaces as an error there).
func waitForEmptySnapshot(t *testing.T, p *ServiceProxy) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := p.snapshot(context.Background()); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot never drained to zero instances")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
