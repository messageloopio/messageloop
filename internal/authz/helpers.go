package authz

import "strings"

// Local copy of the root-package helper, duplicated in PR-KA-D12 (following
// the internal/stream/helpers.go precedent) so this package does not import
// the root package (which would create an import cycle through the root
// transition aliases). The root original (hub.go isWildcard) stays in place
// until the root package is retired in KD-K26 phase three (D13).

// isWildcard returns true if the channel pattern contains a wildcard character.
func isWildcard(ch string) bool {
	return strings.Contains(ch, "*")
}
