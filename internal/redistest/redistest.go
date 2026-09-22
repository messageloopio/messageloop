// Package redistest centralizes the absent-Redis policy for Redis-backed
// integration tests: a missing Redis skips the suite locally (with an
// actionable hint), but setting MESSAGELOOP_TEST_REDIS_REQUIRED turns the
// skip into a hard failure — CI sets it, so a dead Redis service turns the
// build red instead of silently degrading it to a fake green.
package redistest

import (
	"os"
	"testing"
)

// RequiredEnv is the environment variable that makes an unreachable Redis a
// test failure rather than a skip ("1" in CI; any non-empty value counts).
const RequiredEnv = "MESSAGELOOP_TEST_REDIS_REQUIRED"

// SkipOrFatal decides the absent-Redis outcome for one test: skip with an
// actionable hint locally, hard-fail when MESSAGELOOP_TEST_REDIS_REQUIRED is
// set. scope names the suite for the failure message.
func SkipOrFatal(t testing.TB, scope string, pingErr error) {
	t.Helper()
	if os.Getenv(RequiredEnv) != "" {
		t.Fatalf("%s: Redis unreachable and %s is set, failing instead of skipping: %v",
			scope, RequiredEnv, pingErr)
	}
	t.Skipf("%s: Redis unreachable, skipping (start one with `docker run --rm -p 6379:6379 redis:7-alpine`; set %s=1 to make absence a failure): %v",
		scope, RequiredEnv, pingErr)
}
