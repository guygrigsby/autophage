package resolution

import (
	"errors"
	"fmt"
)

// ErrRefused marks a transition the aggregate does not allow in its current
// state. Callers map it to a conflict.
var ErrRefused = errors.New("refused")

// ErrInvalid marks a value a constructor would not build.
var ErrInvalid = errors.New("invalid")

// Refused wraps ErrRefused with detail.
func Refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Invalid wraps ErrInvalid with detail.
func Invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
