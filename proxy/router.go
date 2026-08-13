package proxy

import (
	"errors"
	"sync"

	"github.com/gobwas/glob"
)

// ErrNoProxyFound is returned when no matching proxy is found for a channel/method.
var ErrNoProxyFound = errors.New("no proxy found for channel/method")

// Router matches RPC requests to their configured proxies based on channel and method patterns.
type Router struct {
	mu     sync.RWMutex
	routes []*route
}

// route represents a single routing rule.
type route struct {
	proxy          Proxy
	channelMatcher glob.Glob
	methodMatcher  glob.Glob
}

// NewRouter creates a new empty Router.
func NewRouter() *Router {
	return &Router{
		routes: make([]*route, 0),
	}
}

// Add adds a new route to the router. Routes are evaluated in the order they were added.
// The first matching route is returned.
func (r *Router) Add(proxy Proxy, channelPattern, methodPattern string) error {
	channelGlob, err := glob.Compile(channelPattern)
	if err != nil {
		return err
	}

	methodGlob, err := glob.Compile(methodPattern)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.routes = append(r.routes, &route{
		proxy:          proxy,
		channelMatcher: channelGlob,
		methodMatcher:  methodGlob,
	})

	return nil
}

// Match finds the proxy that matches the given channel and method.
// Returns nil if no match is found.
func (r *Router) Match(channel, method string) Proxy {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, rt := range r.routes {
		if rt.channelMatcher.Match(channel) && rt.methodMatcher.Match(method) {
			return rt.proxy
		}
	}

	return nil
}

// AddFromConfig adds routes from a ProxyConfig. All patterns are compiled
// before any route is committed, so a failure leaves the router unchanged
// instead of half-initialized.
func (r *Router) AddFromConfig(proxy Proxy, cfg *ProxyConfig) error {
	type compiledRoute struct {
		channel glob.Glob
		method  glob.Glob
	}
	compiled := make([]compiledRoute, 0, len(cfg.Routes))
	for _, routeCfg := range cfg.Routes {
		channelGlob, err := glob.Compile(routeCfg.Channel)
		if err != nil {
			return err
		}
		methodGlob, err := glob.Compile(routeCfg.Method)
		if err != nil {
			return err
		}
		compiled = append(compiled, compiledRoute{channel: channelGlob, method: methodGlob})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cr := range compiled {
		r.routes = append(r.routes, &route{
			proxy:          proxy,
			channelMatcher: cr.channel,
			methodMatcher:  cr.method,
		})
	}
	return nil
}

// Close closes all proxies registered in the router.
func (r *Router) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var closeErrs []error
	for _, rt := range r.routes {
		if err := rt.proxy.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	r.routes = nil
	return errors.Join(closeErrs...)
}
