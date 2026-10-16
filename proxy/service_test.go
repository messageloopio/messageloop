package proxy

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/messageloop/shared/v2"
)

// frozenKeyedVector is the same frozen Rendezvous mapping the upstream lynx
// contrib/registry v1.19.0 pins in its picker_test.go (frozenKeyedVector).
// Holding one copy here as well keeps an upstream hash-encoding change (key
// concatenation, separator, hash function, tie-break) from silently breaking
// messageloop's cross-node ownership contract: any drift turns both sides
// red. The expected owners were computed offline with an independent FNV-1a
// 64 reference implementation, not generated from the implementation.
var frozenKeyedVector = map[string]string{
	"acme:battle.scene-42": "inst-4",
	"acme:battle.scene-43": "inst-3",
	"acme:battle.scene-44": "inst-2",
	"im.chat.room1":        "inst-1",
	"game.tick.001":        "inst-3",
	"":                     "inst-5",
	"游戏.房间.1":              "inst-4",
}

// TestServiceProxy_FrozenKeyedVector runs the frozen Rendezvous mapping
// through the ServiceProxy's own selection path (resolver snapshot over a
// registered instance set + keyed pick), not just the upstream picker, so
// the wiring is pinned together with the hash.
func TestServiceProxy_FrozenKeyedVector(t *testing.T) {
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "battle",
		Service: "acme-battle",
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
	}, mem)

	// Register the frozen instance set; endpoint addresses stay unused (the
	// selection path under test does not dial).
	for _, id := range []string{"inst-1", "inst-2", "inst-3", "inst-4", "inst-5"} {
		inst := registry.Instance{
			Name:   "acme-battle",
			ID:     id,
			Status: registry.StatusPassing,
			Endpoints: []registry.Endpoint{
				{Protocol: registry.ProtocolGRPC, Address: "127.0.0.1:1"},
			},
		}
		require.NoError(t, mem.Register(context.Background(), inst))
	}

	for key, want := range frozenKeyedVector {
		snapshot, err := p.snapshot(context.Background())
		require.NoError(t, err)
		require.Len(t, snapshot, 5)

		inst, err := p.selectInstance(snapshot, func() string { return key })
		require.NoError(t, err)
		assert.Equal(t, want, inst.ID, "key %q", key)
	}
}

func TestServiceProxy_ExtractKey(t *testing.T) {
	metaReq := &RPCProxyRequest{
		Channel: "acme:battle.scene-42",
		Meta:    map[string]string{"scene": "scene-99", "blank": ""},
	}
	bareReq := &RPCProxyRequest{Channel: "acme:battle.scene-42"}

	tests := []struct {
		name    string
		routing RoutingConfig
		req     *RPCProxyRequest
		want    string
	}{
		{
			name:    "default key is the raw channel",
			routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
			req:     metaReq,
			want:    "acme:battle.scene-42",
		},
		{
			name:    "explicit channel key keeps the namespace prefix",
			routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: RoutingKeyChannel},
			req:     metaReq,
			want:    "acme:battle.scene-42",
		},
		{
			name:    "metadata hit",
			routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:scene"},
			req:     metaReq,
			want:    "scene-99",
		},
		{
			name:    "metadata entry missing falls back to channel",
			routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:absent"},
			req:     metaReq,
			want:    "acme:battle.scene-42",
		},
		{
			name:    "metadata entry empty falls back to channel",
			routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:blank"},
			req:     metaReq,
			want:    "acme:battle.scene-42",
		},
		{
			name:    "metadata key with no metadata at all falls back to channel",
			routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:scene"},
			req:     bareReq,
			want:    "acme:battle.scene-42",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &ServiceProxy{name: "battle", routing: tc.routing}
			assert.Equal(t, tc.want, p.extractKey(tc.req))
		})
	}
}

// TestServiceProxy_RoundRobinRotates pins rotation at the selection layer:
// consecutive picks over one snapshot cycle through every instance in order.
func TestServiceProxy_RoundRobinRotates(t *testing.T) {
	p := &ServiceProxy{
		name:    "stateless",
		routing: RoutingConfig{Strategy: RoutingStrategyRoundRobin},
		unkeyed: registry.RoundRobinPicker(),
	}
	snapshot := []registry.Instance{{ID: "i1"}, {ID: "i2"}, {ID: "i3"}}

	var got []string
	for i := 0; i < 6; i++ {
		inst, err := p.selectInstance(snapshot, nil)
		require.NoError(t, err)
		got = append(got, inst.ID)
	}
	assert.Equal(t, []string{"i1", "i2", "i3", "i1", "i2", "i3"}, got)
}

// TestServiceProxy_RoundRobinRPCSpreads drives real RPCs through a
// round_robin proxy. The memory registry yields each snapshot in randomized
// map order, so per-call placement is uniform rather than a fixed sequence;
// what must hold is that traffic spreads across the whole snapshot instead
// of sticking to one instance (60 draws make a zero-hit instance a ~1e-11
// event).
func TestServiceProxy_RoundRobinRPCSpreads(t *testing.T) {
	const service = "rr-svc"
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "stateless",
		Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyRoundRobin},
	}, mem)

	hits := registerFakeInstances(t, mem, service, "i1", "i2", "i3")
	waitForSnapshot(t, p, 3)

	for i := 0; i < 60; i++ {
		resp, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID:      fmt.Sprintf("req-%d", i),
			Channel: fmt.Sprintf("acme:ch.%d", i),
			Method:  "echo",
			Payload: testPayload(t, map[string]any{"n": i}),
		})
		require.NoError(t, err)
		require.NotNil(t, resp.Payload)
	}

	total := int64(0)
	for id, h := range hits {
		n := h.Load()
		assert.Greater(t, n, int64(0), "instance %s must receive traffic", id)
		total += n
	}
	assert.Equal(t, int64(60), total, "every call must land in the snapshot")
}

// TestServiceProxy_RandomStaysInSnapshot drives real RPCs through a random
// proxy: every call must land on a snapshot instance, and with enough calls
// more than one instance must be hit (a single-instance outcome across 30
// uniform draws has probability ~1e-14).
func TestServiceProxy_RandomStaysInSnapshot(t *testing.T) {
	const service = "rnd-svc"
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "stateless",
		Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyRandom},
	}, mem)

	hits := registerFakeInstances(t, mem, service, "i1", "i2", "i3")
	waitForSnapshot(t, p, 3)

	for i := 0; i < 30; i++ {
		_, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID: fmt.Sprintf("req-%d", i), Channel: "acme:ch", Method: "echo",
			Payload: testPayload(t, map[string]any{"n": i}),
		})
		require.NoError(t, err)
	}

	total := int64(0)
	distinct := 0
	for _, h := range hits {
		n := h.Load()
		require.LessOrEqual(t, n, int64(30))
		total += n
		if n > 0 {
			distinct++
		}
	}
	assert.Equal(t, int64(30), total, "every call must land in the snapshot")
	assert.Greater(t, distinct, 1, "random must spread across instances")
}

// TestServiceProxy_ConsistentHashVertical runs the full vertical slice over
// real in-process gRPC servers: two fake backend instances registered in the
// memory registry, RPCs landing on the Rendezvous owner with the payload
// round-tripped, then one instance deregistered and traffic converging on
// the survivor with the stale pooled connection pruned.
func TestServiceProxy_ConsistentHashVertical(t *testing.T) {
	const service = "acme-battle"
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "battle",
		Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
	}, mem)

	insts := registerFakeInstances(t, mem, service, "inst-1", "inst-2")
	snapshot := waitForSnapshot(t, p, 2)

	key := "acme:battle.scene-42"
	owner, err := registry.RendezvousPicker().Pick(key, snapshot)
	require.NoError(t, err)

	payload := testPayload(t, map[string]any{"scene": "scene-42"})
	for n := 0; n < 3; n++ {
		resp, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID: fmt.Sprintf("req-%d", n), Channel: key, Method: "echo", Payload: payload,
		})
		require.NoError(t, err)
		require.NotNil(t, resp.Payload)
		assert.True(t, proto.Equal(payload, resp.Payload), "payload must round-trip")
	}
	assert.Equal(t, int64(3), insts[owner.ID].Load(), "all calls land on the Rendezvous owner")
	for id, h := range insts {
		if id != owner.ID {
			assert.Equal(t, int64(0), h.Load(), "instance %s must see no traffic", id)
		}
	}

	// Deregister the owner: the snapshot converges to the survivor, its
	// connection is the only one left (stale one pruned), and traffic lands
	// there.
	require.NoError(t, mem.Deregister(context.Background(), service, owner.ID))
	snapshot = waitForSnapshot(t, p, 1)
	require.Len(t, snapshot, 1)
	survivor := snapshot[0].ID
	assert.NotEqual(t, owner.ID, survivor)

	resp, err := p.RPC(context.Background(), &RPCProxyRequest{
		ID: "req-post", Channel: key, Method: "echo", Payload: payload,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Payload)
	assert.Equal(t, int64(1), insts[survivor].Load())
	assert.Equal(t, int64(3), insts[owner.ID].Load(), "deregistered instance must stay untouched")

	p.mu.RLock()
	conns := len(p.conns)
	p.mu.RUnlock()
	assert.Equal(t, 1, conns, "the removed instance's connection must be pruned")

	// Close drains the pool; a second Close is a no-op.
	require.NoError(t, p.Close())
	p.mu.RLock()
	conns = len(p.conns)
	p.mu.RUnlock()
	assert.Equal(t, 0, conns)
	assert.NoError(t, p.Close())
}

// TestServiceProxy_ConsistentHashSameKeyStable pins dispatch stability end
// to end: the same key always reaches the same instance, while a different
// key reaches its own owner (per the frozen vector over the same five
// instances).
func TestServiceProxy_ConsistentHashSameKeyStable(t *testing.T) {
	const service = "acme-battle"
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "battle",
		Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
	}, mem)

	insts := registerFakeInstances(t, mem, service, "inst-1", "inst-2", "inst-3", "inst-4", "inst-5")
	waitForSnapshot(t, p, 5)

	rpc := func(channel string) error {
		_, err := p.RPC(context.Background(), &RPCProxyRequest{
			ID: "req", Channel: channel, Method: "echo",
			Payload: testPayload(t, map[string]any{}),
		})
		return err
	}

	// Same key, repeated: concentrated on the frozen owner.
	require.NoError(t, rpc("acme:battle.scene-42"))
	require.NoError(t, rpc("acme:battle.scene-42"))
	require.NoError(t, rpc("acme:battle.scene-42"))
	assert.Equal(t, int64(3), insts["inst-4"].Load())
	// Different key: its own frozen owner, and nobody else gains traffic.
	require.NoError(t, rpc("im.chat.room1"))
	assert.Equal(t, int64(1), insts["inst-1"].Load())
	for _, id := range []string{"inst-2", "inst-3", "inst-5"} {
		assert.Equal(t, int64(0), insts[id].Load(), "instance %s must see no traffic", id)
	}
}

// TestServiceProxy_MetadataKeyVertical exercises the metadata routing key
// through real RPCs: the metadata value picks the owner, and a request
// without the entry falls back to the channel's owner.
func TestServiceProxy_MetadataKeyVertical(t *testing.T) {
	const service = "acme-battle"
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "battle",
		Service: service,
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash, Key: "metadata:scene"},
	}, mem)

	insts := registerFakeInstances(t, mem, service, "inst-1", "inst-2", "inst-3", "inst-4", "inst-5")
	snapshot := waitForSnapshot(t, p, 5)

	byKey := func(key string) string {
		inst, err := registry.RendezvousPicker().Pick(key, snapshot)
		require.NoError(t, err)
		return inst.ID
	}
	metaOwner, channelOwner := byKey("scene-77"), byKey("acme:battle.scene-42")

	_, err := p.RPC(context.Background(), &RPCProxyRequest{
		ID: "req-meta", Channel: "acme:battle.scene-42", Method: "echo",
		Payload: testPayload(t, map[string]any{}),
		Meta:    map[string]string{"scene": "scene-77"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), insts[metaOwner].Load(), "metadata value must pick the owner")

	// Missing metadata entry: falls back to the channel key.
	_, err = p.RPC(context.Background(), &RPCProxyRequest{
		ID: "req-fallback", Channel: "acme:battle.scene-42", Method: "echo",
		Payload: testPayload(t, map[string]any{}),
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), insts[channelOwner].Load(), "missing metadata must fall back to the channel")
}

func TestServiceProxy_NoHealthyInstance(t *testing.T) {
	mem := registry.NewMemory()
	p := newTestServiceProxy(t, &ProxyConfig{
		Name:    "battle",
		Service: "empty-svc",
		Routing: RoutingConfig{Strategy: RoutingStrategyConsistentHash},
	}, mem)

	// No instance ever registered: both method families report the sentinel.
	_, err := p.RPC(context.Background(), &RPCProxyRequest{
		ID: "req", Channel: "acme:battle.scene-42", Method: "echo",
		Payload: testPayload(t, map[string]any{}),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoHealthyInstance)

	_, err = p.Authenticate(context.Background(), &AuthenticateProxyRequest{ClientID: "c1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoHealthyInstance)

	// After the last instance deregisters (a real echo server, so the
	// transient pre-push window serves successfully instead of erroring)
	// the sentinel surfaces again, and it stays distinct from NO_PROXY:
	// the session keeps mapping it to the generic PROXY_ERROR code.
	inst, _ := startFakeInstance(t, "empty-svc", "i1")
	require.NoError(t, mem.Register(context.Background(), inst))
	waitForSnapshot(t, p, 1)
	require.NoError(t, mem.Deregister(context.Background(), "empty-svc", "i1"))

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err = p.RPC(context.Background(), &RPCProxyRequest{
			ID: "req", Channel: "acme:battle.scene-42", Method: "echo",
			Payload: testPayload(t, map[string]any{}),
		})
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expected RPCs to start failing after the last instance deregistered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.ErrorIs(t, err, ErrNoHealthyInstance)
	assert.NotErrorIs(t, err, ErrNoProxyFound, "no-instance must stay distinct from no-route")
}

func TestServiceProxy_Name(t *testing.T) {
	p := &ServiceProxy{name: "battle"}
	assert.Equal(t, "battle", p.Name())
}

func TestNewServiceProxy_Strategies(t *testing.T) {
	mem := registry.NewMemory()
	resolver := registry.NewResolver(mem)
	t.Cleanup(func() { _ = resolver.Close() })

	// Default (empty strategy) is the consistent hash with a RoundRobin
	// fallback for the stateless family.
	p, err := NewServiceProxy(&ProxyConfig{Name: "p", Service: "svc"}, resolver)
	require.NoError(t, err)
	assert.NotNil(t, p.keyed, "default strategy must be keyed")
	require.NoError(t, p.Close())

	p, err = NewServiceProxy(&ProxyConfig{
		Name: "p", Service: "svc",
		Routing: RoutingConfig{Strategy: RoutingStrategyRoundRobin},
	}, resolver)
	require.NoError(t, err)
	assert.Nil(t, p.keyed, "round_robin must not carry a keyed picker")
	assert.NotNil(t, p.unkeyed)
	require.NoError(t, p.Close())

	p, err = NewServiceProxy(&ProxyConfig{
		Name: "p", Service: "svc",
		Routing: RoutingConfig{Strategy: RoutingStrategyRandom},
	}, resolver)
	require.NoError(t, err)
	assert.Nil(t, p.keyed, "random must not carry a keyed picker")
	assert.NotNil(t, p.unkeyed)
	require.NoError(t, p.Close())

	// A nil resolver (registry disabled) must be refused, never silently
	// half-wired.
	_, err = NewServiceProxy(&ProxyConfig{Name: "p", Service: "svc"}, nil)
	assert.ErrorContains(t, err, "no registry resolver")

	// Unknown strategies are refused defensively (Validate already rejects
	// them earlier in the wiring).
	_, err = NewServiceProxy(&ProxyConfig{
		Name: "p", Service: "svc",
		Routing: RoutingConfig{Strategy: RoutingStrategySticky},
	}, resolver)
	assert.ErrorContains(t, err, "unknown routing.strategy")

	_, err = NewServiceProxy(&ProxyConfig{Name: "p"}, resolver)
	assert.ErrorContains(t, err, "missing service name")
}

func TestProxyConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ProxyConfig
		wantErr string // empty means valid
	}{
		{
			name: "static endpoint valid",
			cfg:  ProxyConfig{Name: "p", Endpoint: "127.0.0.1:9001"},
		},
		{
			name: "service backend valid",
			cfg:  ProxyConfig{Name: "p", Service: "acme-battle"},
		},
		{
			name:    "default routing valid",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{}},
			wantErr: "",
		},
		{
			name: "explicit consistent_hash with channel key valid",
			cfg: ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{
				Strategy: RoutingStrategyConsistentHash, Key: RoutingKeyChannel,
			}},
		},
		{
			name: "metadata key valid",
			cfg:  ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Key: "metadata:scene"}},
		},
		{
			name:    "round_robin valid",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Strategy: RoutingStrategyRoundRobin}},
			wantErr: "",
		},
		{
			name:    "random valid",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Strategy: RoutingStrategyRandom}},
			wantErr: "",
		},
		{
			name:    "endpoint and service mutually exclusive",
			cfg:     ProxyConfig{Name: "p", Endpoint: "127.0.0.1:9001", Service: "acme-battle"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "endpoint or service required",
			cfg:     ProxyConfig{Name: "p"},
			wantErr: "either endpoint or service",
		},
		{
			name:    "sticky reserved",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Strategy: RoutingStrategySticky}},
			wantErr: "reserved for a future release",
		},
		{
			name:    "unknown strategy rejected",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Strategy: "least_loaded"}},
			wantErr: "unknown routing.strategy",
		},
		{
			name:    "key under round_robin rejected",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Strategy: RoutingStrategyRoundRobin, Key: RoutingKeyChannel}},
			wantErr: "routing.key is only valid",
		},
		{
			name:    "key under random rejected",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Strategy: RoutingStrategyRandom, Key: "metadata:scene"}},
			wantErr: "routing.key is only valid",
		},
		{
			name:    "metadata key with empty name rejected",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Key: "metadata:"}},
			wantErr: "non-empty metadata field name",
		},
		{
			name:    "unknown key syntax rejected",
			cfg:     ProxyConfig{Name: "p", Service: "acme-battle", Routing: RoutingConfig{Key: "userid"}},
			wantErr: "unknown routing.key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

// newTestServiceProxy builds a ServiceProxy over an in-memory registry —
// no Redis is needed. Cleanup closes the proxy first, then the resolver.
func newTestServiceProxy(t *testing.T, cfg *ProxyConfig, mem *registry.Memory) *ServiceProxy {
	t.Helper()
	resolver := registry.NewResolver(mem)
	t.Cleanup(func() { _ = resolver.Close() })
	if cfg.GRPC == nil {
		cfg.GRPC = &GRPCProxyConfig{Insecure: true}
	}
	p, err := NewServiceProxy(cfg, resolver)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// registerFakeInstances starts a fake gRPC echo server per instance ID,
// registers each in the memory registry, and returns the per-instance hit
// counters keyed by instance ID.
func registerFakeInstances(t *testing.T, mem *registry.Memory, service string, ids ...string) map[string]*atomic.Int64 {
	t.Helper()
	hits := make(map[string]*atomic.Int64, len(ids))
	for _, id := range ids {
		inst, h := startFakeInstance(t, service, id)
		require.NoError(t, mem.Register(context.Background(), inst))
		hits[id] = h
	}
	return hits
}

// startFakeInstance serves one echo instance of proxypb.ProxyServiceServer
// on an ephemeral port (modeled after sdks/go/example/proxyserver) and
// returns its registry record advertising the grpc endpoint plus a call
// counter.
func startFakeInstance(t *testing.T, service, id string) (registry.Instance, *atomic.Int64) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	hits := &atomic.Int64{}
	srv := grpc.NewServer()
	proxypb.RegisterProxyServiceServer(srv, &fakeProxyService{hits: hits})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return registry.Instance{
		Name:   service,
		ID:     id,
		Status: registry.StatusPassing,
		Endpoints: []registry.Endpoint{
			{Protocol: registry.ProtocolGRPC, Address: lis.Addr().String()},
		},
	}, hits
}

// fakeProxyService echoes RPC payloads back and counts calls; one instance
// per running server.
type fakeProxyService struct {
	proxypb.UnimplementedProxyServiceServer
	hits *atomic.Int64
}

func (f *fakeProxyService) RPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	f.hits.Add(1)
	return &proxypb.RPCResponse{Id: req.Id, Payload: req.Payload}, nil
}

// waitForSnapshot waits until the proxy's resolver serves a snapshot of the
// wanted size and returns it. The memory backend pushes on
// Register/Deregister but the resolver consumes pushes asynchronously, so
// tests poll instead of racing; an empty cache surfaces as
// ErrNoHealthyInstance and counts as "not yet" while polling.
func waitForSnapshot(t *testing.T, p *ServiceProxy, want int) []registry.Instance {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := p.snapshot(context.Background())
		if err == nil && len(snapshot) == want {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never reached size %d (last error %v)", want, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testPayload builds a JSON payload for RPC round-trips.
func testPayload(t *testing.T, v map[string]any) *sharedv2.Payload {
	t.Helper()
	s, err := structpb.NewStruct(v)
	require.NoError(t, err)
	return &sharedv2.Payload{Data: &sharedv2.Payload_Json{Json: s}}
}
