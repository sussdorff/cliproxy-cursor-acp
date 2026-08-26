package cursor

import (
	"errors"
	"fmt"
)

// FailureKind lets the host distinguish a temporary account failure from an
// invalid request or configuration that should not trigger account failover.
type FailureKind string

const (
	FailureRetryable FailureKind = "retryable"
	FailureFatal     FailureKind = "fatal"
)

// Failure is intentionally free of child output and environment values.
type Failure struct {
	Kind FailureKind
	Code string
	Err  error
}

func (f *Failure) Error() string {
	if f.Err == nil {
		return f.Code
	}
	return fmt.Sprintf("%s: %v", f.Code, f.Err)
}

func (f *Failure) Unwrap() error { return f.Err }

func retryable(code string, err error) error {
	return &Failure{Kind: FailureRetryable, Code: code, Err: err}
}

func fatal(code string, err error) error {
	return &Failure{Kind: FailureFatal, Code: code, Err: err}
}

// ValidationFailure denotes a client/configuration error that must be rendered
// by the native ABI as a stable HTTP 400 rather than an upstream failure.
func ValidationFailure(code, message string) error { return fatal(code, errors.New(message)) }

// FailureCode returns the stable failure code of err, or the empty string when
// err is not a plugin failure.
func FailureCode(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}
