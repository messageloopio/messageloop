package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lynx-go/lynx/contrib/registry"
	"github.com/lynx-go/x/log"
	"google.golang.org/grpc"

	mlmetrics "github.com/messageloopio/messageloop/internal/metrics"
	proxypb "github.com/messageloopio/messageloop/shared/genproto/messageloop/proxy/v2"
)

// ErrNoHealthyInstance is returned when a service-type proxy resolves no
// healthy instance for its service (empty snapshot, or the resolver dropped
// a stale one). It is defined in the style of ErrNoProxyFound; the session
// maps it through the generic proxy-error path to the client-visible
// PROXY_ERROR code — no new client error code is introduced for it
// (v1.1 design §4.5).
var ErrNoHealthyInstance = errors.New("no healthy instance for service")

// ServiceProxy implements the full Proxy interface against a registry-based
// backend: instances are discovered through a registry.Resolver and every
// call picks one according to the configured routing strategy (v1.1 design
// §4.1). To the Router and the session it is just another Proxy — static
// endpoint routing, the client wire protocol and the error mapping are
// untouched.
//
// Method dispatch by strategy:
//
//   - consistent_hash (default): RPC is keyed — Rendezvous over the routing
//     key (channel or metadata entry), so the same key deterministically
//     lands on the same instance on every node. The stateless family
//     (Authenticate, SubscribeAcl, PublishAcl, On*, AuthenticateAPIKey) has
//     no scenario stickiness and round-robins instead.
//   - round_robin / random: every method, RPC included, uses the strategy's
//     unkeyed picker over the snapshot; key extraction is skipped entirely.
type ServiceProxy struct {
	name     string
	service  string
	timeout  time.Duration
	routing  RoutingConfig
	resolver *registry.Resolver
	// keyed is non-nil only under the consistent_hash strategy.
	keyed registry.KeyedPicker
	// unkeyed serves the unkeyed strategies (round_robin / random) for all
	// methods, and the stateless family under consistent_hash (RoundRobin).
	unkeyed registry.Picker
	// grpcCfg carries the TLS / insecure settings shared by every instance
	// of the service.
	grpcCfg *GRPCProxyConfig
	// metrics is the shared Prometheus collector wired by SetMetrics (the
	// same optional-interface injection the broker and command bus use).
	// Nil is tolerated and disables counting, so an unwired proxy never
	// panics.
	metrics *mlmetrics.Metrics

	mu    sync.RWMutex
	conns map[string]*grpc.ClientConn // key = instance ID
}

// NewServiceProxy creates a discovery-based proxy. The resolver is the
// node-level registry singleton injected by the assembly; closing it remains
// the caller's job. The routing strategy must be one Validate accepts; a
// nil resolver (registry disabled) is rejected here so a service-type config
// cannot silently run without discovery.
func NewServiceProxy(cfg *ProxyConfig, resolver *registry.Resolver) (*ServiceProxy, error) {
	if cfg.Service == "" {
		return nil, fmt.Errorf("service proxy %s: missing service name", cfg.Name)
	}
	if resolver == nil {
		return nil, fmt.Errorf("service proxy %s: no registry resolver configured (enable the registry for service-type proxies)", cfg.Name)
	}
	if cfg.GRPC == nil {
		cfg.GRPC = &GRPCProxyConfig{}
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultRPCTimeout
	}

	p := &ServiceProxy{
		name:     cfg.Name,
		service:  cfg.Service,
		timeout:  timeout,
		routing:  cfg.Routing,
		resolver: resolver,
		grpcCfg:  cfg.GRPC,
		conns:    make(map[string]*grpc.ClientConn),
	}
	switch cfg.Routing.Strategy {
	case "", RoutingStrategyConsistentHash:
		p.keyed = registry.RendezvousPicker()
		p.unkeyed = registry.RoundRobinPicker() // stateless method family
	case RoutingStrategyRoundRobin:
		p.unkeyed = registry.RoundRobinPicker()
	case RoutingStrategyRandom:
		p.unkeyed = registry.RandomPicker()
	default:
		// Unreachable when ProxyConfig.Validate ran, but fail loudly anyway.
		return nil, fmt.Errorf("service proxy %s: unknown routing.strategy %q", cfg.Name, cfg.Routing.Strategy)
	}
	return p, nil
}

// RPC implements Proxy.RPC. The instance is picked per the routing strategy:
// keyed under consistent_hash (routing key from the channel or a metadata
// entry), rotating / random for the unkeyed strategies.
func (p *ServiceProxy) RPC(ctx context.Context, req *RPCProxyRequest) (*RPCProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, func() string { return p.extractKey(req) })
	if err != nil {
		return nil, err
	}
	defer cancel()

	protoReq, err := req.ToProtoRequest()
	if err != nil {
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}

	resp, err := client.RPC(ctx, protoReq)
	if err != nil {
		return nil, fmt.Errorf("gRPC request failed: %w", err)
	}
	return FromProtoReply(resp)
}

// Authenticate implements Proxy.Authenticate. It belongs to the stateless
// family: any healthy instance can answer, so the call round-robins even
// under the keyed strategy.
func (p *ServiceProxy) Authenticate(ctx context.Context, req *AuthenticateProxyRequest) (*AuthenticateProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.Authenticate(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC authenticate failed: %w", err)
	}
	return FromProtoAuthenticateResponse(resp), nil
}

// SubscribeAcl implements Proxy.SubscribeAcl (stateless family).
func (p *ServiceProxy) SubscribeAcl(ctx context.Context, req *SubscribeAclProxyRequest) (*SubscribeAclProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.SubscribeAcl(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC subscribe acl failed: %w", err)
	}
	return FromProtoSubscribeAclResponse(resp), nil
}

// PublishAcl implements Proxy.PublishAcl (stateless family).
func (p *ServiceProxy) PublishAcl(ctx context.Context, req *PublishAclProxyRequest) (*PublishAclProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.PublishAcl(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC publish acl failed: %w", err)
	}
	return FromProtoPublishAclResponse(resp), nil
}

// OnConnected implements Proxy.OnConnected (stateless family).
func (p *ServiceProxy) OnConnected(ctx context.Context, req *OnConnectedProxyRequest) (*OnConnectedProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.OnConnected(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC on connected failed: %w", err)
	}
	return FromProtoOnConnectedResponse(resp), nil
}

// OnSubscribed implements Proxy.OnSubscribed (stateless family).
func (p *ServiceProxy) OnSubscribed(ctx context.Context, req *OnSubscribedProxyRequest) (*OnSubscribedProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.OnSubscribed(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC on subscribed failed: %w", err)
	}
	return FromProtoOnSubscribedResponse(resp), nil
}

// OnUnsubscribed implements Proxy.OnUnsubscribed (stateless family).
func (p *ServiceProxy) OnUnsubscribed(ctx context.Context, req *OnUnsubscribedProxyRequest) (*OnUnsubscribedProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.OnUnsubscribed(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC on unsubscribed failed: %w", err)
	}
	return FromProtoOnUnsubscribedResponse(resp), nil
}

// OnDisconnected implements Proxy.OnDisconnected (stateless family).
func (p *ServiceProxy) OnDisconnected(ctx context.Context, req *OnDisconnectedProxyRequest) (*OnDisconnectedProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.OnDisconnected(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC on disconnected failed: %w", err)
	}
	return FromProtoOnDisconnectedResponse(resp), nil
}

// AuthenticateAPIKey implements Proxy.AuthenticateAPIKey (stateless family).
// This keeps an api_auth × service combination valid: the Server API key
// verification rotates across the snapshot instead of pinning to one
// instance.
func (p *ServiceProxy) AuthenticateAPIKey(ctx context.Context, req *AuthenticateAPIKeyProxyRequest) (*AuthenticateAPIKeyProxyResponse, error) {
	client, ctx, cancel, err := p.clientFor(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer cancel()

	resp, err := client.AuthenticateAPIKey(ctx, req.ToProtoRequest())
	if err != nil {
		return nil, fmt.Errorf("gRPC authenticate API key failed: %w", err)
	}
	return FromProtoAuthenticateAPIKeyResponse(resp), nil
}

// SetMetrics wires the shared Prometheus metrics object (v1.1 design §4.6).
// Nil is tolerated and disables counting, so an unwired proxy never panics.
// Assembly propagates it through the same optional-interface pattern the
// broker and the cluster command bus use (Node.SetupProxy), and always before
// the router serves its first request.
func (p *ServiceProxy) SetMetrics(m *mlmetrics.Metrics) {
	p.metrics = m
}

// Name implements Proxy.Name. It returns the configured proxy name so
// ByName resolution (the api_auth assignment) keeps working; an api_auth ×
// service combination rotates the stateless family across the snapshot.
func (p *ServiceProxy) Name() string {
	return p.name
}

// Close implements Proxy.Close. It closes every pooled instance connection;
// the resolver is owned by the assembly and stays open.
func (p *ServiceProxy) Close() error {
	p.mu.Lock()
	conns := p.conns
	p.conns = make(map[string]*grpc.ClientConn)
	p.mu.Unlock()

	var errs []error
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// clientFor resolves the instance snapshot, picks one instance per the
// routing strategy, prunes connections to instances that left the snapshot,
// and returns a ProxyServiceClient bound to the picked instance's connection
// with the proxy timeout applied (the caller defers the returned cancel).
// keyOf supplies the routing key for keyed strategies; it is only invoked
// when the proxy actually runs a keyed strategy, so unkeyed strategies never
// touch it. Membership events (design §4.6) are inferred at the two
// snapshot boundaries: a leave when pruneConns closes a pooled connection,
// a join when connFor dials one for the first time.
func (p *ServiceProxy) clientFor(ctx context.Context, keyOf func() string) (proxypb.ProxyServiceClient, context.Context, context.CancelFunc, error) {
	snapshot, err := p.snapshot(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	p.pruneConns(ctx, snapshot)

	inst, err := p.selectInstance(snapshot, keyOf)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pick instance for service %s: %w", p.service, err)
	}

	conn, created, err := p.connFor(inst)
	if err != nil {
		return nil, nil, nil, err
	}
	if created {
		p.observeMembership(ctx, "join", inst.ID, conn.Target())
	}

	log.DebugContext(ctx, "proxying service gRPC request",
		"proxy", p.name,
		"service", p.service,
		"instance", inst.ID,
		"endpoint", conn.Target(),
	)

	ctx, cancel := p.withTimeout(ctx)
	return proxypb.NewProxyServiceClient(conn), ctx, cancel, nil
}

// snapshot returns the current healthy instances of the service that carry
// a grpc endpoint. An empty result is surfaced as the ErrNoHealthyInstance
// sentinel (v1.1 design §4.5). Every snapshot obtained — empty included —
// refreshes the proxy_service_instances gauge; the empty case additionally
// counts proxy_service_no_instance_total and logs a Warn (the design's ops
// signal pair for a backend fleet that is entirely gone).
func (p *ServiceProxy) snapshot(ctx context.Context) ([]registry.Instance, error) {
	insts, err := p.resolver.GetAll(ctx, p.service, registry.Filter{Protocol: registry.ProtocolGRPC})
	if err != nil {
		// A resolver whose stale snapshot aged out reports ErrNoInstance —
		// semantically "no healthy instance right now".
		if errors.Is(err, registry.ErrNoInstance) {
			p.observeNoInstance(ctx)
			return nil, fmt.Errorf("%w: service %s", ErrNoHealthyInstance, p.service)
		}
		return nil, fmt.Errorf("resolve service %s: %w", p.service, err)
	}
	p.observeInstances(len(insts))
	if len(insts) == 0 {
		p.observeNoInstance(ctx)
		return nil, fmt.Errorf("%w: service %s", ErrNoHealthyInstance, p.service)
	}
	return insts, nil
}

// selectInstance applies the routing strategy to a snapshot: a keyed
// strategy with a key supplier routes through the keyed picker; everything
// else (unkeyed strategies, the stateless method family) goes through the
// unkeyed picker.
func (p *ServiceProxy) selectInstance(snapshot []registry.Instance, keyOf func() string) (registry.Instance, error) {
	if p.keyed != nil && keyOf != nil {
		return p.keyed.Pick(keyOf(), snapshot)
	}
	return p.unkeyed.Pick(snapshot)
}

// extractKey returns the routing key for an RPC under the keyed strategy
// (v1.1 design §4.3). "channel" uses the raw channel string including its
// namespace prefix — stripping it would map the same scene segment of
// different namespaces onto one instance, while keeping it isolates them for
// free. "metadata:<name>" reads the named request metadata entry, falling
// back to the channel when it is missing or empty; the fallback counts
// proxy_service_key_fallback_total. The channel is a required field, so the
// key is always non-empty.
func (p *ServiceProxy) extractKey(req *RPCProxyRequest) string {
	if name, ok := strings.CutPrefix(p.routing.Key, RoutingKeyMetadataPrefix); ok {
		if req.Meta != nil {
			if v := req.Meta[name]; v != "" {
				return v
			}
		}
		p.observeKeyFallback()
	}
	return req.Channel
}

// connFor returns the pooled connection for an instance, dialing it lazily
// on first use. grpc.NewClient defers the actual connect to the first RPC,
// so no warm-up is needed. The created flag reports a first dial — the
// join point of the membership inference.
func (p *ServiceProxy) connFor(inst registry.Instance) (*grpc.ClientConn, bool, error) {
	p.mu.RLock()
	conn, ok := p.conns[inst.ID]
	p.mu.RUnlock()
	if ok {
		return conn, false, nil
	}

	ep, err := registry.EndpointOf(inst, registry.ProtocolGRPC)
	if err != nil {
		return nil, false, fmt.Errorf("instance %s of service %s: %w", inst.ID, p.service, err)
	}
	conn, err = grpc.NewClient(ep.Address, grpcDialOptions(p.grpcCfg)...)
	if err != nil {
		return nil, false, fmt.Errorf("dial instance %s (%s): %w", inst.ID, ep.Address, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.conns[inst.ID]; ok {
		// Another goroutine won the race; drop ours and reuse theirs. The
		// winner's join already covers the instance.
		_ = conn.Close()
		return existing, false, nil
	}
	p.conns[inst.ID] = conn
	return conn, true, nil
}

// pruneConns closes and drops connections to instances that are no longer in
// the current snapshot. It runs before every proxied call, so a removed
// instance stops receiving traffic as soon as the resolver snapshot turns
// over; in-flight RPCs on the closed conn fail and surface as PROXY_ERROR,
// matching the fail-fast semantics (v1.1 design §4.2). Every pruned
// connection is a leave membership event.
func (p *ServiceProxy) pruneConns(ctx context.Context, current []registry.Instance) {
	live := make(map[string]struct{}, len(current))
	for _, inst := range current {
		live[inst.ID] = struct{}{}
	}

	p.mu.Lock()
	var stale []pooledConn
	for id, conn := range p.conns {
		if _, ok := live[id]; !ok {
			stale = append(stale, pooledConn{id: id, conn: conn})
			delete(p.conns, id)
		}
	}
	p.mu.Unlock()

	for _, pc := range stale {
		p.observeMembership(ctx, "leave", pc.id, pc.conn.Target())
		_ = pc.conn.Close()
	}
}

// pooledConn pairs a pooled connection with its instance ID so a leave event
// can log both.
type pooledConn struct {
	id   string
	conn *grpc.ClientConn
}

// observeInstances refreshes the proxy_service_instances gauge with the
// snapshot size (0 included).
func (p *ServiceProxy) observeInstances(n int) {
	if p.metrics == nil {
		return
	}
	p.metrics.ProxyServiceInstances.WithLabelValues(p.service).Set(float64(n))
}

// observeNoInstance counts a request rejected for a missing backend fleet and
// logs the Warn side of the design §4.5 ops signal.
func (p *ServiceProxy) observeNoInstance(ctx context.Context) {
	log.WarnContext(ctx, "service proxy has no healthy instance",
		"proxy", p.name,
		"service", p.service,
	)
	if p.metrics == nil {
		return
	}
	p.metrics.ProxyServiceNoInstanceTotal.WithLabelValues(p.service).Inc()
}

// observeKeyFallback counts an RPC whose metadata routing key was missing or
// empty and fell back to the channel (keyed strategies only — extractKey is
// never invoked otherwise).
func (p *ServiceProxy) observeKeyFallback() {
	if p.metrics == nil {
		return
	}
	p.metrics.ProxyServiceKeyFallbackTotal.WithLabelValues(p.service).Inc()
}

// observeMembership records one join/leave event at the connection-pool
// boundary: the Info log carries the instance ID and endpoint, the counter
// the (service, event) labels.
func (p *ServiceProxy) observeMembership(ctx context.Context, event, instanceID, endpoint string) {
	log.InfoContext(ctx, "service proxy membership event",
		"proxy", p.name,
		"service", p.service,
		"event", event,
		"instance", instanceID,
		"endpoint", endpoint,
	)
	if p.metrics == nil {
		return
	}
	p.metrics.ProxyServiceMembershipEventsTotal.WithLabelValues(p.service, event).Inc()
}

// withTimeout applies the proxy timeout if not already set in context.
// The caller must defer the returned CancelFunc.
func (p *ServiceProxy) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return context.WithTimeout(ctx, p.timeout)
	}
	return ctx, func() {}
}
