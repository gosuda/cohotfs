// Package apperr defines errors with stable CLI exit codes.
package apperr

import (
	"errors"
	"fmt"
)

const (
	ExitSuccess        = 0
	ExitUsage          = 2
	ExitUnavailable    = 3
	ExitPolicy         = 4
	ExitStateConflict  = 5
	ExitRuntime        = 6
	ExitPartialCleanup = 7
)

// Error carries a stable machine category without exposing sensitive details.
type Error struct {
	Code     int
	Category string
	Message  string
	Cause    error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Category
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code int, category, format string, args ...any) error {
	return &Error{Code: code, Category: category, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code int, category string, err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Category: category, Message: fmt.Sprintf(format, args...), Cause: err}
}

func Code(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ExitRuntime
}

func Category(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Category
	}
	return "internal"
}
