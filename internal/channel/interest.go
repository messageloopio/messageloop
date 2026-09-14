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
//  3. Split on "." and ":" (topics.SplitSegments). The final segment must be
//     "*" or "**", and every segment before it must be literal (no
//     "*"/"**"). Otherwise ErrPatternNotRoutable.
//  4. Empty literal prefix (key is "*" or "**") → ErrPatternNotRoutable (it
//     would degrade to a cluster-wide PSubscribe, KD-K13).
//  5. Prefix = the literal segments rejoined per the namespace grammar
//     (joinPrefix); the glob suffix keeps the key's namespace scoping:
//     - "im.*"       → Pattern "im.*"
//     - "acme:im.*"  → Pattern "acme:im.*"
//     - "acme:*"     → Pattern "acme:*"
//     - Final "**" additionally sets AlsoExact to the prefix ("acme:im.**"
//       → AlsoExact "acme:im", covering the zero-segment case).
func CompileInterest(key string) (CompiledInterest, error) {
	if err := topics.ValidateTopic(key); err != nil {
		return CompiledInterest{}, err
	}
	if !strings.Contains(key, "*") {
		return CompiledInterest{Exact: key}, nil
	}

	segments := topics.SplitSegments(key)
	last := segments[len(segments)-1]
	if last != "*" && last != "**" {
		return CompiledInterest{}, ErrPatternNotRoutable
	}
	for _, seg := range segments[:len(segments)-1] {
		if strings.Contains(seg, "*") {
			return CompiledInterest{}, ErrPatternNotRoutable
		}
	}
	prefix, sep := joinPrefix(key, segments[:len(segments)-1])
	if prefix == "" {
		return CompiledInterest{}, ErrPatternNotRoutable
	}

	ci := CompiledInterest{Pattern: prefix + sep + "*"}
	if last == "**" {
		ci.AlsoExact = prefix
	}
	return ci, nil
}

// joinPrefix reassembles the literal prefix segments of a subscription key
// and returns the delimiter that continues the compiled glob after the
// prefix. Namespaced keys (exactly one ":" per the channel grammar) keep the
// namespace at the head of the prefix: "acme:im.*" rejoins as "acme:im" and
// continues with "." ("acme:im.*"), while a bare namespace prefix ("acme:*")
// rejoins as "acme" and continues with ":" so the glob stays scoped to the
// namespace ("acme:*"). Un-namespaced keys rejoin with "." as before.
func joinPrefix(key string, prefixSegs []string) (prefix, sep string) {
	const dot = "."
	sep = dot
	if !strings.Contains(key, topics.NSDelimiter) {
		return strings.Join(prefixSegs, dot), sep
	}
	if len(prefixSegs) == 1 {
		return prefixSegs[0], topics.NSDelimiter
	}
	return prefixSegs[0] + topics.NSDelimiter + strings.Join(prefixSegs[1:], dot), sep
}

// MatchAfterCompile reports whether a subscription key (exact channel or
// routable pattern) covers the concrete channel under segment semantics. It
// uses the same segment matching as the topic matchers (topics.Match), so the
// Redis glob over-match ("im.room.*" also matches "im.room.a.b" because Redis
// "*" crosses dots) is discarded locally.
func MatchAfterCompile(key, concrete string) bool {
	return topics.Match(key, concrete)
}
