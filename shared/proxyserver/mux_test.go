package proxyserver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	proxypb "github.com/messageloopio/messageloop/shared/genproto/messageloop/proxy/v2"
	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/messageloop/shared/v2"
)

func echo(method string) RPCHandlerFunc {
	return func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
		return &proxypb.RPCResponse{Id: req.GetId(), Payload: req.GetPayload(), Metadata: &sharedv2.Metadata{Entries: map[string]string{"method": method}}}, nil
	}
}

func call(mux *RPCMux, channel, method string) (*proxypb.RPCResponse, error) {
	return mux.HandleRPC(context.Background(), &proxypb.RPCRequest{Channel: channel, Method: method})
}

func TestRPCMuxChannelMethodRouting(t *testing.T) {
	assert := assert.New(t)
	mux := NewRPCMux()

	require.NoError(t, mux.Handle("chat.*", "send", echo("chat-send")))
	require.NoError(t, mux.Handle("chat.*", "*", echo("chat-any")))
	require.NoError(t, mux.HandleMethod("ping", echo("ping")))

	resp, err := call(mux, "chat.room1", "send")
	require.NoError(t, err)
	assert.Equal("chat-send", resp.Metadata.Entries["method"], "first registered match wins")

	resp, err = call(mux, "chat.room1", "other")
	require.NoError(t, err)
	assert.Equal("chat-any", resp.Metadata.Entries["method"])

	resp, err = call(mux, "game.room1", "ping")
	require.NoError(t, err)
	assert.Equal("ping", resp.Metadata.Entries["method"], "HandleMethod covers all channels")

	_, err = call(mux, "game.room1", "unknown")
	assert.True(errors.Is(err, ErrNoRoute))
}

func TestRPCMuxRegistrationAtomicity(t *testing.T) {
	assert := assert.New(t)
	mux := NewRPCMux()
	require.NoError(t, mux.Handle("*", "known", echo("known")))

	assert.Error(mux.Handle("[bad", "*", echo("never")))
	assert.Error(mux.Handle("*", "a[b", echo("never")))
	assert.Error(mux.Handle("", "*", echo("never")))

	_, err := call(mux, "any.channel", "known")
	require.NoError(t, err, "failed registrations must leave the mux unchanged")

	_, err = call(mux, "any.channel", "unknown")
	assert.True(errors.Is(err, ErrNoRoute))
}

func TestRPCMuxMiddlewareOrder(t *testing.T) {
	assert := assert.New(t)
	mux := NewRPCMux()
	var order []string

	mux.Use(func(next RPCHandlerFunc) RPCHandlerFunc {
		return func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
			order = append(order, "outer-in")
			resp, err := next(ctx, req)
			order = append(order, "outer-out")
			return resp, err
		}
	})
	mux.Use(func(next RPCHandlerFunc) RPCHandlerFunc {
		return func(ctx context.Context, req *proxypb.RPCRequest) (*proxypb.RPCResponse, error) {
			order = append(order, "inner-in")
			return next(ctx, req)
		}
	})

	// A route registered before the middleware still gets wrapped.
	require.NoError(t, mux.Handle("*", "m", echo("m")))
	_, err := call(mux, "ch", "m")
	require.NoError(t, err)
	assert.Equal([]string{"outer-in", "inner-in", "outer-out"}, order)
}

func TestRPCMuxLateRouteVisible(t *testing.T) {
	assert := assert.New(t)
	mux := NewRPCMux()

	// First request builds and caches the middleware chain.
	_, err := call(mux, "ch", "m")
	assert.True(errors.Is(err, ErrNoRoute))

	require.NoError(t, mux.Handle("*", "m", echo("m")))
	resp, err := call(mux, "ch", "m")
	require.NoError(t, err, "routes registered after the chain was cached must route")
	assert.Equal("m", resp.Metadata.Entries["method"])
}

func TestRPCMuxConcurrentUse(t *testing.T) {
	mux := NewRPCMux()
	require.NoError(t, mux.HandleMethod("ping", echo("ping")))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = mux.HandleMethod(string(rune('a'+i%26))+".method", echo("x"))
		}
	}()
	for i := 0; i < 200; i++ {
		_, err := call(mux, "ch", "ping")
		require.NoError(t, err)
	}
	<-done
}
