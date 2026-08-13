package messageloop

import (
	"testing"
)

func TestACLEngine_CanSubscribe(t *testing.T) {
	engine := NewACLEngine([]ACLRule{
		{ChannelPattern: "private.*", AllowSubscribe: []string{"user-1", "user-2"}},
		{ChannelPattern: "public.*", AllowSubscribe: []string{"*"}},
		{ChannelPattern: "blocked.*", DenyAll: true},
	})

	tests := []struct {
		channel string
		userID  string
		want    bool
	}{
		{"private.chat", "user-1", true},
		{"private.chat", "user-2", true},
		{"private.chat", "user-3", false},
		{"public.news", "anyone", true},
		{"blocked.secret", "user-1", false},
		{"unmatched.channel", "user-1", true}, // no rule = allow
	}

	for _, tt := range tests {
		t.Run(tt.channel+"_"+tt.userID, func(t *testing.T) {
			got := engine.CanSubscribe(tt.channel, tt.userID)
			if got != tt.want {
				t.Errorf("CanSubscribe(%q, %q) = %v, want %v", tt.channel, tt.userID, got, tt.want)
			}
		})
	}
}

func TestACLEngine_CanPublish(t *testing.T) {
	engine := NewACLEngine([]ACLRule{
		{ChannelPattern: "readonly.*", AllowPublish: []string{"admin"}},
		{ChannelPattern: "open.*", AllowPublish: []string{"*"}},
		{ChannelPattern: "blocked.*", DenyAll: true},
	})

	tests := []struct {
		channel string
		userID  string
		want    bool
	}{
		{"readonly.data", "admin", true},
		{"readonly.data", "user-1", false},
		{"open.chat", "anyone", true},
		{"blocked.secret", "admin", false},
		{"unmatched.channel", "user-1", true},
	}

	for _, tt := range tests {
		t.Run(tt.channel+"_"+tt.userID, func(t *testing.T) {
			got := engine.CanPublish(tt.channel, tt.userID)
			if got != tt.want {
				t.Errorf("CanPublish(%q, %q) = %v, want %v", tt.channel, tt.userID, got, tt.want)
			}
		})
	}
}

func TestACLEngine_NoRules(t *testing.T) {
	engine := NewACLEngine(nil)
	if !engine.CanSubscribe("any.channel", "any-user") {
		t.Error("expected allow when no rules configured")
	}
	if !engine.CanPublish("any.channel", "any-user") {
		t.Error("expected allow when no rules configured")
	}
}

// TestACLEngine_DenyAllPrecedence verifies that DenyAll wins regardless of
// rule order: a permissive rule placed before or after a DenyAll rule on the
// same channel must never bypass the deny.
func TestACLEngine_DenyAllPrecedence(t *testing.T) {
	ruleSets := map[string][]ACLRule{
		// permissive rule first, denyAll later
		"permissive_first": {
			{ChannelPattern: "finance.*", AllowSubscribe: []string{"*"}, AllowPublish: []string{"*"}},
			{ChannelPattern: "finance.*", DenyAll: true},
		},
		// denyAll first, permissive later
		"denyAll_first": {
			{ChannelPattern: "finance.*", DenyAll: true},
			{ChannelPattern: "finance.*", AllowSubscribe: []string{"*"}, AllowPublish: []string{"*"}},
		},
		// denyAll on a more specific overlapping pattern must still win
		"overlapping_denyAll": {
			{ChannelPattern: "app.*", AllowSubscribe: []string{"*"}, AllowPublish: []string{"*"}},
			{ChannelPattern: "app.admin.*", DenyAll: true},
		},
	}

	for name, rules := range ruleSets {
		t.Run(name, func(t *testing.T) {
			engine := NewACLEngine(rules)
			channel := "finance.trade"
			if name == "overlapping_denyAll" {
				channel = "app.admin.secret"
			}
			if engine.CanSubscribe(channel, "user-1") {
				t.Errorf("CanSubscribe(%q): expected deny", channel)
			}
			if engine.CanPublish(channel, "user-1") {
				t.Errorf("CanPublish(%q): expected deny", channel)
			}
		})
	}
}

// TestACLEngine_OverlappingAllowChannelNotDenied ensures an overly broad
// permissive rule does not invert the denyAll precedence on non-denied channels.
func TestACLEngine_OverlappingAllowChannelNotDenied(t *testing.T) {
	engine := NewACLEngine([]ACLRule{
		{ChannelPattern: "app.*", AllowSubscribe: []string{"*"}, AllowPublish: []string{"*"}},
		{ChannelPattern: "app.admin.*", DenyAll: true},
	})
	if !engine.CanSubscribe("app.chat", "user-1") {
		t.Error("expected allow for channel not matching denyAll rule")
	}
}

// TestACLEngine_LastMatchingAllowWins verifies the documented last-write-wins
// semantics: when multiple rules match and none denies all, the last matching
// rule's allow list decides.
func TestACLEngine_LastMatchingAllowWins(t *testing.T) {
	subEngine := NewACLEngine([]ACLRule{
		{ChannelPattern: "chat.*", AllowSubscribe: []string{"user-1"}},
		{ChannelPattern: "chat.*", AllowSubscribe: []string{"user-2"}},
	})
	pubEngine := NewACLEngine([]ACLRule{
		{ChannelPattern: "chat.*", AllowPublish: []string{"user-1"}},
		{ChannelPattern: "chat.*", AllowPublish: []string{"user-2"}},
	})

	subs := []struct {
		userID string
		want   bool
	}{
		{"user-1", false}, // overridden by the later rule
		{"user-2", true},
	}
	for _, tt := range subs {
		if got := subEngine.CanSubscribe("chat.room", tt.userID); got != tt.want {
			t.Errorf("CanSubscribe(%q) = %v, want %v", tt.userID, got, tt.want)
		}
	}

	pubs := []struct {
		userID string
		want   bool
	}{
		{"user-1", false},
		{"user-2", true},
	}
	for _, tt := range pubs {
		if got := pubEngine.CanPublish("chat.room", tt.userID); got != tt.want {
			t.Errorf("CanPublish(%q) = %v, want %v", tt.userID, got, tt.want)
		}
	}
}

// TestACLEngine_EntryWithoutAllowListDoesNotOverride verifies that a matching
// rule which does not specify an allow list for the queried operation does not
// reset the decision made by the last rule carrying such a list.
func TestACLEngine_EntryWithoutAllowListDoesNotOverride(t *testing.T) {
	engine := NewACLEngine([]ACLRule{
		{ChannelPattern: "chat.*", AllowSubscribe: []string{"user-1"}},
		{ChannelPattern: "chat.*", AllowPublish: []string{"*"}},
	})
	if !engine.CanSubscribe("chat.room", "user-1") {
		t.Error("expected user-1 allowed by last subscribe allow rule")
	}
	if engine.CanSubscribe("chat.room", "user-2") {
		t.Error("expected user-2 denied")
	}
	if !engine.CanPublish("chat.room", "anyone") {
		t.Error("expected publish allowed by last publish allow rule")
	}
}

// --- Fix task 10: segment-based wildcard semantics consistent with the
// subscription matcher ("*" = one segment, "**" = zero or more) ---

func TestMatchChannelPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		channel string
		want    bool
	}{
		{"exact", "chat.room", "chat.room", true},
		{"exact differs", "chat.room", "chat.room2", false},
		{"single star one segment", "chat.*", "chat.room", true},
		{"single star not multi-segment", "chat.*", "chat.room.sub", false},
		{"single star not empty", "chat.*", "chat", false},
		{"double star matches multi-segment", "chat.**", "chat.room.sub", true},
		{"double star matches one segment", "chat.**", "chat.room", true},
		{"double star matches prefix only", "chat.**", "chat", true},
		{"double star requires prefix", "chat.**", "other.room", false},
		{"double star matches zero segments anywhere", "a.**.b", "a.b", true},
		{"double star consumes middle", "a.**.b", "a.x.y.b", true},
		{"leading double star", "**.x", "a.b.x", true},
		{"global star", "*", "room", true},
		{"global star not multi-segment", "*", "a.b", false},
		{"global double star", "**", "a.b.c", true},
		{"presence sub-channel matched by chat.*", "chat.*", "chat.room/__presence", true},
		{"presence sub-channel matched by chat.**", "chat.**", "chat.room/__presence", true},
		{"extra pattern segment", "chat.room.extra", "chat.room", false},
		{"extra channel segment", "chat.room", "chat.room.extra", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchChannelPattern(tt.pattern, tt.channel); got != tt.want {
				t.Errorf("matchChannelPattern(%q, %q) = %v, want %v", tt.pattern, tt.channel, got, tt.want)
			}
		})
	}
}

// TestACLEngine_StarIsSingleSegment locks the matcher-consistent semantics at
// the engine level: a "chat.*" deny must not cover deeper channels, while a
// "log.**" deny covers the whole subtree including the bare prefix.
func TestACLEngine_StarIsSingleSegment(t *testing.T) {
	engine := NewACLEngine([]ACLRule{
		{ChannelPattern: "chat.*", DenyAll: true},
		{ChannelPattern: "log.**", DenyAll: true},
	})

	if engine.CanSubscribe("chat.room", "user-1") {
		t.Error("chat.* must match chat.room")
	}
	if !engine.CanSubscribe("chat.room.sub", "user-1") {
		t.Error("chat.* must not match chat.room.sub (single-segment wildcard)")
	}
	if engine.CanSubscribe("log.a.b.c", "user-1") {
		t.Error("log.** must match log.a.b.c")
	}
	if engine.CanSubscribe("log", "user-1") {
		t.Error("log.** must match log itself")
	}
}
