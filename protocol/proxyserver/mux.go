package proxyserver

import (
	"context"
	"errors"
	"sync"

	"github.com/gobwas/glob"

	proxypb "github.com/messageloopio/messageloop/protocol/genproto/messageloop/proxy/v2"
)

// Middleware wraps an RPCHandlerFunc. Middleware registered via Use runs in
// registration order around the mux dispatch (first registered is outermost).
type Middleware func(next RPCHandlerFunc) RPCHandlerFunc

// muxRoute is one compiled (channel, method) routing rule.
type muxRoute struct {
	channel *glob.Pattern
	method  *glob.Pattern
	handler RPCHandlerFunc
}

// RPCMux routes forwarded RPC requests to handlers by (channel, method) glob
// patterns, mirroring the routing semantics of the MessageLoop server-side
// proxy router: patterns are gobwas/glob, routes are evaluated in
// registration order and the first match wins. An empty pattern is rejected
// at Handle time; use "*" (or a concrete prefix pattern like "chat.room*")
// to cover channels.
//
// RPCMux is safe for concurrent use: Handle may be called while requests are
// in flight (new routes are visible to subsequent requests).
type RPCMux struct {
	mu          sync.RWMutex
	routes      []muxRoute
	middlewares []Middleware
	chain       RPCHandlerFunc
}

// NewRPCMux creates an empty mux.
func NewRPCMux() *RPCMux {
	return &RPCMux{}
}

// Use registers middleware around the dispatch. Middleware applies to all
// routes, including ones registered earlier.
func (m *RPCMux) Use(mw ...Middleware) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.middlewares = append(m.middlewares, mw...)
	m.chain = nil
}

// Handle registers a handler for the first (channel, method) pattern pair
// that matches a request. Both patterns are compiled before the route is
// committed, so a bad pattern leaves the mux unchanged.
func (m *RPCMux) Handle(channelPattern, methodPattern string, handler RPCHandlerFunc) error {
	if channelPattern == "" || methodPattern == "" {
		return errors.New("proxyserver: empty channel/method pattern never matches")
	}
	channelGlob, err := glob.Compile(channelPattern)
	if err != nil {
		return err
	}
	methodGlob, err := glob.Compile(methodPattern)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes = append(m.routes, muxRoute{channel: channelGlob, method: methodGlob, handler: handler})
	return nil
}

// HandleMethod registers a handler for a method pattern across all channels.
func (m *RPCMux) HandleMethod(methodPattern string, handler RPCHandlerFunc) error {
	return m.Handle("*", methodPattern, handler)
}

// HandleRPC implements RPCHandler: it dispatches to the first route matching
// the request's channel and method. No match returns ErrNoRoute (which the
// Server bridges to an in-band NO_ROUTE reply error).
func (m *RPCMux) HandleRPC(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
	return m.chained()(ctx, req)
}

// match returns the handler of the first route matching channel and method.
func (m *RPCMux) match(channel, method string) (RPCHandlerFunc, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, rt := range m.routes {
		if rt.channel.Match(channel) && rt.method.Match(method) {
			return rt.handler, true
		}
	}
	return nil, false
}

// chained returns the middleware-wrapped dispatch, rebuilding and caching it
// until the middleware set changes. Route matching happens inside the chain,
// so newly registered routes are picked up without rebuilding.
func (m *RPCMux) chained() RPCHandlerFunc {
	m.mu.RLock()
	chain := m.chain
	m.mu.RUnlock()
	if chain != nil {
		return chain
	}

	m.mu.RLock()
	mws := make([]Middleware, len(m.middlewares))
	copy(mws, m.middlewares)
	m.mu.RUnlock()

	inner := RPCHandlerFunc(func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		handler, ok := m.match(req.GetChannel(), req.GetMethod())
		if !ok {
			return nil, ErrNoRoute
		}
		return handler(ctx, req)
	})
	for i := len(mws) - 1; i >= 0; i-- {
		inner = mws[i](inner)
	}

	m.mu.Lock()
	// Only cache when the middleware set has not moved underneath us; a
	// stale cache is harmless (it is rebuilt on the next request).
	if len(mws) == len(m.middlewares) {
		m.chain = inner
	}
	m.mu.Unlock()
	return inner
}
