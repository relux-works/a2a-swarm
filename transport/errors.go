package transport

import "errors"

type ErrorCode string

const (
	UnsupportedByTransport ErrorCode = "unsupported_by_transport"
	ContentTooLarge        ErrorCode = "content_too_large"
	InvalidReference       ErrorCode = "invalid_reference"
	InvalidCursor          ErrorCode = "invalid_cursor"
	Unauthenticated        ErrorCode = "unauthenticated"
	Unavailable            ErrorCode = "unavailable"
	Refused                ErrorCode = "refused"
)

// Error holds no received data. Even an unknown constructor code is normalized.
type Error struct{ code ErrorCode }

func NewError(code ErrorCode) *Error {
	switch code {
	case UnsupportedByTransport, ContentTooLarge, InvalidReference, InvalidCursor, Unauthenticated, Unavailable, Refused:
	default:
		code = Unavailable
	}
	return &Error{code: code}
}
func (e *Error) Error() string { return "transport: " + string(e.code) }
func Code(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}
