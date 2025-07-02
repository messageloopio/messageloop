package messageloop

import "fmt"

type Disconnect struct {
	// Code is a disconnect code.
	Code uint32 `json:"code,omitempty"`
	// Reason is a short description of disconnect code for humans.
	Reason string `json:"reason"`
}

// String representation.
func (d Disconnect) String() string {
	return fmt.Sprintf("code: %d, reason: %s", d.Code, d.Reason)
}

// Error to use Disconnect as a callback handler error to signal Centrifuge
// that client must be disconnected with corresponding Code and Reason.
func (d Disconnect) Error() string {
	return d.String()
}
