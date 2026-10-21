package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"sync"
)

// cpuMeter sums the CPU time (user + system) of the ffmpeg/ffprobe children
// one phase ran. It is carried on that phase's context, never on Worker, so
// concurrent jobs cannot charge each other's phases.
//
// The figure comes from each child's own rusage, so it covers every encoder
// thread but not grandchildren; ffmpeg and ffprobe do not fork. A phase that
// ran no measured child reports nil (unknown), never 0.
type cpuMeter struct {
	mu       sync.Mutex
	ms       int64
	measured bool
}

type cpuMeterKey struct{}

// withCPUMeter returns ctx carrying m; children run under it charge m.
func withCPUMeter(ctx context.Context, m *cpuMeter) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, cpuMeterKey{}, m)
}

// chargeCPU charges a finished child to the meter on ctx, if any. Call it
// after Run/Output returned, whatever the error: a failed or cancelled child
// still spent its CPU time.
func chargeCPU(ctx context.Context, cmd *exec.Cmd) {
	if m, ok := ctx.Value(cpuMeterKey{}).(*cpuMeter); ok && cmd != nil {
		m.add(cmd.ProcessState)
	}
}

// runCharged runs cmd and charges its CPU time to the meter on ctx.
func runCharged(ctx context.Context, cmd *exec.Cmd) error {
	err := cmd.Run()
	chargeCPU(ctx, cmd)
	return err
}

func (m *cpuMeter) add(ps *os.ProcessState) {
	if m == nil || ps == nil {
		return
	}
	d := ps.UserTime() + ps.SystemTime()
	m.mu.Lock()
	m.ms += d.Milliseconds()
	m.measured = true
	m.mu.Unlock()
}

// activeComputeMs is the phase's measured CPU time, or nil when no child was
// measured (an injected runner in tests, or a phase that failed to start).
func (m *cpuMeter) activeComputeMs() *int64 {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.measured {
		return nil
	}
	return knownCount(m.ms)
}
