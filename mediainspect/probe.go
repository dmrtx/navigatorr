package mediainspect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProbeKind says why an ffprobe run produced no usable inspection.
type ProbeKind string

const (
	// ProbeTimeout is an ffprobe run that did not finish inside its deadline.
	// On network storage this is usually a stalled read, not a bad file.
	ProbeTimeout ProbeKind = "timeout"
	// ProbeUnavailable is an ffprobe binary that could not be started.
	ProbeUnavailable ProbeKind = "unavailable"
	// ProbeToolExit is an ffprobe that ran and exited with a failure status.
	ProbeToolExit ProbeKind = "tool_exit"
	// ProbeBadOutput is an ffprobe that exited cleanly with output that is not
	// the expected JSON document.
	ProbeBadOutput ProbeKind = "bad_output"
)

// probeStderrCap keeps the first bytes of ffprobe's stderr for the error.
const probeStderrCap = 2 << 10

// The probe limits are variables only so tests can shrink them.
var (
	// probeWaitDelay bounds how long Wait lingers after the deadline killed
	// ffprobe: a child holding the output pipe open, or a process that is slow
	// to die, must not keep a probe blocked long after its deadline.
	probeWaitDelay = 5 * time.Second
	// probeAbandonGrace is how long past the wait delay a probe may stay
	// blocked before the call stops waiting for a process the kernel will not
	// reap (a read stuck in uninterruptible storage I/O ignores SIGKILL).
	probeAbandonGrace = time.Second
	// detailedProbeTimeout and frameProbeTimeout are the per-run deadlines.
	detailedProbeTimeout = 60 * time.Second
	frameProbeTimeout    = 90 * time.Second
)

// ProbeError is the typed reason an ffprobe inspection failed. It replaces the
// silent "not probed" result, so callers can say why and decide whether one
// more attempt makes sense. Every ProbeError means the file was NOT inspected;
// callers must keep failing closed.
type ProbeError struct {
	Kind ProbeKind
	// Err is the underlying cause (deadline, exit status, JSON error).
	Err error
	// Stderr is the first bytes of ffprobe's stderr, capped at about 2 KB.
	Stderr string
	// Elapsed is how long the probe ran before it failed.
	Elapsed time.Duration
}

func (e *ProbeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ffprobe %s after %s", e.Kind, e.Elapsed.Round(time.Millisecond))
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if s := strings.TrimSpace(e.Stderr); s != "" {
		fmt.Fprintf(&b, ": %s", s)
	}
	return b.String()
}

func (e *ProbeError) Unwrap() error { return e.Err }

// Transient reports whether one more attempt could plausibly succeed: a probe
// that timed out, or whose stderr shows a storage or transport hiccup. A
// missing binary, a clean exit with malformed output or an ordinary demuxer
// rejection of the file is deterministic and is never transient.
func (e *ProbeError) Transient() bool {
	switch e.Kind {
	case ProbeTimeout:
		return true
	case ProbeToolExit:
		s := strings.ToLower(e.Stderr)
		for _, marker := range []string{"input/output error", "i/o error", "stale file handle", "resource temporarily unavailable", "connection reset", "connection timed out", "timed out", "transport endpoint", "no route to host", "host is down"} {
			if strings.Contains(s, marker) {
				return true
			}
		}
	}
	return false
}

// AsProbeError returns the ProbeError in err's chain, if any.
func AsProbeError(err error) (*ProbeError, bool) {
	var pe *ProbeError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// cappedBuffer keeps at most limit bytes and drops the rest. It is safe for
// the concurrent write of a still-running process and a read after the call
// gave up waiting for it.
type cappedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(string(c.buf))
}

// syncBuffer is a bytes.Buffer guarded for the same reason as cappedBuffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// runFFprobe runs one ffprobe with a fixed argv and returns its stdout. It
// never blocks past timeout + probeWaitDelay + probeAbandonGrace: the process
// is killed at the deadline, its pipes are closed after the wait delay, and a
// process the kernel will not reap (a stalled network read) is abandoned.
//
// A failure is a *ProbeError. A caller cancellation is not a probe failure and
// comes back as the context error.
func runFFprobe(ctx context.Context, ffprobePath string, timeout time.Duration, args ...string) (probeOutput, error) {
	start := time.Now()
	if ffprobePath == "" {
		return probeOutput{}, &ProbeError{Kind: ProbeUnavailable, Err: errors.New("ffprobe not found"), Elapsed: time.Since(start)}
	}
	if err := ctx.Err(); err != nil {
		return probeOutput{}, fmt.Errorf("ffprobe not started: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(pctx, ffprobePath, args...)
	cmd.WaitDelay = probeWaitDelay
	stdout := &syncBuffer{}
	stderr := &cappedBuffer{limit: probeStderrCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	var runErr error
	abandoned := false
	select {
	case runErr = <-done:
	case <-pctx.Done():
		select {
		case runErr = <-done:
		case <-time.After(probeWaitDelay + probeAbandonGrace):
			abandoned = true
		}
	}
	elapsed := time.Since(start)

	if parent := ctx.Err(); parent != nil && !errors.Is(parent, context.DeadlineExceeded) {
		// The caller gave up; that says nothing about the file.
		return probeOutput{}, fmt.Errorf("ffprobe interrupted: %w", parent)
	}
	if abandoned || errors.Is(pctx.Err(), context.DeadlineExceeded) {
		cause := fmt.Errorf("no result within %s", timeout)
		if abandoned {
			cause = fmt.Errorf("no result within %s and the process did not exit after it was killed", timeout)
		}
		return probeOutput{}, &ProbeError{Kind: ProbeTimeout, Err: cause, Stderr: stderr.String(), Elapsed: elapsed}
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.Is(runErr, exec.ErrWaitDelay):
			// Exited cleanly but a child kept the pipe open; whatever was
			// written before is parsed (and fails as bad output if cut short).
		case errors.As(runErr, &exitErr):
			return probeOutput{}, &ProbeError{Kind: ProbeToolExit, Err: runErr, Stderr: stderr.String(), Elapsed: elapsed}
		default:
			return probeOutput{}, &ProbeError{Kind: ProbeUnavailable, Err: runErr, Stderr: stderr.String(), Elapsed: elapsed}
		}
	}
	return probeOutput{stdout: stdout.bytes(), stderr: stderr.String(), elapsed: elapsed}, nil
}

// probeOutput is a finished ffprobe run.
type probeOutput struct {
	stdout  []byte
	stderr  string
	elapsed time.Duration
}

// badOutput is the failure of a run whose output did not decode.
func (o probeOutput) badOutput(err error) *ProbeError {
	return &ProbeError{Kind: ProbeBadOutput, Err: err, Stderr: o.stderr, Elapsed: o.elapsed}
}
