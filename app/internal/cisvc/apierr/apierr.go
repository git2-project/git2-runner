// Package apierr defines the errors that ci-svc's domain packages return
// to callers.
//
// An *Error carries a message that is safe to send to the client and a
// Kind that package server maps to a gRPC status code. Every other error is
// treated as an internal failure: it is logged, and the client only sees a
// generic INTERNAL error. Domain packages therefore never put SQL, paths
// or secrets into an *Error.
package apierr

import (
	"errors"

	"github.com/google/uuid"
)

// Kind is the class of an error, which decides its gRPC status code.
type Kind int

// Kinds of errors.
const (
	Invalid Kind = iota + 1
	NotFound
	AlreadyExists
	FailedPrecondition
	Unauthenticated
	ResourceExhausted
	Unavailable
)

// Error is an error whose message is safe to send to the client.
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

// New returns an *Error.
func New(kind Kind, message string) error {
	return &Error{Kind: kind, Message: message}
}

// InvalidField returns an Invalid error about one request field, for
// example "name: must not be empty".
func InvalidField(field, reason string) error {
	return &Error{Kind: Invalid, Message: field + ": " + reason}
}

// KindOf returns the Kind of err, or 0 if err is not an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// ParseID parses a UUID request field. A malformed ID is Invalid, never
// NotFound.
func ParseID(field, s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, InvalidField(field, "is required")
	}
	id, err := uuid.Parse(s)
	if err != nil || len(s) != 36 {
		return uuid.Nil, InvalidField(field, "must be a UUID")
	}
	return id, nil
}
