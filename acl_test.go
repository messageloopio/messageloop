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
