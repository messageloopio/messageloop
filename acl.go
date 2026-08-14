package messageloop

import (
	"strings"
	"sync"
)

// ACLRule defines a single access control rule for channel operations.
type ACLRule struct {
	// ChannelPattern is a glob pattern to match channels, e.g. "private.*",
	// "chat.**". Matching is segment-based (dots separate segments): "*"
	// matches exactly one non-empty segment (consistent with the matcher),
	// while "**" matches zero or more segments. The matcher only supports
	// "**" as the final segment; ACL patterns additionally allow it in the
	// middle (e.g. "a.**.b"), making the ACL layer more permissive.
	ChannelPattern string `yaml:"channel_pattern" json:"channel_pattern"`

	// AllowSubscribe lists user IDs allowed to subscribe. Use "*" for any authenticated user.
	AllowSubscribe []string `yaml:"allow_subscribe" json:"allow_subscribe"`

	// AllowPublish lists user IDs allowed to publish. Use "*" for any authenticated user.
	AllowPublish []string `yaml:"allow_publish" json:"allow_publish"`

	// AllowSurvey lists user IDs allowed to initiate client surveys. Use "*"
	// for any authenticated user. Unset means the rule does not open survey;
	// CanSurvey defaults to deny, unlike subscribe/publish.
	AllowSurvey []string `yaml:"allow_survey" json:"allow_survey"`

	// DenyAll blocks subscribe, publish, and client survey on matching channels.
	DenyAll bool `yaml:"deny_all" json:"deny_all"`
}

type aclEntry struct {
	pattern        string
	allowSubscribe map[string]bool // nil means no rule; empty means deny all
	allowPublish   map[string]bool
	allowSurvey    map[string]bool
	wildcardSub    bool // true if AllowSubscribe contains "*"
	wildcardPub    bool // true if AllowPublish contains "*"
	wildcardSurvey bool // true if AllowSurvey contains "*"
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
		if len(r.AllowSurvey) > 0 {
			e.allowSurvey = make(map[string]bool, len(r.AllowSurvey))
			for _, u := range r.AllowSurvey {
				if u == "*" {
					e.wildcardSurvey = true
				}
				e.allowSurvey[u] = true
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

// CanSurvey returns true if userID is allowed to initiate a client survey on
// the channel. Same structure as CanSubscribe (denyAll short-circuits, the
// last matching rule with an allow list wins), but the default is deny:
//   - No matching rule, or a matching rule that only lists allow_subscribe /
//     allow_publish without allow_survey, never opens survey (allowed=false).
//   - denyAll on any matching rule denies regardless of rule order.
//   - The last matching rule that specifies an allow_survey list decides.
//
// Admin Node.Survey does NOT go through CanSurvey.
func (e *ACLEngine) CanSurvey(channel, userID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	allowed := false
	for _, entry := range e.entries {
		if matchChannelPattern(entry.pattern, channel) {
			if entry.denyAll {
				return false
			}
			if entry.allowSurvey != nil {
				allowed = entry.wildcardSurvey || entry.allowSurvey[userID]
			}
		}
	}
	return allowed
}

// matchChannelPattern reports whether channel matches pattern using
// segment-based wildcard semantics:
//
//   - segments are separated by "."; each pattern segment must match the
//     corresponding channel segment
//   - "*" matches exactly one non-empty segment, consistent with the
//     subscription matcher ("chat.*" matches "chat.room" but not
//     "chat.room.sub")
//   - "**" matches zero or more segments, consistent with the matcher's
//     trailing "**"; unlike the matcher, ACL patterns may also place "**" in
//     the middle (e.g. "a.**.b")
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
