package main

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/messageloopio/messageloop"
	"github.com/messageloopio/messageloop/config"
	"github.com/stretchr/testify/require"
)

type fakeNodeRunner struct {
	ran bool
}

func (f *fakeNodeRunner) Run(context.Context) error {
	f.ran = true
	return nil
}

func TestRunNodeWithPreflight_PreflightErrorDoesNotRunNode(t *testing.T) {
	runner := &fakeNodeRunner{}
	err := runNodeWithPreflight(context.Background(), runner, func() error {
		return errors.New("preflight failed")
	})
	require.EqualError(t, err, "preflight failed")
	require.False(t, runner.ran)
}

func TestPrepareGRPCServers_CleansUpClientListenerOnAdminFailure(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	cfg := &config.Config{
		Server: config.Server{
			GRPCAdmin: config.GRPCAdmin{Addr: addr},
		},
		Transport: config.Transport{
			GRPC: config.GRPCTransport{Addr: addr},
		},
	}

	_, err = prepareGRPCServers(cfg, messageloop.NewNode(nil))
	require.Error(t, err)

	rebound, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}

func TestPrepareGRPCServers_RequiresAdminAddr(t *testing.T) {
	cfg := &config.Config{
		Transport: config.Transport{
			GRPC: config.GRPCTransport{Addr: "127.0.0.1:0"},
		},
	}

	_, err := prepareGRPCServers(cfg, messageloop.NewNode(nil))
	require.EqualError(t, err, "grpc-admin-server addr is required")
}
