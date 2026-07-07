package ingestor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestIsFinalState(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"COMPLETED", true},
		{"FAILED", true},
		{"CANCELLED", true},
		{"CANCELLED+", true}, // flag suffix trimmed
		{"TIMEOUT", true},
		{"NODE_FAIL", true},
		{"PREEMPTED", true},
		{"BOOT_FAIL", true},
		{"DEADLINE", true},
		{"OUT_OF_MEMORY", true},
		{"RUNNING", false},
		{"PENDING", false},
		{"SUSPENDED", false},
		{"", false},
		{"completed", false}, // case-sensitive
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			if got := isFinalState(tt.state); got != tt.want {
				t.Errorf("isFinalState(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

// timeoutErr implements net.Error with a configurable Timeout result so we can
// exercise the typed net.Error branch of isRetryableErr.
type timeoutErr struct{ timeout bool }

func (e timeoutErr) Error() string   { return "mock net error" }
func (e timeoutErr) Timeout() bool   { return e.timeout }
func (e timeoutErr) Temporary() bool { return false }

func TestIsRetryableErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled is not retryable", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"wrapped eof", fmt.Errorf("http request error: %w", io.EOF), true},
		{"net timeout", timeoutErr{timeout: true}, true},
		{"net non-timeout falls through to substring", timeoutErr{timeout: false}, false},
		{"connection reset substring", errors.New("read tcp: connection reset by peer"), true},
		{"connection refused substring", errors.New("dial tcp: connection refused"), true},
		{"timeout substring", errors.New("Timeout was reached"), true},
		{"unrelated error", errors.New("json decode error"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableErr(tt.err); got != tt.want {
				t.Errorf("isRetryableErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsRetryableErrWrappedCanceled guards the ordering guarantee that a
// canceled context wins even when wrapped, so shutdown is never mistaken for a
// transient failure and retried.
func TestIsRetryableErrWrappedCanceled(t *testing.T) {
	err := fmt.Errorf("fetch aborted: %w", context.Canceled)
	if isRetryableErr(err) {
		t.Error("wrapped context.Canceled must not be retryable")
	}
}
