package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validBatching returns a BatchingConfig that passes the frame-budget gate
// against the 64KB default max_message_size.
func validBatching() *BatchingConfig {
	return &BatchingConfig{}
}

func batchingConfig(b *BatchingConfig) *Config {
	cfg := &Config{
		Transport: validTransport(),
		Server:    validServer(),
	}
	cfg.Server.Broadcast = b
	return cfg
}

func TestBatchingConfig_ResolveDefaults(t *testing.T) {
	s, err := validBatching().Resolve()
	require.NoError(t, err)
	assert.Equal(t, DefaultBatchMaxDelay, s.MaxDelay)
	assert.Equal(t, DefaultBatchMaxMessages, s.MaxMessages)
	assert.Equal(t, DefaultBatchMaxBytes, s.MaxBatchBytes)
	assert.Equal(t, DefaultBatchMinSubscribers, s.MinSubscribers)
	assert.Equal(t, DefaultBatchIdleReap, s.IdleReap)
}

func TestBatchingConfig_ResolveOverrides(t *testing.T) {
	s, err := (&BatchingConfig{
		MaxDelay:       "15ms",
		MaxMessages:    8,
		MaxBatchBytes:  4096,
		MinSubscribers: 100,
		IdleReap:       "1m",
	}).Resolve()
	require.NoError(t, err)
	assert.Equal(t, 15*time.Millisecond, s.MaxDelay)
	assert.Equal(t, 8, s.MaxMessages)
	assert.Equal(t, 4096, s.MaxBatchBytes)
	assert.Equal(t, 100, s.MinSubscribers)
	assert.Equal(t, time.Minute, s.IdleReap)
}

func TestBatchingConfig_ResolveInvalid(t *testing.T) {
	cases := []struct {
		name string
		cfg  BatchingConfig
	}{
		{"zero delay", BatchingConfig{MaxDelay: "0s"}},
		{"negative delay", BatchingConfig{MaxDelay: "-5ms"}},
		{"oversize delay", BatchingConfig{MaxDelay: "1s"}},
		{"garbage delay", BatchingConfig{MaxDelay: "soon"}},
		{"one message", BatchingConfig{MaxMessages: 1}},
		{"tiny bytes", BatchingConfig{MaxBatchBytes: 512}},
		{"garbage idle_reap", BatchingConfig{IdleReap: "forever"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Resolve()
			assert.Error(t, err)
		})
	}
}

func TestValidate_BroadcastBatching(t *testing.T) {
	// A nil broadcast block stays valid (batching disabled).
	cfg := &Config{Transport: validTransport(), Server: validServer()}
	assert.NoError(t, cfg.Validate())

	// Defaults pass the frame-budget gate against the 64KB default
	// max_message_size.
	assert.NoError(t, batchingConfig(validBatching()).Validate())

	// The parameter matrix flows through Resolve.
	for _, tc := range []struct {
		name string
		cfg  BatchingConfig
	}{
		{"zero delay", BatchingConfig{MaxDelay: "0s"}},
		{"oversize delay", BatchingConfig{MaxDelay: "600ms"}},
		{"one message", BatchingConfig{MaxMessages: 1}},
		{"tiny bytes", BatchingConfig{MaxBatchBytes: 100}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Error(t, batchingConfig(&tc.cfg).Validate())
		})
	}

	// Frame-budget gate: 2 × max_batch_bytes must fit max_message_size.
	t.Run("batch bytes exceed max_message_size", func(t *testing.T) {
		cfg := batchingConfig(&BatchingConfig{MaxBatchBytes: 40 * 1024}) // 2×40KiB > 64KiB
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_message_size")
	})

	// The gate measures the explicit max_message_size, not the default.
	t.Run("fits explicit max_message_size", func(t *testing.T) {
		cfg := batchingConfig(&BatchingConfig{MaxBatchBytes: 40 * 1024})
		cfg.Server.Limits.MaxMessageSize = 128 * 1024
		assert.NoError(t, cfg.Validate())
	})
}

func TestValidate_ChannelPolicySpecBatching(t *testing.T) {
	// An authorizer rule carrying only the batching override must count as a
	// set spec (it matters for rejecting the removed server.channels block)
	// and must validate cleanly in the authorizer table.
	on := true
	cfg := &Config{
		Transport: validTransport(),
		Server:    validServer(),
	}
	cfg.Server.Authorizer.Rules = []AuthorizerRule{
		{Pattern: "game.*", ChannelPolicySpec: ChannelPolicySpec{Batching: &on}},
	}
	assert.NoError(t, cfg.Validate())
	assert.True(t, channelPolicySpecSet(ChannelPolicySpec{Batching: &on}))
	assert.False(t, channelPolicySpecSet(ChannelPolicySpec{}))
}
