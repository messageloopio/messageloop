package messageloop

import (
	"path"
	"sync"
)

// ACLRule defines a single access control rule for channel operations.
type ACLRule struct {
	// ChannelPattern is a glob pattern to match channels, e.g. "private.*", "chat.**".
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
// If no rule matches the channel, access is allowed by default.
func (e *ACLEngine) CanSubscribe(channel, userID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, entry := range e.entries {
		if matched, _ := path.Match(entry.pattern, channel); matched {
			if entry.denyAll {
				return false
			}
			if entry.allowSubscribe != nil {
				return entry.wildcardSub || entry.allowSubscribe[userID]
			}
		}
	}
	return true
}

// CanPublish returns true if userID is allowed to publish to the channel.
// If no rule matches the channel, access is allowed by default.
func (e *ACLEngine) CanPublish(channel, userID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, entry := range e.entries {
		if matched, _ := path.Match(entry.pattern, channel); matched {
			if entry.denyAll {
				return false
			}
			if entry.allowPublish != nil {
				return entry.wildcardPub || entry.allowPublish[userID]
			}
		}
	}
	return true
}
