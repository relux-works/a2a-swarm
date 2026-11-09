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

// Error returns a fixed diagnostic; nil and zero values report unavailable.
func (e *Error) Error() string { return "transport: " + string(e.Code()) }

// Code returns a closed code, including for a nil receiver or the zero value.
func (e *Error) Code() ErrorCode {
	if e == nil || e.code == "" {
		return Unavailable
	}
	return e.code
}
func Code(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code()
	}
	return ""
}

// Refusal carries only a validated definite-refusal code, never received text.
// Its zero value means no refusal and is invalid on a REFUSED result.
type Refusal struct{ code ErrorCode }

// NewRefusal normalizes unknown or transient codes to the fixed refused code.
func NewRefusal(code ErrorCode) Refusal {
	switch code {
	case UnsupportedByTransport, ContentTooLarge, InvalidReference, Unauthenticated, Refused:
	default:
		code = Refused
	}
	return Refusal{code: code}
}

// Code returns the closed refusal code, or empty for an absent refusal.
func (r Refusal) Code() ErrorCode { return r.code }

// Valid reports whether this value represents a definite refusal.
func (r Refusal) Valid() bool {
	switch r.code {
	case UnsupportedByTransport, ContentTooLarge, InvalidReference, Unauthenticated, Refused:
		return true
	}
	return false
}
