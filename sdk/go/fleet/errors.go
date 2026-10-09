package fleet

import "fleetplatform/sdk/go/protocol"

// Error codes the client adds to the server's (protocol.Err*).
const (
	// CodeNetwork: the socket could not be opened or was lost. Retried.
	CodeNetwork = "network"
	// CodeTimeout: no enroll.response / welcome within Config.HandshakeTimeout. Retried.
	CodeTimeout = "timeout"
	// CodeClosed: the client is closed, or not open right now (Send).
	CodeClosed = "closed"
	// CodeProtocol: the server answered a handshake with something unexpected.
	CodeProtocol = "protocol"
	// CodeTokenStore: the TokenStore failed to load or save credentials.
	CodeTokenStore = "token_store"
)

// Error is every error the client surfaces. Code is the server's error code
// when the server refused something (protocol.ErrAuthFailed, ...), otherwise one
// of the Code* constants above.
//
// Match on the code with errors.Is and the sentinels below:
//
//	if errors.Is(err, fleet.ErrAuthFailed) { ... }
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "fleet: " + e.Code
	}
	return "fleet: " + e.Code + ": " + e.Message
}

// Is reports a match on Code, so errors.Is(err, fleet.ErrNotFound) holds for any
// Error with that code whatever its message.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code && (t.Message == "" || t.Message == e.Message)
}

// Retryable reports whether the client reconnects after this failure. Only
// transient ones qualify: a network drop or server restart, a handshake
// timeout, and a heartbeat lapse (the server's rate_limited close). Every other
// refusal is terminal and closes the client: auth_failed (a bad or revoked
// token, a bad enrollment key), conflict (another connection took over this
// identity, so reconnecting would only kick it back), and the rest.
func (e *Error) Retryable() bool {
	switch e.Code {
	case CodeNetwork, CodeTimeout, protocol.ErrRateLimited:
		return true
	}
	return false
}

// Sentinels for errors.Is. They carry no message and match on Code alone.
var (
	ErrClosed         = &Error{Code: CodeClosed}
	ErrNetwork        = &Error{Code: CodeNetwork}
	ErrTimeout        = &Error{Code: CodeTimeout}
	ErrAuthFailed     = &Error{Code: protocol.ErrAuthFailed}
	ErrInvalidMessage = &Error{Code: protocol.ErrInvalidMessage}
	ErrNotFound       = &Error{Code: protocol.ErrNotFound}
	ErrNotAuthorized  = &Error{Code: protocol.ErrNotAuthorized}
	ErrConflict       = &Error{Code: protocol.ErrConflict}
	ErrRateLimited    = &Error{Code: protocol.ErrRateLimited}
)
