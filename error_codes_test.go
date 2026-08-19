package messageloop_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// productionErrorCodes enumerates every code the server writes into an Error
// envelope (see docs/v2/tasks/pr-ka-d7-error-codes.md §3.1/§3.2 for the
// emission-site census). When you add, rename or remove an emission-site
// code, update this list, the Error.code comment in
// protocol/shared/v2/errors.proto and docs/protocol.md#Error-Codes in the
// same change. Cluster command-bus codes and SDK-side codes are internal
// protocols and stay out of this table.
var productionErrorCodes = []string{
	// authentication/version (client.go handleConnect)
	"AUTH_REQUIRED",
	"AUTH_ERROR",
	"VERSION_UNSUPPORTED",
	// request/permission (client.go)
	"BAD_REQUEST",
	"PERMISSION_DENIED",
	"POLICY_DENIED",
	"PATTERN_NOT_ROUTABLE",
	"RATE_LIMITED",
	// proxy/RPC (client.go)
	"NO_PROXY",
	"PROXY_ERROR",
	"RPC_TIMEOUT",
	// recovery (recover.go)
	"RECOVER_FAILED",
	"RECOVER_SKIPPED",
	// survey top-level (client.go)
	"SURVEY_DISABLED",
	"SURVEY_TOO_MANY_SUBSCRIBERS",
	// survey per-answer (node.go, pkg/grpcstream/api_handler.go)
	"SURVEY_FAILED",
	"SURVEY_ANSWER_TOO_LARGE",
	// server/transport (client.go, pkg/grpcstream + pkg/quicstream transports)
	"INTERNAL_ERROR",
	"DISCONNECT_ERROR",
}

var errorCodeToken = regexp.MustCompile(`[A-Z][A-Z0-9_]{3,}`)

// TestWellKnownErrorCodeTable guards the single error-code table: every
// production Error.code must be listed in the well-known table carried by the
// Error.code comment in protocol/shared/v2/errors.proto, and the table must
// not list codes nothing emits.
func TestWellKnownErrorCodeTable(t *testing.T) {
	raw, err := os.ReadFile("protocol/shared/v2/errors.proto")
	require.NoError(t, err)
	content := string(raw)

	start := strings.Index(content, "message Error {")
	end := strings.Index(content, "string code = 1;")
	require.NotEqual(t, -1, start, "errors.proto must define message Error")
	require.NotEqual(t, -1, end, "errors.proto Error must keep `string code = 1;`")
	require.Less(t, start, end)
	comment := content[start:end]

	table := map[string]bool{}
	for _, token := range errorCodeToken.FindAllString(comment, -1) {
		table[token] = true
	}

	for _, code := range productionErrorCodes {
		assert.True(t, table[code], "emitted code %s is missing from the errors.proto well-known table", code)
	}
	assert.Len(t, table, len(productionErrorCodes),
		"the errors.proto table must list exactly the emitted codes (update productionErrorCodes and the proto comment together)")
}
