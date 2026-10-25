package ingestor

import (
	"context"
	"encoding/json"
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

func TestTotalCPUSeconds(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		want  int64
		valid bool
	}{
		// slurmdb v0.0.40+ job record without steps: the job's own time.total
		{"seconds and microseconds", `{"time":{"total":{"seconds":3725,"microseconds":600000}}}`, 3726, true},
		{"rounds down under half a second", `{"time":{"total":{"seconds":10,"microseconds":499999}}}`, 10, true},
		{"no microseconds", `{"time":{"total":{"seconds":42}}}`, 42, true},
		{"zero is a value", `{"time":{"total":{"seconds":0,"microseconds":0}}}`, 0, true},
		{"no total", `{"time":{"start":1700000000}}`, 0, false},
		{"no time", `{}`, 0, false},
		// slurmdbd reports 0 for the job and the real time on its steps;
		// microseconds can exceed a second
		{"summed over steps", `{"time":{"total":{"seconds":0,"microseconds":0}},"steps":[
			{"time":{"total":{"seconds":0,"microseconds":1081881}}},
			{"time":{"total":{"seconds":0,"microseconds":0}}},
			{"time":{"total":{"seconds":23446,"microseconds":1029692}}}]}`, 23448, true},
		{"steps without time", `{"time":{"total":{"seconds":5}},"steps":[{}]}`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var job RawJob
			if err := json.Unmarshal([]byte(tt.json), &job); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := totalCPUSeconds(job)
			if got.Valid != tt.valid || got.Int64 != tt.want {
				t.Errorf("totalCPUSeconds = %+v, want %d (valid %v)", got, tt.want, tt.valid)
			}
		})
	}
}

func TestMaxRSS(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		want  int64
		valid bool
	}{
		{"highest step peak", `{"steps":[
			{"tres":{"requested":{"max":[{"type":"cpu","count":1070},{"type":"mem","count":19238912}]}}},
			{"tres":{"requested":{"max":[]}}},
			{"tres":{"requested":{"max":[{"type":"mem","count":2228412416},{"type":"vmem","count":1939476480}]}}}]}`, 2228412416, true},
		{"no mem reported", `{"steps":[{"tres":{"requested":{"max":[{"type":"cpu","count":5}]}}}]}`, 0, false},
		{"no steps", `{}`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var job RawJob
			if err := json.Unmarshal([]byte(tt.json), &job); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := maxRSS(job)
			if got.Valid != tt.valid || got.Int64 != tt.want {
				t.Errorf("maxRSS = %+v, want %d (valid %v)", got, tt.want, tt.valid)
			}
		})
	}
}

func TestNextChunkSeconds(t *testing.T) {
	tests := []struct {
		name          string
		cur, maxChunk int64
		jobs, target  int
		want          int64
	}{
		{"no target keeps max", 600, 3600, 50000, 0, 3600},
		{"busy window shrinks to target", 600, 3600, 16000, 4000, 150},
		{"never below a minute", 300, 3600, 95000, 1000, 60},
		{"quiet window grows at most fourfold", 300, 3600, 10, 4000, 1200},
		{"empty window grows fourfold", 600, 3600, 0, 4000, 2400},
		{"growth capped at max", 1800, 3600, 0, 4000, 3600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextChunkSeconds(tt.cur, tt.maxChunk, tt.jobs, tt.target); got != tt.want {
				t.Errorf("nextChunkSeconds(%d, %d, %d, %d) = %d, want %d", tt.cur, tt.maxChunk, tt.jobs, tt.target, got, tt.want)
			}
		})
	}
}
