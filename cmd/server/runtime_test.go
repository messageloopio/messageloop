package main

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"
	"github.com/messageloopio/messageloop/internal/runtime"
)

func TestPrepareGRPCServers_CleansUpClientListenerOnAPIFailure(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	cfg := &config.Config{
		Server: config.Server{
			API: config.ServerAPI{Addr: addr},
		},
		Transport: config.Transport{
			GRPC: config.GRPCTransport{Addr: addr},
		},
	}

	_, err = prepareGRPCServers(cfg, runtime.NewNode(nil), nil, nil)
	require.Error(t, err)

	rebound, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}

func TestPrepareGRPCServers_RequiresAPIAddr(t *testing.T) {
	cfg := &config.Config{
		Transport: config.Transport{
			GRPC: config.GRPCTransport{Addr: "127.0.0.1:0"},
		},
	}

	_, err := prepareGRPCServers(cfg, runtime.NewNode(nil), nil, nil)
	require.EqualError(t, err, "grpc-api-server addr is required")
}
