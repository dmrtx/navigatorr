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
// free text; when it names a configured transcode node the build is about that
// node, but the server never holds the node for it.
//
// The server's builds are a pure record: create, heartbeat (forwarded progress
// and log lines), finish or cancel, expiry (a build whose heartbeat lapses
// becomes lost), history with its pruning, logs, and the per-node "builds
// paused" switch (create refuses with BuildsPausedError while it is on, and the
// node's worker relies on that answer). A build created here is running at once,
// whatever its priority: "low" and the preempt mode are stored as given and
// change nothing. A build the server holds a record of never makes a node wait
// and a build never waits.
//
// The node's worker is the only authority on who occupies the node: it takes
// the lease (see internal/transcodeworker and docs/transcode/BUILD_LEASES.md),
// holds its queue, freezes its encoders, refuses room and expires the lease by
// itself.
//
// Invariants.
//
//	N1  With a running lease on a node, no new conversion starts on that node.
//	    The worker alone guarantees it (queueHeld and the room it reports); the
//	    server does nothing for it.
//	N2  The transcode engine's dispatch, placement, pause view (workerPauses),
//	    drain and freeze logic never read build records. Outside the builds
//	    record files (builds.go, builds_pause.go, buildlog.go) nothing in this
//	    package calls a store build function.
//	N3  The routes /api/maintenance/builds* and the builds-only token keep their
//	    request and response shapes. holds_transcode_node stays in the JSON for
//	    compatibility and is always false.
//
// What the server shows of a node's builds (the UI) is the registry's view, read
// from these records; why an encode is frozen comes from the leases its worker
// reports (see suspendedBy), not from here.

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

// BuildIDTakenError is a create whose client id already names a build of
// another target or source.
type BuildIDTakenError struct{ ID string }

func (e *BuildIDTakenError) Error() string {
	return "id " + e.ID + " already names another build"
}

// BuildViaNodeLease is the "via" a node's worker sends when it registers one of
// its own leases. A create that targets a configured node without it holds
// nothing there, so it is refused (NodeLeaseRequiredError).
const BuildViaNodeLease = "node_lease"

// NodeLeaseRequiredCode is the stable code of a NodeLeaseRequiredError answer.
const NodeLeaseRequiredCode = "node_lease_required"

// NodeLeaseRequiredError is a create for a configured node that did not come
// from that node's worker: the server holds no node for a build.
type NodeLeaseRequiredError struct{ Node string }

func (e *NodeLeaseRequiredError) Error() string {
	return "builds on node " + e.Node + " are leased by its worker: run nbuild on that node"
}

// BuildIDTakenCode is the stable code of a BuildIDTakenError answer.
const BuildIDTakenCode = "build_id_taken"

// buildClientID is the shape of a client-chosen build id.
var buildClientID = regexp.MustCompile(`^bld_[A-Za-z0-9_-]{8,59}$`)

// BuildInput is a request to start a build.
type BuildInput struct {
	// ID is an optional client-chosen id ("bld_" and 8 to 59 letters, digits,
	// '_' or '-'). It makes the create idempotent: asking again with the same
	// id, target and source returns that build as it is now (even ended)
	// and creates nothing. A client that retries a create whose answer it
	// did not get (a timeout) needs it, or every retry is one more build.
	ID          string
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
	// Via is "node_lease" when the node's worker registers its own lease.
	Via string
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

// CreateBuild records a build. It is running at once, whatever its priority,
// and holds nothing: on a node the worker's lease is what holds the node. The
// only refusal that depends on the node is the builds pause.
//
// Pause versus create: the builds-pause lock below orders this check against
// the toggle, so a record is either refused or registered before the pause.
// The operator's transcode pause (SetNodePause) is deliberately not part of
// it: a record holds nothing, so one registered while that pause is being set
// changes no admission anywhere. The worker already granted its lease locally
// (a lease is only ever refused by the builds pause, checked by the worker
// before it grants); the record is bookkeeping that follows the lease and may
// lag it or, after a refusal, be absent (the lease is then unrecorded).
func (e *Engine) CreateBuild(ctx context.Context, in BuildInput) (store.Build, error) {
	if e.deps.Store == nil {
		return store.Build{}, errors.New("store is required for builds")
	}
	var b store.Build
	var err error
	if in.ID != "" && !buildClientID.MatchString(in.ID) {
		return b, invalidBuild(`id must be "bld_" and 8 to 59 letters, digits, '_' or '-'`)
	}
	b.ID = in.ID
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
	ttl, err := buildTTL(in.TTLSeconds)
	if err != nil {
		return b, err
	}
	w, isNode := e.buildNode(b.Target)
	if isNode && in.Via != BuildViaNodeLease {
		return b, &NodeLeaseRequiredError{Node: w.Name}
	}
	// A retry of a create already made is answered before any lock.
	if got, found, err := e.buildByClientID(b); found || err != nil {
		return got, err
	}
	if isNode {
		// The node's builds-pause lock orders the check against the toggle, so
		// a build is either refused here or registered before the pause.
		lock := &e.workerStateFor(w.Name).buildsPauseMu
		lock.Lock()
		defer lock.Unlock()
	}
	// Again under the lock: two creates of one id serialise here.
	if got, found, err := e.buildByClientID(b); found || err != nil {
		return got, err
	}
	if isNode {
		paused, err := e.buildsPausedOn(w.Name)
		if err != nil {
			return store.Build{}, err
		}
		if paused {
			return store.Build{}, &BuildsPausedError{Node: w.Name}
		}
		if e.afterBuildsPausedCheck != nil {
			e.afterBuildsPausedCheck(w.Name)
		}
	}
	want := b
	b, err = e.deps.Store.CreateBuild(b, e.now(), ttl)
	if err != nil {
		// A target that is not a node has no lock: a create of the same id can
		// win the insert between the check and here.
		if got, found, ferr := e.buildByClientID(want); found {
			return got, ferr
		}
		return store.Build{}, err
	}
	return b, nil
}

// buildByClientID is the build a client id already names. found is false when
// the request has no id or the id is new; an id that names a build of another
// target or source is a BuildIDTakenError.
func (e *Engine) buildByClientID(want store.Build) (store.Build, bool, error) {
	if want.ID == "" {
		return store.Build{}, false, nil
	}
	got, err := e.deps.Store.GetBuild(want.ID)
	switch {
	case errors.Is(err, store.ErrBuildNotFound):
		return store.Build{}, false, nil
	case err != nil:
		return store.Build{}, true, err
	case got.Target != want.Target || got.Source != want.Source:
		return store.Build{}, true, &BuildIDTakenError{ID: want.ID}
	}
	return got, true, nil
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
// lost here rather than left for the next tick, and is answered the same way.
// Otherwise it returns the build, unchanged.
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
			return cur, store.ErrBuildEnded
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
	return b, err
}

// FinishBuild ends a running build with a status (succeeded, failed or
// cancelled) and an optional exit code. Finishing a build that already ended
// changes nothing: it returns the stored record, and changed is false.
func (e *Engine) FinishBuild(ctx context.Context, id, status string, exit *int) (b store.Build, changed bool, err error) {
	if e.deps.Store == nil {
		return store.Build{}, false, errors.New("store is required for builds")
	}
	switch status {
	case store.BuildSucceeded, store.BuildFailed, store.BuildCancelled:
	default:
		return store.Build{}, false, invalidBuild(`status must be "succeeded", "failed" or "cancelled"`)
	}
	e.buildMu.Lock()
	b, changed, err = e.deps.Store.FinishBuild(id, status, exit, e.now())
	e.buildMu.Unlock()
	if err != nil {
		return store.Build{}, false, err
	}
	return b, changed, nil
}

// GetBuild returns one build.
func (e *Engine) GetBuild(id string) (store.Build, error) {
	if e.deps.Store == nil {
		return store.Build{}, errors.New("store is required for builds")
	}
	return e.deps.Store.GetBuild(id)
}

// ListBuilds returns builds: the active ones (running, waiting) first, then the
// history, each newest first. The limit cuts the history, never an active build.
func (e *Engine) ListBuilds(f store.BuildFilter) ([]store.Build, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	return e.deps.Store.ListBuilds(f)
}

// reconcileBuilds is the build part of the reconcile tick: running (or, from an
// old row, waiting) builds past their expiry become lost, waiting ones left by
// an earlier server are cancelled, and history older
// than the retention is pruned.
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
	// An earlier server could leave a build waiting; nothing promotes it now.
	e.buildMu.Lock()
	cancelled, err := e.deps.Store.CancelWaitingBuilds(now)
	e.buildMu.Unlock()
	if err != nil {
		log.Printf("builds: cancelling legacy waiting builds: %v", err)
	}
	for _, b := range cancelled {
		log.Printf("build %s on %s: %s", b.ID, b.Target, store.LegacyWaitingReason)
	}
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

// NodeBuildRecords is a node's running and waiting build records, oldest first,
// for the registry's view of it (the UI). It is a record read only: nothing in
// the engine decides on it.
func (e *Engine) NodeBuildRecords(node string) []transcode.NodeBuild {
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

// RunningLeases is how many build leases the node's worker reports running,
// and when the oldest began. It reads the last worker observation (memory
// only, up to a few seconds old), never the build records: the worker is the
// only authority on what holds the node, the records are history.
func (e *Engine) RunningLeases(node string) (int, string) {
	n, since := 0, ""
	for _, l := range e.workerLeases(node) {
		if l.Status != "running" {
			continue
		}
		if n == 0 {
			since = l.StartedAt
		}
		n++
	}
	return n, since
}

// AttachNodeBuilds adds each node's build records to its pause status, for the
// callers that show the registry's view next to the pause. The records are a
// list only. "Held by builds" comes solely from the leases the node's worker
// reports running; a running record with no reported lease holds nothing.
func (e *Engine) AttachNodeBuilds(statuses map[string]transcode.NodePauseStatus) map[string]transcode.NodePauseStatus {
	for name, st := range statuses {
		st.Builds = e.NodeBuildRecords(name)
		// An operator pause wins over the display of a lease.
		if st.HeldBy == "" {
			if running, since := e.RunningLeases(name); running > 0 {
				st.Paused, st.HeldBy = true, "builds"
				st.Since = since
				for _, b := range st.Builds {
					if b.Status == store.BuildRunning {
						st.Mode = b.PreemptMode
						break
					}
				}
				st.Message = fmt.Sprintf("Busy with %d running build(s). New jobs wait on this node.", running)
			}
		}
		statuses[name] = st
	}
	return statuses
}
