// Package channel holds the subscription-interest contract
// (CompileInterest/CompiledInterest/MatchAfterCompile) sunk from the root
// package in PR-KA-D11 (KD-K26 phase one; target layout:
// docs/v2/kernel-architecture.md :173-191) and the channel-policy contract
// (ChannelPolicy/CompiledPolicySpec) sunk in PR-KA-D12 (phase two).
package channel

import (
	"errors"
	"strings"

	"github.com/messageloopio/messageloop/pkg/topics"
)

// ErrPatternNotRoutable is returned by CompileInterest (and by the broker
// Subscribe entry point) when a subscription key cannot be routed on the live
// bus: Redis Pub/Sub can only subscribe exact channels and literal-prefix
// glob patterns (KD-K13). Examples: "*.room", "im.*.tick", and bare "*"/"**"
// (which would degrade to a cluster-wide PSubscribe).
var ErrPatternNotRoutable = errors.New("pattern is not routable on the live bus")

// CompiledInterest is the Redis-routable form of one subscription key: an
// exact channel, a literal-prefix glob pattern, and (for a trailing "**"
// suffix) an extra exact channel covering the zero-segment case.
type CompiledInterest struct {
	// Exact is the concrete channel name (no pubsub prefix). Empty if none.
	Exact string
	// Pattern is the Redis glob WITHOUT prefix, or empty.
	// Example: key "im.**" → Pattern "im.*"
	Pattern string
	// AlsoExact is an extra exact subscribe (for trailing ** zero-segment).
	// Example: "im.**" → AlsoExact "im"
	AlsoExact string
}

// CompileInterest compiles one subscription key into its routable form on the
// live bus. The rules are fixed and shared by the memory and Redis brokers:
//
//  1. topics.ValidateTopic(key) failure → the original ErrBadTopic (not
//     NotRoutable).
//  2. No "*" → Exact=key.
//  3. Split on ".". The final segment must be "*" or "**", and every segment
//     before it must be literal (no "*"/"**"). Otherwise
//     ErrPatternNotRoutable.
//  4. Empty literal prefix (key is "*" or "**") → ErrPatternNotRoutable (it
//     would degrade to a cluster-wide PSubscribe, KD-K13).
//  5. Prefix = the literal segments joined with ".".
//     - Final "*" → Pattern = prefix+".*" (Redis glob).
//     - Final "**" → Pattern = prefix+".*", AlsoExact = prefix.
func CompileInterest(key string) (CompiledInterest, error) {
	if err := topics.ValidateTopic(key); err != nil {
		return CompiledInterest{}, err
	}
	if !strings.Contains(key, "*") {
		return CompiledInterest{Exact: key}, nil
	}

	segments := strings.Split(key, ".")
	last := segments[len(segments)-1]
	if last != "*" && last != "**" {
		return CompiledInterest{}, ErrPatternNotRoutable
	}
	for _, seg := range segments[:len(segments)-1] {
		if strings.Contains(seg, "*") {
			return CompiledInterest{}, ErrPatternNotRoutable
		}
	}
	prefix := strings.Join(segments[:len(segments)-1], ".")
	if prefix == "" {
		return CompiledInterest{}, ErrPatternNotRoutable
	}

	ci := CompiledInterest{Pattern: prefix + ".*"}
	if last == "**" {
		ci.AlsoExact = prefix
	}
	return ci, nil
}

// MatchAfterCompile reports whether a subscription key (exact channel or
// routable pattern) covers the concrete channel under segment semantics. It
// uses the same segment matching as the topic matchers (topics.Match), so the
// Redis glob over-match ("im.room.*" also matches "im.room.a.b" because Redis
// "*" crosses dots) is discarded locally.
func MatchAfterCompile(key, concrete string) bool {
	return topics.Match(key, concrete)
}
