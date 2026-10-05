package quic

import (
	"errors"
	"fmt"
	"testing"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"

	"github.com/messageloopio/messageloop/internal/session"
)

// TestWrapPeerGone pins the write-error classification contract of ADR-0001:
// only a CONNECTION_CLOSE the peer sent (*quic.ApplicationError with
// Remote=true) is marked session.ErrPeerGone; local closes, stream resets
// and other shapes pass through so they keep their slow-consumer visibility.
func TestWrapPeerGone(t *testing.T) {
	remoteClose := &quic.ApplicationError{Remote: true, ErrorCode: 0, ErrorMessage: "bye"}
	localClose := &quic.ApplicationError{Remote: false, ErrorCode: 0x100, ErrorMessage: "local shutdown"}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"remote application close", remoteClose, true},
		{"wrapped remote close", fmt.Errorf("write frame: %w", remoteClose), true},
		{"local close", localClose, false},
		{"other error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := wrapPeerGone(tc.err)
			if tc.err == nil {
				require.NoError(t, wrapped)
				return
			}
			require.ErrorIs(t, wrapped, tc.err, "original shape must survive the join")
			require.Equal(t, tc.want, errors.Is(wrapped, session.ErrPeerGone))
		})
	}
}
