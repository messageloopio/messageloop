package survey

import (
	"testing"
	"time"
)

// TestClampTimeout pins the survey timeout clamp shared by the client path
// and the Server API path (previously two verbatim copies that could drift).
func TestClampTimeout(t *testing.T) {
	cases := []struct {
		name        string
		policyCap   time.Duration
		requestedMs int64
		want        time.Duration
	}{
		{"no policy, no request", 0, 0, 5 * time.Second},
		{"policy below default", 2 * time.Second, 0, 2 * time.Second},
		{"policy above ceiling", time.Minute, 0, 10 * time.Second},
		{"request within cap", 5 * time.Second, 2000, 2 * time.Second},
		{"request above cap", 5 * time.Second, 60000, 5 * time.Second},
		{"request above ceiling", time.Minute, 60000, 10 * time.Second},
		{"request below floor", 5 * time.Second, 1, 100 * time.Millisecond},
		{"negative request keeps cap", 3 * time.Second, -5, 3 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampTimeout(tc.policyCap, tc.requestedMs); got != tc.want {
				t.Fatalf("ClampTimeout(%v, %d) = %v, want %v", tc.policyCap, tc.requestedMs, got, tc.want)
			}
		})
	}
}
