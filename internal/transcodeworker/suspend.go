package transcodeworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Suspend.
//
// A node paused in "suspend" mode keeps the encodes it is running: their
// ffmpeg processes are frozen with SIGSTOP and continue with SIGCONT where
// they were when the node is resumed. The state is a durable record in the
// state directory, like the queue hold, and it also holds the queue: nothing
// queued starts while it is on.
//
// Each job runs in its own detached runner process, and ffmpeg is the runner's
// child, so the freezing is done by the runner, not by the HTTP server. The
// server's PUT /v1/suspend only writes the record; every runner polls it
// (suspendPollInterval), signals the children it has started and refuses to
// start new ones while it is on. A runner marks itself suspended in its
// heartbeat, which is what GET /v1/suspend counts.
//
// Only the runner's long-lived children are frozen: everything started
// through runCharged (the encode, x265 passes, the quality measurements, the
// source scans) and the full-file packet hashing. A short probe with its own
// deadline (compact_chain, the capability probes) does not go through
// runCharged on purpose: freezing it would turn the pause into a timeout, and
// it runs for seconds. The runner's own work that is not an ffmpeg child
// (copying and publishing a finished encode) is not frozen either: a finished
// encode is saved as in any pause.

const suspendFile = "worker-suspend.json"

// suspendPollInterval is how often a runner reads the durable record. A var so
// tests can shorten it.
var suspendPollInterval = 500 * time.Millisecond

type suspendRecord struct {
	Suspend bool `json:"suspend"`
	// Generation names this suspension: a resume removes the record and the
	// next suspend gets a new one. Runners report the generation they froze
	// under, so a stale heartbeat is never counted for a later suspend.
	Generation int64     `json:"generation,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// unreadableSuspendGeneration is the generation of a record that cannot be
// read: it suspends (see Suspended) under one fixed name.
const unreadableSuspendGeneration = 1

func (w *Worker) suspendPath() string {
	if w == nil || w.cfg == nil || w.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, suspendFile)
}

// suspendState reads the durable record: whether the node is suspended and
// under which generation. An unreadable record suspends too, like the hold: a
// frozen encode is recoverable, an encode that keeps using a CPU the operator
// asked for is the mistake the pause exists to prevent.
func (w *Worker) suspendState() (on bool, generation int64) {
	path := w.suspendPath()
	if path == "" {
		return false, 0
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0
	}
	if err != nil {
		return true, unreadableSuspendGeneration
	}
	var rec suspendRecord
	if json.Unmarshal(data, &rec) != nil {
		return true, unreadableSuspendGeneration
	}
	if !rec.Suspend {
		return false, 0
	}
	if rec.Generation == 0 {
		return true, unreadableSuspendGeneration
	}
	return true, rec.Generation
}

// Suspended reports whether the node is suspended.
func (w *Worker) Suspended() bool {
	on, _ := w.suspendState()
	return on
}

// queueHeld is whether the scheduler must start nothing: the hold, or a
// suspended node (which is also held).
func (w *Worker) queueHeld() bool { return w.Held() || w.Suspended() }

// SetSuspend turns the suspend on or off. Turning it on waits for a scheduler
// pass in progress, like SetHold, so a job started by that pass finds the
// record when its runner begins.
func (w *Worker) SetSuspend(suspend bool) error {
	path := w.suspendPath()
	if path == "" {
		return errors.New("worker has no state_dir to store the suspend")
	}
	w.suspendWrite.Lock()
	defer w.suspendWrite.Unlock()
	if !suspend {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	capLock, err := acquireCapacityLock(filepath.Clean(w.cfg.StateDir))
	if err != nil {
		return err
	}
	defer capLock.Unlock()
	// Repeating a suspend keeps its generation; only a readable record counts,
	// so an unreadable one is rewritten under a fresh name.
	if raw, rerr := os.ReadFile(path); rerr == nil {
		var cur suspendRecord
		if json.Unmarshal(raw, &cur) == nil && cur.Suspend && cur.Generation > unreadableSuspendGeneration {
			return nil
		}
	}
	data, err := json.MarshalIndent(suspendRecord{Suspend: true, Generation: time.Now().UnixNano(), UpdatedAt: time.Now().UTC()}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "worker-suspend-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// suspendCounts is how many jobs the worker is running and how many of those
// their runners report frozen.
func (w *Worker) suspendCounts() (active, frozen int, err error) {
	on, generation := w.suspendState()
	entries, err := os.ReadDir(w.cfg.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || len(name) == 0 || name[0] == '.' {
			continue
		}
		job, lerr := LoadJob(filepath.Join(w.cfg.StateDir, name, "job.json"))
		if lerr != nil || job == nil || job.ID != name || job.Status != "running" || job.PID <= 0 || !w.jobAlive(job) {
			continue
		}
		// A podcast job is never frozen, and a finished encode only publishes:
		// neither is an encode a suspend waits for.
		if (job.Plan != nil && job.Plan.Podcast != nil) || job.EncodeComplete {
			continue
		}
		active++
		// Frozen only under the suspend that is in force now: a heartbeat
		// from an earlier suspension says nothing about this one.
		if on && job.Suspended && job.SuspendGeneration == generation {
			frozen++
		}
	}
	return active, frozen, nil
}

// suspendControl is one runner's registry of the children it may freeze, and
// the gate that keeps it from starting new ones while frozen.
//
// Descendants. A child that leads its own process group (Setpgid) is signalled
// as a group, so whatever it spawned freezes with it. A child that does not is
// signalled alone: its descendants, if it had any, keep running. That is the
// accepted limit, because the runner's own group holds the runner, which must
// keep running, and no gated command spawns long-lived descendants (ffmpeg,
// ffprobe and x265 are single processes). A command that did would have to
// start in its own group to be frozen whole.
type suspendControl struct {
	mu       sync.Mutex
	frozen   bool  // the gate is closed: nothing new starts
	gen      int64 // generation of the suspend being applied; 0 when not frozen
	children map[int]*suspendChild
	resume   chan struct{}
	// onChange runs after the reported state changes, outside mu, one delivery
	// at a time (see deliver).
	// It receives the generation the children are frozen under, or 0 when
	// they are not (all) frozen.
	onChange  func(generation int64)
	notifyMu  sync.Mutex
	delivered int64 // last value handed to onChange; guarded by mu
}

type suspendChild struct {
	ownGroup bool // leads its own process group
	failed   bool // the last signal to it failed for a reason other than "gone"
}

type suspendControlKey struct{}

// killProcess is syscall.Kill; tests replace it to make a process impossible
// to signal.
var killProcess = syscall.Kill

func newSuspendControl() *suspendControl {
	return &suspendControl{children: map[int]*suspendChild{}}
}

func withSuspendControl(ctx context.Context, c *suspendControl) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, suspendControlKey{}, c)
}

func suspendControlFrom(ctx context.Context) *suspendControl {
	c, _ := ctx.Value(suspendControlKey{}).(*suspendControl)
	return c
}

// reportedLocked is what the heartbeat tells the server: the gate is closed
// and no child is known to have escaped the freeze. A child that cannot be
// signalled keeps the job from being reported frozen, so the server never
// treats the CPU as free while an encode may still run.
func (c *suspendControl) reportedLocked() bool {
	if !c.frozen {
		return false
	}
	for _, ch := range c.children {
		if ch.failed {
			return false
		}
	}
	return true
}

// mutate runs fn under the lock and then tells onChange what the reported
// state is.
func (c *suspendControl) mutate(fn func()) {
	c.mu.Lock()
	fn()
	c.mu.Unlock()
	c.deliver()
}

// deliver hands the reported state to onChange. Deliveries are serialised and
// each reads the state when its turn comes, not when its mutation ended, so
// the last value onChange sees is always the latest state: two transitions
// racing cannot leave a stale "frozen" behind a freeze that failed. It only
// calls onChange when the value differs from the last one delivered.
func (c *suspendControl) deliver() {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.mu.Lock()
	reported := int64(0)
	if c.reportedLocked() {
		reported = c.gen
	}
	notify := c.onChange
	changed := reported != c.delivered
	c.delivered = reported
	c.mu.Unlock()
	if changed && notify != nil {
		notify(reported)
	}
}

func (c *suspendControl) isFrozen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reportedLocked()
}

// signalChild signals a child: its whole group when it leads one, else the
// process alone. A process that is already gone (ESRCH) is not an error: the
// signal's purpose is moot. Anything else (EPERM, say) is returned.
func signalChild(pid int, ownGroup bool, sig syscall.Signal) error {
	if pid <= 1 {
		return nil
	}
	target := pid
	if ownGroup {
		target = -pid
	}
	if err := killProcess(target, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal %d to %d: %w", sig, target, err)
	}
	return nil
}

// signalLocked signals the registered children and records which could not be
// reached. With onlyFailed it retries just the ones whose last signal failed.
func (c *suspendControl) signalLocked(sig syscall.Signal, onlyFailed bool) {
	for pid, ch := range c.children {
		if onlyFailed && !ch.failed {
			continue
		}
		c.signalOneLocked(pid, ch, sig)
	}
}

func (c *suspendControl) signalOneLocked(pid int, ch *suspendChild, sig syscall.Signal) {
	err := signalChild(pid, ch.ownGroup, sig)
	if err != nil && !ch.failed {
		log.Printf("suspend: %v", err)
	}
	ch.failed = err != nil
}

// apply freezes (under generation 1) or continues every registered child; the
// worker's watcher uses applyGeneration with the record's own generation.
func (c *suspendControl) apply(frozen bool) {
	generation := int64(0)
	if frozen {
		generation = 1
	}
	c.applyGeneration(frozen, generation)
}

// applyGeneration freezes or continues every registered child. Repeating a
// state does nothing except retry the children that could not be signalled the
// last time. A new generation while already frozen is a new suspension that
// this runner never saw end (a resume and a suspend between two polls): the
// children are stopped again under it, so what is reported is true of it.
func (c *suspendControl) applyGeneration(frozen bool, generation int64) {
	c.mutate(func() {
		sig := syscall.SIGCONT
		if frozen {
			sig = syscall.SIGSTOP
		}
		if c.frozen == frozen {
			if frozen && c.gen != generation {
				c.gen = generation
				c.signalLocked(sig, false)
				return
			}
			c.signalLocked(sig, true)
			return
		}
		c.frozen = frozen
		c.gen = generation
		if frozen {
			c.resume = make(chan struct{})
			c.signalLocked(sig, false)
			return
		}
		c.signalLocked(sig, false)
		close(c.resume)
		c.resume = nil
	})
}

// admit blocks while frozen, so no child starts during a suspend.
func (c *suspendControl) admit(ctx context.Context) error {
	for {
		c.mu.Lock()
		if !c.frozen {
			c.mu.Unlock()
			return ctx.Err()
		}
		ch := c.resume
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// track registers a started child. A suspend that landed between admit and
// here freezes it at once. The returned func unregisters it; call it right
// after Wait returns.
func (c *suspendControl) track(cmd *exec.Cmd) func() {
	if cmd == nil || cmd.Process == nil {
		return func() {}
	}
	pid := cmd.Process.Pid
	ch := &suspendChild{ownGroup: cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid}
	c.mutate(func() {
		c.children[pid] = ch
		if c.frozen {
			c.signalOneLocked(pid, ch, syscall.SIGSTOP)
		}
	})
	return func() {
		c.mutate(func() { delete(c.children, pid) })
	}
}

// startGated starts cmd unless the runner is frozen (it waits for the resume
// first) and registers it. The returned func unregisters it. Without a control
// on ctx (the HTTP server's own children) it only starts the command.
func startGated(ctx context.Context, cmd *exec.Cmd) (func(), error) {
	c := suspendControlFrom(ctx)
	if c == nil {
		return func() {}, cmd.Start()
	}
	if err := c.admit(ctx); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return c.track(cmd), nil
}

// watchSuspend follows the durable record for the life of the runner: it
// freezes or continues the registered children as the record changes and
// mirrors the generation it is frozen under in w.suspendedGen for the
// heartbeat.
func (w *Worker) watchSuspend(ctx context.Context, c *suspendControl) {
	c.onChange = func(generation int64) { w.suspendedGen.Store(generation) }
	apply := func() {
		on, generation := w.suspendState()
		c.applyGeneration(on, generation)
	}
	apply()
	go func() {
		ticker := time.NewTicker(suspendPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				apply()
			}
		}
	}()
}
