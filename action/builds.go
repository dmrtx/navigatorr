package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// Builds.
//
// A build is one job another tool runs on a machine: the target. The target is
// free text. When it names a configured transcode node, the build occupies the
// node the way an encode does: while any build on the node is running the node
// takes no new transcode work, and the encodes already on it are handled by the
// build's preempt mode with the machinery of a node pause (graceful, immediate,
// suspend). When the last running build on the node ends, or its heartbeat
// lapses and the reconcile loop marks it lost, the node resumes by itself.
// A low priority build (cache warming) never preempts (its preempt mode is
// always graceful, whatever was asked): it is created waiting
// and holds nothing until its node is idle, when it becomes running (see
// promoteWaitingBuilds). A target that is not a node is only tracked: lifecycle, heartbeat, expiry and
// history, and nothing is held.
//
// Effective node state. A build never writes, changes or clears the operator's
// worker_pauses row. The engine's pause view (workerPauses) is the merge of two
// sources: the operator's pause if the node has one, otherwise the strongest
// preempt mode among the node's running builds (immediate > suspend >
// graceful), otherwise free. Everything that decides dispatch, requeue, the
// worker hold and the freeze reads that view, so the freeze and the thaw follow
// the effective state: an operator resume while a build runs leaves the build's
// hold in place, and the end of the builds never resumes an operator pause.

const (
	// BuildDefaultTTL is how long a build lives without a heartbeat when the
	// request names no ttl_seconds.
	BuildDefaultTTL = 15 * time.Minute
	// BuildMaxTTL caps ttl_seconds on create and on heartbeat.
	BuildMaxTTL = 2 * time.Hour
	// BuildDefaultLimit is how many builds a list returns without a limit.
	BuildDefaultLimit = 100

	buildFieldMax = 64
	buildTitleMax = 200
	buildProgMax  = 200
)

var errStoreForBuilds = errors.New("store is required for builds")

func errorsIsBuildNotFound(err error) bool { return errors.Is(err, store.ErrBuildNotFound) }

var buildTargetPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// BuildInvalidError is a build request the caller can fix.
type BuildInvalidError struct{ Reason string }

func (e *BuildInvalidError) Error() string { return e.Reason }

func invalidBuild(format string, args ...any) error {
	return &BuildInvalidError{Reason: fmt.Sprintf(format, args...)}
}

// BuildInput is a request to start a build.
type BuildInput struct {
	Target      string
	Source      string
	Holder      string
	Kind        string
	Title       string
	PreemptMode string
	// Priority is "normal" (default: starts running at once and preempts) or
	// "low" (waits for an idle node and never preempts).
	Priority string
	Progress string
	// TTLSeconds is how long the build lives without a heartbeat; 0 is the default.
	TTLSeconds int
}

func buildTTL(seconds int) (time.Duration, error) {
	switch {
	case seconds < 0:
		return 0, invalidBuild("ttl_seconds must be positive")
	case seconds == 0:
		return BuildDefaultTTL, nil
	case int64(seconds) > int64(BuildMaxTTL/time.Second):
		// Compared in seconds, before multiplying: a huge value would overflow
		// the duration and slip under the cap.
		return BuildMaxTTL, nil
	}
	return time.Duration(seconds) * time.Second, nil
}

func checkBuildText(name, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "" && required:
		return "", invalidBuild("%s is required", name)
	case utf8.RuneCountInString(v) > max:
		return "", invalidBuild("%s must be at most %d characters", name, max)
	case strings.ContainsAny(v, "\r\n\x00"):
		return "", invalidBuild("%s must be a single line", name)
	}
	return v, nil
}

// buildNode is the configured transcode node a target names, if any.
func (e *Engine) buildNode(target string) (transcode.NamedWorker, bool) {
	return e.deps.Workers.Get(target)
}

func (e *Engine) markBuild(b store.Build) store.Build {
	_, b.HoldsTranscodeNode = e.buildNode(b.Target)
	return b
}

func (e *Engine) markBuilds(list []store.Build) []store.Build {
	for i := range list {
		list[i] = e.markBuild(list[i])
	}
	return list
}

// withNodeLock runs fn with the node's request lock, shared with SetNodePause,
// so a build change and an operator pause never interleave their writes and
// worker syncs. A target that is not a node needs no lock.
func (e *Engine) withNodeLock(target string, fn func(w transcode.NamedWorker, isNode bool)) {
	w, ok := e.buildNode(target)
	if ok {
		lock := &e.workerStateFor(w.Name).pauseMu
		lock.Lock()
		defer lock.Unlock()
	}
	fn(w, ok)
}

// CreateBuild starts a build and, on a node, brings the node's hold and freeze
// in line before it returns, so the caller can start its work. A worker that
// does not confirm the freeze does not fail the call: the node is still held
// and its status says the freeze is not confirmed.
func (e *Engine) CreateBuild(ctx context.Context, in BuildInput) (store.Build, error) {
	if e.deps.Store == nil {
		return store.Build{}, errors.New("store is required for builds")
	}
	var b store.Build
	var err error
	if in.Target = strings.TrimSpace(in.Target); !buildTargetPattern.MatchString(in.Target) {
		return b, invalidBuild("target is required: up to 64 characters of letters, digits, '.', '_' or '-'")
	}
	b.Target = in.Target
	if b.Source, err = checkBuildText("source", in.Source, buildFieldMax, true); err != nil {
		return b, err
	}
	if b.Holder, err = checkBuildText("holder", in.Holder, buildFieldMax, false); err != nil {
		return b, err
	}
	if b.Kind, err = checkBuildText("kind", in.Kind, buildFieldMax, false); err != nil {
		return b, err
	}
	if b.Title, err = checkBuildText("title", in.Title, buildTitleMax, true); err != nil {
		return b, err
	}
	if b.Progress, err = checkBuildText("progress", in.Progress, buildProgMax, false); err != nil {
		return b, err
	}
	switch in.PreemptMode {
	case "":
		b.PreemptMode = store.WorkerPauseSuspend
	case store.WorkerPauseGraceful, store.WorkerPauseImmediate, store.WorkerPauseSuspend:
		b.PreemptMode = in.PreemptMode
	default:
		return b, invalidBuild(`preempt_mode must be "suspend", "graceful" or "immediate"`)
	}
	switch in.Priority {
	case "", store.BuildPriorityNormal:
		b.Priority = store.BuildPriorityNormal
	case store.BuildPriorityLow:
		b.Priority = store.BuildPriorityLow
	default:
		return b, invalidBuild(`priority must be "normal" or "low"`)
	}
	if b.Priority == store.BuildPriorityLow {
		// A low priority build never preempts. admitOnNode serialises the
		// dispatcher's record step with promotion under the node's request
		// lock, so an encode and a low build do not start together; forcing
		// graceful is defence in depth should one ever slip through, since a
		// stronger mode would then freeze or requeue that encode. Whatever mode
		// was asked for, a low build holds the node gracefully.
		b.PreemptMode = store.WorkerPauseGraceful
	}
	ttl, err := buildTTL(in.TTLSeconds)
	if err != nil {
		return b, err
	}
	e.withNodeLock(b.Target, func(w transcode.NamedWorker, isNode bool) {
		// A low priority build waits only where there is a node to wait for.
		b, err = e.deps.Store.CreateBuild(b, e.now(), ttl, b.Priority == store.BuildPriorityLow && isNode)
		if err != nil || !isNode {
			return
		}
		if b.Status == store.BuildWaiting {
			// Oldest first, like the tick: the new build starts only if no older
			// waiting build on the node goes before it.
			e.promoteNodeLocked(w)
			if cur, gerr := e.deps.Store.GetBuild(b.ID); gerr == nil {
				b = cur
			}
		}
		if b.Status == store.BuildRunning {
			e.syncWorkerHold(ctx, w)
		}
	})
	if err != nil {
		return store.Build{}, err
	}
	e.WakeReconciler()
	return e.markBuild(b), nil
}

// buildHeartbeatGap is a test seam: it runs between a heartbeat's store update
// and its log append, to widen the window a missing lock would leave open.
var buildHeartbeatGap atomic.Pointer[func()]

// checkHeartbeat validates the log, the progress line and the ttl of a
// heartbeat, and returns the ttl (0 keeps the window the build has). progress
// is replaced by its trimmed form.
func checkHeartbeat(progress **string, ttlSeconds int, logLines []string) (time.Duration, error) {
	if err := checkBuildLog(logLines); err != nil {
		return 0, err
	}
	if *progress != nil {
		p, err := checkBuildText("progress", **progress, buildProgMax, false)
		if err != nil {
			return 0, err
		}
		*progress = &p
	}
	if ttlSeconds == 0 {
		return 0, nil
	}
	return buildTTL(ttlSeconds)
}

// OpenBuild is the first step of a heartbeat that comes from outside: it finds
// the build before anything the caller sent is read. An unknown build gives
// store.ErrBuildNotFound; an ended one gives store.ErrBuildEnded with its
// record. A running build whose expiry passed counts as ended too: it is marked
// lost here (releasing its node hold) rather than left for the next tick, and
// is answered the same way. Otherwise it returns the build, unchanged.
func (e *Engine) OpenBuild(ctx context.Context, id string) (store.Build, error) {
	cur, err := e.GetBuild(id)
	if err != nil {
		return store.Build{}, err
	}
	if cur.Status != store.BuildRunning && cur.Status != store.BuildWaiting {
		return cur, store.ErrBuildEnded
	}
	if exp, perr := time.Parse(time.RFC3339, cur.ExpiresAt); perr == nil && !exp.After(e.now()) {
		if b, err := e.HeartbeatBuild(ctx, id, nil, 0); err != nil {
			return b, err
		}
	}
	return cur, nil
}

// HeartbeatBuild extends a running build's expiry and optionally replaces its
// progress line and ttl. A build that already ended gives store.ErrBuildEnded
// together with its record.
func (e *Engine) HeartbeatBuild(ctx context.Context, id string, progress *string, ttlSeconds int, logLines ...string) (store.Build, error) {
	if e.deps.Store == nil {
		return store.Build{}, errStoreForBuilds
	}
	// Everything the caller sent is checked before anything is applied, and a
	// build that is gone or over answers as such whatever was wrong with it.
	ttl, err := checkHeartbeat(&progress, ttlSeconds, logLines)
	if err != nil {
		if cur, gerr := e.deps.Store.GetBuild(id); gerr != nil {
			return store.Build{}, gerr
		} else if cur.Status != store.BuildRunning && cur.Status != store.BuildWaiting {
			return e.markBuild(cur), store.ErrBuildEnded
		}
		return store.Build{}, err
	}
	// The update and the append are one step against a finish, a cancel or a
	// loss, and against another heartbeat: lines stay in order and never land
	// on an ended build.
	e.buildMu.Lock()
	b, err := e.deps.Store.HeartbeatBuild(id, e.now(), ttl, progress)
	if err == nil {
		if gap := buildHeartbeatGap.Load(); gap != nil {
			(*gap)()
		}
		e.buildLogs.append(id, logLines, e.now(), e.buildEnded)
	}
	e.buildMu.Unlock()
	if errors.Is(err, store.ErrBuildExpired) {
		// The late heartbeat marked the build lost: release its node hold the
		// way a finish does, and let a waiting build take the node.
		e.withNodeLock(b.Target, func(w transcode.NamedWorker, isNode bool) {
			if isNode {
				e.syncWorkerHold(ctx, w)
			}
		})
		e.promoteWaitingBuilds()
		e.WakeReconciler()
	}
	return e.markBuild(b), err
}

// FinishBuild ends a running build with a status (succeeded, failed or
// cancelled) and an optional exit code, then lets the node go on if no other
// build holds it. Finishing a build that already ended changes nothing: it
// returns the stored record, and changed is false.
func (e *Engine) FinishBuild(ctx context.Context, id, status string, exit *int) (b store.Build, changed bool, err error) {
	if e.deps.Store == nil {
		return store.Build{}, false, errors.New("store is required for builds")
	}
	switch status {
	case store.BuildSucceeded, store.BuildFailed, store.BuildCancelled:
	default:
		return store.Build{}, false, invalidBuild(`status must be "succeeded", "failed" or "cancelled"`)
	}
	current, err := e.deps.Store.GetBuild(id)
	if err != nil {
		return store.Build{}, false, err
	}
	e.withNodeLock(current.Target, func(w transcode.NamedWorker, isNode bool) {
		e.buildMu.Lock()
		b, changed, err = e.deps.Store.FinishBuild(id, status, exit, e.now())
		e.buildMu.Unlock()
		if err == nil && changed && isNode {
			e.syncWorkerHold(ctx, w)
		}
	})
	if err != nil {
		return store.Build{}, false, err
	}
	if changed {
		// The node may be idle for a waiting build now.
		e.promoteWaitingBuilds()
		e.WakeReconciler()
	}
	return e.markBuild(b), changed, nil
}

// GetBuild returns one build.
func (e *Engine) GetBuild(id string) (store.Build, error) {
	if e.deps.Store == nil {
		return store.Build{}, errors.New("store is required for builds")
	}
	b, err := e.deps.Store.GetBuild(id)
	return e.markBuild(b), err
}

// ListBuilds returns builds: the active ones (running, waiting) first, then the
// history, each newest first. The limit cuts the history, never an active build.
func (e *Engine) ListBuilds(f store.BuildFilter) ([]store.Build, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	list, err := e.deps.Store.ListBuilds(f)
	return e.markBuilds(list), err
}

// reconcileBuilds is the build part of the reconcile tick: running builds past
// their expiry become lost (which drops their hold), and history older than the
// retention is pruned. The worker holds and freezes follow in the same tick,
// since they are synced from the effective state.
func (e *Engine) reconcileBuilds(ctx context.Context) {
	if e.deps.Store == nil {
		return
	}
	now := e.now()
	e.buildMu.Lock()
	lost, err := e.deps.Store.ExpireBuilds(now)
	e.buildMu.Unlock()
	if err != nil {
		log.Printf("builds: expiring: %v", err)
	}
	for _, b := range lost {
		log.Printf("build %s on %s: no heartbeat before its expiry, marked lost", b.ID, b.Target)
	}
	if len(lost) > 0 {
		// The tick's hold sync lifts the hold of a node nothing else holds.
		e.WakeReconciler()
	}
	e.promoteWaitingBuilds()
	e.pruneBuildLogs()
	retention := 30 * 24 * time.Hour
	if e.deps.Config != nil {
		retention = e.deps.Config.Builds.RetentionDuration()
	}
	if n, err := e.deps.Store.PruneBuilds(now.Add(-retention)); err != nil {
		log.Printf("builds: pruning history: %v", err)
	} else if n > 0 {
		log.Printf("builds: pruned %d ended build(s) older than %s", n, retention)
	}
}

// nodeIdleForBuild is whether a waiting low priority build may start on the
// node: no operator pause, no running build, and no job on it (encoding,
// queued on the node, frozen, or finished and waiting to be saved). Anything
// that cannot be read counts as not idle.
func (e *Engine) nodeIdleForBuild(node string) bool {
	operator, err := e.operatorPauses()
	if err != nil {
		return false
	}
	if _, paused := operator[node]; paused {
		return false
	}
	holds, err := e.buildHolds()
	if err != nil {
		return false
	}
	if _, held := holds[node]; held {
		return false
	}
	rows, err := e.deps.Store.ListDrainActions()
	if err != nil {
		return false
	}
	for _, r := range rows {
		if e.resolveWorker(r.Worker) != node {
			continue
		}
		switch classifyDrainRow(r) {
		case transcode.DrainJobRunning, transcode.DrainJobPublicationPending:
			return false
		}
	}
	// A worker that says it runs jobs the rows do not show is not idle either.
	if active, _, ok := e.workerFrozen(node); ok && active > 0 {
		return false
	}
	return true
}

// promoteIfIdleLocked starts a waiting build when its node is idle. The node's
// request lock is held by the caller.
//
// Exclusivity with dispatch: the idle check and the promotion are both under
// the node's pauseMu, and a conversion records its job on the node (the
// identity checkpoint before its submit, see admitOnNode) under the same lock,
// re-checking the node's hold there. So either the job is recorded first and
// the idle check sees it, or the build is running first and the dispatch sees
// the hold: a low build and an encode never run on the node together.
func (e *Engine) promoteIfIdleLocked(w transcode.NamedWorker, b store.Build) (bool, error) {
	if !e.nodeIdleForBuild(w.Name) {
		return false, nil
	}
	if e.afterBuildIdleCheck != nil {
		e.afterBuildIdleCheck(w.Name)
	}
	return e.deps.Store.PromoteBuild(b.ID, e.now())
}

// promoteNodeLocked starts the oldest waiting build on the node when the node
// is idle (by created_at, then insertion order). A build whose expiry has
// passed is not started and the next one is tried. The node's request lock is
// held by the caller.
func (e *Engine) promoteNodeLocked(w transcode.NamedWorker) (bool, error) {
	waiting, err := e.deps.Store.WaitingBuilds()
	if err != nil {
		return false, err
	}
	for _, b := range waiting {
		if n, ok := e.buildNode(b.Target); !ok || n.Name != w.Name {
			continue
		}
		ok, err := e.promoteIfIdleLocked(w, b)
		if err != nil || ok {
			return ok, err
		}
		if !e.nodeIdleForBuild(w.Name) {
			return false, nil
		}
	}
	return false, nil
}

// admitOnNode is the dispatcher's side of the exclusivity with builds. It
// re-checks that the conversion's node takes new work and, under the node's
// request lock, runs record, which persists the job on the node (the state a
// build's idle check reads). It never waits for the lock: a busy node
// (a build promotion or an operator request in flight) defers the job for a
// pass. Lock order: the dispatcher holds its execution and parent leases, then
// only tries pauseMu; nothing that holds pauseMu waits for those leases on a
// blocking path, so there is no cycle. The lock is released before the submit.
func (e *Engine) admitOnNode(ec *ExecutionContext, phase string, record func() error) (StepResult, bool, error) {
	name := e.resolveWorker(assignedWorker(ec.State))
	if _, ok := e.buildNode(name); !ok {
		return StepResult{}, false, record()
	}
	lock := &e.workerStateFor(name).pauseMu
	if !lock.TryLock() {
		return pauseBlockedResult(phase, "a request for node "+name+" is in progress"), true, nil
	}
	defer lock.Unlock()
	if held, reason := e.pauseHold(name); held {
		return pauseBlockedResult(phase, reason), true, nil
	}
	return StepResult{}, false, record()
}

// promoteWaitingBuilds starts the waiting builds whose node is idle, oldest
// first, one per node at a time (a running build keeps the node from being
// idle). A waiting build is never promoted over a running one. A node whose
// request lock is busy is left for the next pass, so the reconcile loop never
// waits behind an operator request.
func (e *Engine) promoteWaitingBuilds() {
	if e.deps.Store == nil {
		return
	}
	waiting, err := e.deps.Store.WaitingBuilds()
	if err != nil {
		log.Printf("builds: reading waiting builds: %v", err)
		return
	}
	promoted := false
	// Nodes whose lock was busy this pass: every later waiting build on them
	// waits too, so a newer build never starts ahead of an older one.
	skipped := map[string]bool{}
	for _, b := range waiting {
		w, ok := e.buildNode(b.Target)
		if !ok || skipped[w.Name] {
			continue
		}
		// Re-read: an earlier iteration may have started or expired it.
		if cur, gerr := e.deps.Store.GetBuild(b.ID); gerr != nil || cur.Status != store.BuildWaiting {
			continue
		}
		lock := &e.workerStateFor(w.Name).pauseMu
		if e.beforeBuildTryLock != nil {
			e.beforeBuildTryLock(w.Name)
		}
		if !lock.TryLock() {
			skipped[w.Name] = true
			continue
		}
		ok, err := e.promoteIfIdleLocked(w, b)
		lock.Unlock()
		if err != nil {
			log.Printf("builds: starting %s: %v", b.ID, err)
		}
		promoted = promoted || ok
	}
	if promoted {
		// The tick's hold sync puts the hold on the node.
		e.WakeReconciler()
	}
}

// buildRank orders the preempt modes by how much they take from the node.
func buildRank(mode string) int {
	switch mode {
	case store.WorkerPauseImmediate:
		return 3
	case store.WorkerPauseSuspend:
		return 2
	case store.WorkerPauseGraceful:
		return 1
	}
	return 0
}

// buildHolds is the hold the running builds put on each configured node: the
// strongest preempt mode among them, starting at the oldest build.
func (e *Engine) buildHolds() (map[string]store.WorkerPause, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	running, err := e.deps.Store.RunningBuilds()
	if err != nil {
		return nil, err
	}
	out := map[string]store.WorkerPause{}
	for _, b := range running {
		w, ok := e.buildNode(b.Target)
		if !ok {
			continue
		}
		since := b.StartedAt
		if since == "" {
			since = b.CreatedAt
		}
		hold, seen := out[w.Name]
		if !seen {
			hold = store.WorkerPause{Worker: w.Name, Since: since, UpdatedAt: b.LastHeartbeatAt}
		}
		if buildRank(b.PreemptMode) > buildRank(hold.Mode) {
			hold.Mode = b.PreemptMode
		}
		if since < hold.Since {
			hold.Since = since
		}
		out[w.Name] = hold
	}
	return out, nil
}

// NodeBuilds is a node's running and waiting builds, oldest first, for its
// status.
func (e *Engine) NodeBuilds(node string) []transcode.NodeBuild {
	if e.deps.Store == nil {
		return nil
	}
	active, err := e.deps.Store.ActiveBuilds()
	if err != nil {
		return nil
	}
	var out []transcode.NodeBuild
	for _, b := range active {
		if w, ok := e.buildNode(b.Target); ok && w.Name == node {
			out = append(out, transcode.NodeBuild{ID: b.ID, Source: b.Source, Holder: b.Holder, Kind: b.Kind, Title: b.Title, PreemptMode: b.PreemptMode, Priority: b.Priority, Status: b.Status, StartedAt: b.StartedAt, Progress: b.Progress, CreatedAt: b.CreatedAt, LastHeartbeatAt: b.LastHeartbeatAt, ExpiresAt: b.ExpiresAt})
		}
	}
	return out
}

// operatorPauses is the operator's own pause rows, with no build hold.
func (e *Engine) operatorPauses() (map[string]store.WorkerPause, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	return e.deps.Store.ListWorkerPauses()
}
