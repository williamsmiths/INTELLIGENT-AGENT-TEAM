// Package kerr holds the error kinds every service returns. The HTTP layer
// maps them to status codes (spec §3): Forbidden 403, NotFound 404,
// Conflict 409, Invalid 422, TooMany 429.
package kerr

import (
	"errors"
	"fmt"
)

// Kind is the class of a service error.
type Kind int

const (
	Forbidden Kind = iota + 1 // wrong role
	NotFound                  // missing, or outside the caller's data scope
	Conflict                  // wrong state or version
	Invalid                   // bad input
	TooMany                   // rate limit reached (spec S2 E.1-10)
)

// Error is a typed service error with a message that is safe to return to the caller.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// New builds a typed error.
func New(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// KindOf returns the kind of err, or 0 for an untyped (internal) error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// Is reports whether err has the given kind.
func Is(err error, kind Kind) bool { return KindOf(err) == kind }
