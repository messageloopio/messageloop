package topics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitSegments(t *testing.T) {
	assert := assert.New(t)
	assert.Equal([]string{"acme", "chat", "room1"}, SplitSegments("acme:chat.room1"))
	assert.Equal([]string{"forex", "eur"}, SplitSegments("forex.eur"))
	assert.Equal([]string{"single"}, SplitSegments("single"))
	// Consecutive delimiters yield empty segments (invalid-topic detection
	// depends on this).
	assert.Equal([]string{"a", "", "b"}, SplitSegments("a..b"))
	assert.Equal([]string{"a", "", "b"}, SplitSegments("a.:b"))
	assert.Equal([]string{""}, SplitSegments(""))
}

func TestValidateNamespace(t *testing.T) {
	assert := assert.New(t)
	for _, ns := range []string{"acme", "a", "a1", "1a", "my-app", "a-1-b"} {
		assert.NoError(ValidateNamespace(ns), ns)
	}
	for _, ns := range []string{
		"",
		"ACME",          // uppercase
		"app_name",      // underscore
		"-acme",         // leading dash
		"acme-",         // trailing dash
		"ns:with:colon", // delimiter is not part of the identifier
		"ns.ch",         // dot is not part of the identifier
		"ns*me",         // wildcard is not part of the identifier
		repeat('a', 33), // above the length cap
	} {
		assert.Error(ValidateNamespace(ns), ns)
	}
	// 32 chars is the cap.
	assert.NoError(ValidateNamespace(repeat('a', 32)))
	assert.Error(ValidateNamespace(repeat('a', 33)))
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func TestValidateChannel(t *testing.T) {
	assert := assert.New(t)
	for _, ch := range []string{
		"acme:chat",
		"acme:chat.room1",
		"acme:chat.room1/__presence", // "/" inside a segment stays legal
		"acme:*",
		"acme:im.*",
		"acme:im.**",
		"a-b:topic",
	} {
		assert.NoError(ValidateChannel(ch), ch)
	}
	for _, ch := range []string{
		"",                // empty
		"chat",            // no namespace
		"acme:",           // empty topic part
		":chat",           // empty namespace
		"a:b:c",           // more than one delimiter
		"ACME:chat",       // bad namespace charset
		"-acme:chat",      // leading dash
		"a-:chat",         // trailing dash
		"acme:chat..room", // empty segment
		"acme:chat.**.x",  // "**" outside the final segment
	} {
		assert.Error(ValidateChannel(ch), ch)
	}
}

func TestNamespaceOf(t *testing.T) {
	assert := assert.New(t)
	ns, err := NamespaceOf("acme:chat.room1")
	assert.NoError(err)
	assert.Equal("acme", ns)

	_, err = NamespaceOf("chat.room1")
	assert.Error(err)
	_, err = NamespaceOf("a:b:c")
	assert.Error(err)
}

// TestMatch_Namespaced verifies the ":" delimiter participates in segment
// matching: namespace-scoped wildcards work and never leak across namespaces.
func TestMatch_Namespaced(t *testing.T) {
	assert := assert.New(t)
	assert.True(Match("acme:chat.room1", "acme:chat.room1"))
	assert.True(Match("acme:*", "acme:chat"))
	assert.False(Match("acme:*", "acme:chat.room1"), "* matches exactly one segment")
	assert.True(Match("acme:im.*", "acme:im.room1"))
	assert.False(Match("acme:im.*", "acme:other.room1"))
	assert.True(Match("acme:im.**", "acme:im"))
	assert.True(Match("acme:im.**", "acme:im.a.b"))
	assert.False(Match("acme:im.**", "acme:other"), "the topic prefix must match")
	assert.False(Match("acme:*", "other:chat"), "namespaces never leak")
	assert.False(Match("acme:*", "acme2:chat"), "segments compare exactly")
	// Un-namespaced strings keep their original semantics.
	assert.True(Match("chat.*", "chat.general"))
	assert.False(Match("chat.*", "chat.general.extra"))
}

// TestMatcher_NamespacedMatchers pins the namespaced behavior across the
// matcher implementations that split segments themselves.
func TestMatcher_NamespacedMatchers(t *testing.T) {
	matchers := map[string]Matcher{
		"cstrie":  NewCSTrieMatcher(),
		"trie":    NewTrieMatcher(),
		"naive":   NewNaiveMatcher(),
		"obitmap": NewOptimizedInvertedBitmapMatcher(16),
	}
	for name, m := range matchers {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			sub := testSubscriber("s1")
			_, err := m.Subscribe("acme:im.**", sub)
			assert.NoError(err)

			assert.Len(m.Lookup("acme:im"), 1, "trailing ** matches the zero-segment case")
			assert.Len(m.Lookup("acme:im.room1"), 1)
			assert.Empty(m.Lookup("acme:other.room1"), "no cross-namespace leak")
			assert.Empty(m.Lookup("other:im.room1"), "no cross-namespace leak")

			_, err = m.Subscribe("acme:*", testSubscriber("s2"))
			assert.NoError(err)
			assert.Len(m.Lookup("acme:general"), 1, "only the bare-namespace wildcard covers the topic prefix")
			assert.Empty(m.Lookup("acme:general.extra"), "* matches exactly one segment")
		})
	}
}

func testSubscriber(name string) Subscriber { return name }
