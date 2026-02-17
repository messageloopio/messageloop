package messageloopgo

import (
	"context"
	"fmt"

	sharedpb "github.com/messageloopio/messageloop/shared/genproto/shared/v1"
)

// RPCHandlerFunc is the function signature for RPC handlers.
type RPCHandlerFunc func(ctx context.Context, req *RPCRequest) (*RPCResponse, error)

// RPCMiddleware is a function that wraps an RPCHandlerFunc.
// It can perform operations before/after calling the next handler.
type RPCMiddleware func(next RPCHandlerFunc) RPCHandlerFunc

// RPCMux is a multiplexer for RPC method routing with middleware support.
// It implements the RPCHandler interface, so it can be used anywhere
// an RPCHandler is expected.
type RPCMux struct {
	handlers    map[string]RPCHandlerFunc
	middlewares []RPCMiddleware
}

// NewRPCMux creates a new RPC multiplexer.
func NewRPCMux() *RPCMux {
	return &RPCMux{
		handlers:    make(map[string]RPCHandlerFunc),
		middlewares: nil,
	}
}

// Handle registers a handler for the given method.
// If a handler is already registered for the method, it will be replaced.
func (m *RPCMux) Handle(method string, handler RPCHandlerFunc) {
	m.handlers[method] = handler
}

// Use registers middleware that wraps all handlers.
// Middleware is applied in registration order (first registered = outermost).
// For example: Use(m1), Use(m2) results in: m1 -> m2 -> handler
func (m *RPCMux) Use(middleware RPCMiddleware) {
	m.middlewares = append(m.middlewares, middleware)
}

// HandleRPC implements RPCHandler interface.
// Routes to registered handler, returns UNKNOWN_METHOD error if not found.
func (m *RPCMux) HandleRPC(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
	handler, ok := m.handlers[req.Method]
	if !ok {
		return &RPCResponse{
			Error: &sharedpb.Error{
				Code:    "UNKNOWN_METHOD",
				Type:    "rpc_error",
				Message: fmt.Sprintf("Unknown method: %s", req.Method),
			},
		}, nil
	}

	// Apply middlewares in reverse order so that first registered is outermost
	wrapped := handler
	for i := len(m.middlewares) - 1; i >= 0; i-- {
		wrapped = m.middlewares[i](wrapped)
	}

	return wrapped(ctx, req)
}
