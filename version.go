package messageloop

import (
	"strconv"
	"strings"
)

// protocolGeneration is the only Connect.version major generation this server
// accepts. Envelope field numbers are rearranged between generations (e.g.
// outbound field 9 is rpc_reply in v1 and recover_complete in v2), so a client
// speaking another generation would be silently misinterpreted without a gate.
const protocolGeneration = 2

// protocolGenerationOK reports whether version parses to the supported
// generation: the decimal integer before the first '.' must equal
// protocolGeneration ("2", "2.0.0", "2.1.3" are all valid). The gate is
// fail-closed: empty strings and unparseable values are rejected, which also
// catches v1 clients whose Connect decodes to garbage under the v2 wire format.
func protocolGenerationOK(version string) bool {
	major, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n == protocolGeneration
}
