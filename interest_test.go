package messageloop

import (
	"errors"
	"testing"

	"github.com/messageloopio/messageloop/pkg/topics"
	"github.com/stretchr/testify/require"
)

// TestCompileInterest_Table pins the §4 table of PR-KA-A3: every row must
// compile exactly as specified.
func TestCompileInterest_Table(t *testing.T) {
	require := require.New(t)

	tests := []struct {
		key       string
		want      CompiledInterest
		wantError error
	}{
		{key: "chat.room.1", want: CompiledInterest{Exact: "chat.room.1"}},
		{key: "im.room.*", want: CompiledInterest{Pattern: "im.room.*"}},
		{key: "im.**", want: CompiledInterest{Pattern: "im.*", AlsoExact: "im"}},
		{key: "*", wantError: ErrPatternNotRoutable},
		{key: "**", wantError: ErrPatternNotRoutable},
		{key: "*.room", wantError: ErrPatternNotRoutable},
		{key: "im.*.tick", wantError: ErrPatternNotRoutable},
		{key: "a.", wantError: topics.ErrBadTopic},
		{key: "a..b", wantError: topics.ErrBadTopic},
	}

	for _, tt := range tests {
		got, err := CompileInterest(tt.key)
		if tt.wantError != nil {
			require.ErrorIsf(err, tt.wantError, "key %q", tt.key)
			require.Equalf(CompiledInterest{}, got, "key %q", tt.key)
			continue
		}
		require.NoErrorf(err, "key %q", tt.key)
		require.Equalf(tt.want, got, "key %q", tt.key)
	}
}

// TestCompileInterest_ErrorsAreDistinguishable pins that ErrBadTopic and
// ErrPatternNotRoutable are distinct and errors.Is detects both.
func TestCompileInterest_ErrorsAreDistinguishable(t *testing.T) {
	_, err := CompileInterest("*.room")
	require.ErrorIs(t, err, ErrPatternNotRoutable)
	require.False(t, errors.Is(err, topics.ErrBadTopic))

	_, err = CompileInterest("a..b")
	require.ErrorIs(t, err, topics.ErrBadTopic)
	require.False(t, errors.Is(err, ErrPatternNotRoutable))
}

// TestCompileInterest_AlsoExactAndPattern verify the trailing-** expansion
// covers zero and multiple segments while a trailing-* pattern stays single
// segment.
func TestCompileInterest_AlsoExactAndPattern(t *testing.T) {
	ci, err := CompileInterest("im.room.**")
	require.NoError(t, err)
	require.Equal(t, "im.room.*", ci.Pattern)
	require.Equal(t, "im.room", ci.AlsoExact)

	ci, err = CompileInterest("im.room.*")
	require.NoError(t, err)
	require.Equal(t, "im.room.*", ci.Pattern)
	require.Empty(t, ci.AlsoExact)
}

// TestMatchAfterCompile verifies segment semantics: exact keys match
// themselves only, "*" matches exactly one segment, "**" matches zero or
// more, and the Redis glob over-match ("im.room.*" covering "im.room.a.b")
// is rejected.
func TestMatchAfterCompile(t *testing.T) {
	require.True(t, MatchAfterCompile("chat.room.1", "chat.room.1"))
	require.False(t, MatchAfterCompile("chat.room.1", "chat.room.2"))

	require.True(t, MatchAfterCompile("im.room.*", "im.room.a"))
	require.False(t, MatchAfterCompile("im.room.*", "im.room.a.b"))
	require.False(t, MatchAfterCompile("im.room.*", "im.other.a"))

	require.True(t, MatchAfterCompile("im.**", "im"))
	require.True(t, MatchAfterCompile("im.**", "im.x"))
	require.True(t, MatchAfterCompile("im.**", "im.a.b.c"))
	require.False(t, MatchAfterCompile("im.**", "stocks"))
	require.False(t, MatchAfterCompile("im.**", "imx"))
}
