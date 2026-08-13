package messageloop

import (
	"strings"
	"sync"
)

// ACLRule defines a single access control rule for channel operations.
type ACLRule struct {
	// ChannelPattern is a glob pattern to match channels, e.g. "private.*",
	// "chat.**". Matching is segment-based (dots separate segments) and more
	// permissive than the subscription matcher: "*" matches exactly one
	// non-empty segment (consistent with the matcher), while "**" matches
	// zero or more segments and is supported only at the ACL layer.
	ChannelPattern string `yaml:"channel_pattern" json:"channel_pattern"`

	// AllowSubscribe lists user IDs allowed to subscribe. Use "*" for any authenticated user.
	AllowSubscribe []string `yaml:"allow_subscribe" json:"allow_subscribe"`

	// AllowPublish lists user IDs allowed to publish. Use "*" for any authenticated user.
	AllowPublish []string `yaml:"allow_publish" json:"allow_publish"`

	// DenyAll blocks all subscribe and publish operations on matching channels.
	DenyAll bool `yaml:"deny_all" json:"deny_all"`
}

type aclEntry struct {
	pattern        string
	allowSubscribe map[string]bool // nil means no rule; empty means deny all
	allowPublish   map[string]bool
	wildcardSub    bool // true if AllowSubscribe contains "*"
	wildcardPub    bool // true if AllowPublish contains "*"
	denyAll        bool
}

// ACLEngine evaluates channel access control rules.
type ACLEngine struct {
	mu      sync.RWMutex
	entries []aclEntry
}

// NewACLEngine creates an ACLEngine from the given rules.
func NewACLEngine(rules []ACLRule) *ACLEngine {
	entries := make([]aclEntry, 0, len(rules))
	for _, r := range rules {
		e := aclEntry{
			pattern: r.ChannelPattern,
			denyAll: r.DenyAll,
		}
		if len(r.AllowSubscribe) > 0 {
			e.allowSubscribe = make(map[string]bool, len(r.AllowSubscribe))
			for _, u := range r.AllowSubscribe {
				if u == "*" {
					e.wildcardSub = true
				}
				e.allowSubscribe[u] = true
			}
		}
		if len(r.AllowPublish) > 0 {
			e.allowPublish = make(map[string]bool, len(r.AllowPublish))
			for _, u := range r.AllowPublish {
				if u == "*" {
					e.wildcardPub = true
				}
				e.allowPublish[u] = true
			}
		}
		entries = append(entries, e)
	}
	return &ACLEngine{entries: entries}
}

// CanSubscribe returns true if userID is allowed to subscribe to the channel.
//
// Rule evaluation uses worst-match-first (most restrictive wins) semantics:
//   - If any matching rule has DenyAll set, access is denied regardless of
//     rule order, so a permissive rule can never bypass a later denyAll.
//   - Otherwise the last matching rule that specifies an allow list decides
//     (documented deterministic last-write-wins behavior); a matching rule
//     without an allow list only contributes its DenyAll flag and does not
//     affect the allow decision.
//   - If no rule matches the channel, access is allowed by default.
func (e *ACLEngine) CanSubscribe(channel, userID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	allowed := true
	for _, entry := range e.entries {
		if matchChannelPattern(entry.pattern, channel) {
			if entry.denyAll {
				return false
			}
			if entry.allowSubscribe != nil {
				allowed = entry.wildcardSub || entry.allowSubscribe[userID]
			}
		}
	}
	return allowed
}

// CanPublish returns true if userID is allowed to publish to the channel.
//
// Rule evaluation uses worst-match-first (most restrictive wins) semantics:
//   - If any matching rule has DenyAll set, access is denied regardless of
//     rule order, so a permissive rule can never bypass a later denyAll.
//   - Otherwise the last matching rule that specifies an allow list decides
//     (documented deterministic last-write-wins behavior); a matching rule
//     without an allow list only contributes its DenyAll flag and does not
//     affect the allow decision.
//   - If no rule matches the channel, access is allowed by default.
func (e *ACLEngine) CanPublish(channel, userID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	allowed := true
	for _, entry := range e.entries {
		if matchChannelPattern(entry.pattern, channel) {
			if entry.denyAll {
				return false
			}
			if entry.allowPublish != nil {
				allowed = entry.wildcardPub || entry.allowPublish[userID]
			}
		}
	}
	return allowed
}

// matchChannelPattern reports whether channel matches pattern using
// segment-based wildcard semantics that are more permissive than the
// subscription matcher:
//
//   - segments are separated by "."; each pattern segment must match the
//     corresponding channel segment
//   - "*" matches exactly one non-empty segment, consistent with the
//     subscription matcher ("chat.*" matches "chat.room" but not
//     "chat.room.sub")
//   - "**" matches zero or more segments; this is supported only at the ACL
//     layer, the subscription matcher has no equivalent, so "chat.**" there
//     is treated as a literal channel name
//   - any other segment matches only an identical channel segment
//
// This deliberately replaces path.Match, whose "*" matches across dots,
// making "chat.*" and "chat.**" behave identically and failing to match the
// CSTrie single-segment wildcard used for subscription routing.
func matchChannelPattern(pattern, channel string) bool {
	return matchSegments(strings.Split(pattern, "."), strings.Split(channel, "."))
}

func matchSegments(pattern, channel []string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case "**":
			// Try every split point: "**" may consume zero or more segments.
			for i := 0; i <= len(channel); i++ {
				if matchSegments(pattern[1:], channel[i:]) {
					return true
				}
			}
			return false
		case "*":
			if len(channel) == 0 {
				return false
			}
			pattern, channel = pattern[1:], channel[1:]
		default:
			if len(channel) == 0 || pattern[0] != channel[0] {
				return false
			}
			pattern, channel = pattern[1:], channel[1:]
		}
	}
	return len(channel) == 0
}
