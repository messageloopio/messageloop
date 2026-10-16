package proxyserver

import (
	"errors"

	sharedv2 "github.com/messageloopio/messageloop/shared/genproto/messageloop/shared/v2"
)

// ErrNoRoute is returned by RPCMux when no registered (channel, method)
// pattern matches the request. The Server maps it to the in-band reply
// error with code "NO_ROUTE"; the MessageLoop server forwards in-band reply
// errors to the calling client verbatim.
var ErrNoRoute = errors.New("no rpc route matches channel/method")

// Error carries a proto Error through a handler's error return so the
// bridge can use it in-band verbatim instead of applying the default code
// mapping. Handlers that are fine with the default codes just return plain
// errors.
type Error struct {
	Proto *sharedv2.Error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil || e.Proto == nil {
		return "<nil>"
	}
	return e.Proto.Message
}

// NewError wraps a proto Error for returning from a handler.
func NewError(proto *sharedv2.Error) *Error {
	return &Error{Proto: proto}
}

// inBandError picks the error to place in a response's in-band Error field:
// a *Error carried by err wins, otherwise a default code/type mapping
// applies. ACL and auth denials fall in the vocabulary the server-side
// examples already use; RPC failures use the server_error family.
func inBandError(err error, defaultCode, defaultType string) *sharedv2.Error {
	var carried *Error
	if errors.As(err, &carried) && carried.Proto != nil {
		return carried.Proto
	}
	return &sharedv2.Error{
		Code:    defaultCode,
		Type:    defaultType,
		Message: err.Error(),
	}
}
