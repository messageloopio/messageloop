package authz

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/config"
)

// TestCapabilityNameCensus extends the census red-line to the one mirror the
// Server API design's census philosophy did not cover: config.Validate keeps
// its own copy of the closed capability-name set because authz imports
// config (config.AuthorizerConfig) and cannot be imported back. This test is
// the structural pin that replaces the "keep the two lists in sync" comments:
// a capability added or renamed on either side without the other fails here.
func TestCapabilityNameCensus(t *testing.T) {
	require.NotEmpty(t, ClosedCapabilityNames, "closed capability set must not be empty")
	require.NotEmpty(t, config.CapabilityNames, "config capability set must not be empty")

	authzNames := make([]string, 0, len(ClosedCapabilityNames))
	for name := range ClosedCapabilityNames {
		authzNames = append(authzNames, name)
	}
	sort.Strings(authzNames)

	configNames := make([]string, 0, len(config.CapabilityNames))
	for name := range config.CapabilityNames {
		configNames = append(configNames, name)
	}
	sort.Strings(configNames)

	assert.Equal(t, configNames, authzNames,
		"authz.ClosedCapabilityNames and config.CapabilityNames must accept exactly the same names")
}

// TestParseCapabilityNames pins the shared fold: known names OR into bits,
// unknown names are dropped toward safety, empty stays zero.
func TestParseCapabilityNames(t *testing.T) {
	assert.Equal(t, Capability(0), ParseCapabilityNames(nil))
	assert.Equal(t, Capability(0), ParseCapabilityNames([]string{"nope", "nada"}))
	assert.Equal(t, CapHistoryRead|CapPresenceRead, ParseCapabilityNames([]string{"history.read", "presence.read", "brand.new.cap"}))
	assert.Equal(t, DefaultCapabilityCeiling, ParseCapabilityNames([]string{
		"presence.large_snapshot", "survey.bypass_gate", "history.read", "presence.read",
		"channels.list", "session.act", "user.fanout", "subscribe.any",
	}))
}
