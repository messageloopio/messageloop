package messageloop

import (
	"errors"
	"time"

	"github.com/messageloopio/messageloop/config"
)

// ErrHistoryDisabled is returned by Node.Publish when the channel policy
// disables history for the channel (transient_only or history=false).
// Clients never see this error: handlePublish routes those channels through
// PublishTransient and acks offset 0 instead.
var ErrHistoryDisabled = errors.New("channel policy: history disabled for channel")

// ChannelPolicy is the effective per-channel behavior. Effects are resolved
// by the Authorizer (server.authorizer table), no longer by a separate
// first-match policy engine (PR-KA-A4 §5.5). Zero HistorySize and HistoryTTL
// mean "use the broker global default" (memory 256 / Redis stream_max_length
// and Redis history_ttl).
type ChannelPolicy struct {
	History               bool
	HistorySize           int           // 0 = broker global
	HistoryTTL            time.Duration // 0 = broker global; memory broker ignores it
	Presence              bool
	Recover               bool
	Survey                bool
	TransientOnly         bool
	RecoverLimit          int
	MaxSurveySubscribers  int
	MaxSurveyTimeout      time.Duration
	LegacyPresenceChannel bool
	PresenceSnapshotLimit int
}

// DefaultChannelPolicy returns the pre-policy behavior: history on, presence
// on, recover on, client survey off (KD-6), and the documented caps.
func DefaultChannelPolicy() ChannelPolicy {
	return ChannelPolicy{
		History:               true,
		Presence:              true,
		Recover:               true,
		Survey:                false,
		MaxSurveySubscribers:  256,
		MaxSurveyTimeout:      5 * time.Second,
		PresenceSnapshotLimit: 256,
	}
}

// compiledPolicySpec is the pre-parsed form of config.ChannelPolicySpec.
// Pointer fields distinguish "not overridden" (nil) from an explicit value;
// the two duration fields carry an explicit "set" flag because "0s" is a
// valid override that must not be confused with "unset".
type compiledPolicySpec struct {
	history               *bool
	historySize           *int
	historyTTL            time.Duration
	historyTTLSet         bool
	presence              *bool
	recover               *bool
	survey                *bool
	transientOnly         *bool
	recoverLimit          *int
	maxSurveySubscribers  *int
	maxSurveyTimeout      time.Duration
	maxSurveyTimeoutSet   bool
	legacyPresenceChannel *bool
	presenceSnapshotLimit *int
}

// compilePolicySpec converts a config spec into the parsed overlay form.
// Duration parse failures (already rejected by config.Validate) fall back to
// "not overridden" and are ignored by the overlay.
func compilePolicySpec(spec config.ChannelPolicySpec) compiledPolicySpec {
	compiled := compiledPolicySpec{
		history:               spec.History,
		historySize:           spec.HistorySize,
		presence:              spec.Presence,
		recover:               spec.Recover,
		survey:                spec.Survey,
		transientOnly:         spec.TransientOnly,
		recoverLimit:          spec.RecoverLimit,
		maxSurveySubscribers:  spec.MaxSurveySubscribers,
		legacyPresenceChannel: spec.LegacyPresenceChannel,
		presenceSnapshotLimit: spec.PresenceSnapshotLimit,
	}
	if spec.HistoryTTL != "" {
		if d, err := time.ParseDuration(spec.HistoryTTL); err == nil {
			compiled.historyTTL = d
			compiled.historyTTLSet = true
		}
	}
	if spec.MaxSurveyTimeout != "" {
		if d, err := time.ParseDuration(spec.MaxSurveyTimeout); err == nil {
			compiled.maxSurveyTimeout = d
			compiled.maxSurveyTimeoutSet = true
		}
	}
	return compiled
}

// overlay applies the non-nil fields of spec on top of pol and returns the
// result.
func overlay(pol ChannelPolicy, spec compiledPolicySpec) ChannelPolicy {
	if spec.history != nil {
		pol.History = *spec.history
	}
	if spec.historySize != nil {
		pol.HistorySize = *spec.historySize
	}
	if spec.historyTTLSet {
		pol.HistoryTTL = spec.historyTTL
	}
	if spec.presence != nil {
		pol.Presence = *spec.presence
	}
	if spec.recover != nil {
		pol.Recover = *spec.recover
	}
	if spec.survey != nil {
		pol.Survey = *spec.survey
	}
	if spec.transientOnly != nil {
		pol.TransientOnly = *spec.transientOnly
	}
	if spec.recoverLimit != nil {
		pol.RecoverLimit = *spec.recoverLimit
	}
	if spec.maxSurveySubscribers != nil {
		pol.MaxSurveySubscribers = *spec.maxSurveySubscribers
	}
	if spec.maxSurveyTimeoutSet {
		pol.MaxSurveyTimeout = spec.maxSurveyTimeout
	}
	if spec.legacyPresenceChannel != nil {
		pol.LegacyPresenceChannel = *spec.legacyPresenceChannel
	}
	if spec.presenceSnapshotLimit != nil {
		pol.PresenceSnapshotLimit = *spec.presenceSnapshotLimit
	}
	return pol
}
