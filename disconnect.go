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

// DisconnectConnectionClosed is a special Disconnect object used when
// client connection was closed without any advice from a server side.
// This can be a clean disconnect, or temporary disconnect of the client
// due to internet connection loss. Server can not distinguish the actual
// reason of disconnect.
var DisconnectConnectionClosed = Disconnect{
	Code:   3000,
	Reason: "connection closed",
}

// The codes below are built-in terminal codes.
var (
	// DisconnectInvalidToken issued when client came with invalid token.
	DisconnectInvalidToken = Disconnect{
		Code:   3500,
		Reason: "invalid token",
	}
	// DisconnectBadRequest issued when client uses malformed protocol frames.
	DisconnectBadRequest = Disconnect{
		Code:   3501,
		Reason: "bad request",
	}
	// DisconnectStale issued to close connection that did not become
	// authenticated in configured interval after dialing.
	DisconnectStale = Disconnect{
		Code:   3502,
		Reason: "stale",
	}
	// DisconnectForceNoReconnect issued when server disconnects connection
	// and asks it to not reconnect again.
	DisconnectForceNoReconnect = Disconnect{
		Code:   3503,
		Reason: "force disconnect",
	}
	// DisconnectConnectionLimit can be issued when client connection exceeds a
	// configured connection limit (per user ID or due to other rule).
	DisconnectConnectionLimit = Disconnect{
		Code:   3504,
		Reason: "connection limit",
	}
	// DisconnectChannelLimit can be issued when client connection exceeds a
	// configured channel limit.
	DisconnectChannelLimit = Disconnect{
		Code:   3505,
		Reason: "channel limit",
	}
	// DisconnectInappropriateProtocol can be issued when client connection format can not
	// handle incoming data. For example, this happens when JSON-based clients receive
	// binary data in a channel. This is usually an indicator of programmer error, JSON
	// clients can not handle binary.
	DisconnectInappropriateProtocol = Disconnect{
		Code:   3506,
		Reason: "inappropriate protocol",
	}
	// DisconnectPermissionDenied may be issued when client attempts accessing a server without
	// enough permissions.
	DisconnectPermissionDenied = Disconnect{
		Code:   3507,
		Reason: "permission denied",
	}
	// DisconnectNotAvailable may be issued when ErrorNotAvailable does not fit message type, for example
	// we issue DisconnectNotAvailable when client sends asynchronous message without MessageHandler set
	// on server side.
	DisconnectNotAvailable = Disconnect{
		Code:   3508,
		Reason: "not available",
	}
	// DisconnectTooManyErrors may be issued when client generates too many errors.
	DisconnectTooManyErrors = Disconnect{
		Code:   3509,
		Reason: "too many errors",
	}
	// DisconnectIdleTimeout may be issued when client connection is idle for too long.
	DisconnectIdleTimeout = Disconnect{
		Code:   3511,
		Reason: "idle timeout",
	}
)
