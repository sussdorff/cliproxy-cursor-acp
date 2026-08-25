package cursor

import "fmt"

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
