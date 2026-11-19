package nodes

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// buildClock is a clock the tests move by hand.
type buildClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *buildClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *buildClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeTopology has nodeA and nodeB as its nodes.
type fakeTopology struct{}

func (fakeTopology) Node(target string) (string, bool) {
	if target == "nodeA" || target == "nodeB" {
		return target, true
	}
	return "", false
}
func (fakeTopology) Nodes() []string { return []string{"nodeA", "nodeB"} }

// fakeTranscode reports a fixed state per node.
type fakeTranscode map[string]TranscodeState

func (f fakeTranscode) TranscodeStates(context.Context) map[string]TranscodeState { return f }

func buildSvc(t *testing.T) (*Service, *buildClock) {
	t.Helper()
	return buildSvcAt(t, filepath.Join(t.TempDir(), "nodes.db"))
}

func buildSvcAt(t *testing.T, path string) (*Service, *buildClock) {
	t.Helper()
	clock := &buildClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	s, err := Open(Options{Path: path, Topology: fakeTopology{}, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clock
}

func startBuild(t *testing.T, e *Service, target string, mode string, ttl int) Build {
	t.Helper()
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "ci-tool", Holder: "runner1", Kind: "gradle", Title: "integration run", PreemptMode: mode, TTLSeconds: ttl, Via: BuildViaNodeLease})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func lowBuild(t *testing.T, e *Service, target string, ttl int) Build {
	t.Helper()
	b, err := e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "cache-warmer", Title: "warm", Priority: BuildPriorityLow, TTLSeconds: ttl, Via: BuildViaNodeLease})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func finishBuild(t *testing.T, e *Service, id, status string) Build {
	t.Helper()
	b, _, err := e.FinishBuild(context.Background(), id, status, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tick is what the reconcile loop does on each pass.
func tick(e *Service) { e.Reconcile(context.Background()) }

func pauseBuilds(t *testing.T, e *Service, node string, paused bool) NodeView {
	t.Helper()
	if err := e.SetBuildsPause(context.Background(), node, paused); err != nil {
		t.Fatal(err)
	}
	v, err := e.Node(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func createBuild(e *Service, target, priority string) (Build, error) {
	return e.CreateBuild(context.Background(), BuildInput{Target: target, Source: "ci-tool", Title: "run", Priority: priority, Via: BuildViaNodeLease})
}
