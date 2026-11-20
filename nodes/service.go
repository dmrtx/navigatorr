package nodes

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Topology tells the registry which build targets are configured nodes. The
// transcode engine's worker set sits behind it (wired in main); Nodes does not
// import it.
type Topology interface {
	// Node resolves a build target to a configured node's name.
	Node(target string) (name string, ok bool)
	// Nodes lists the configured nodes' names.
	Nodes() []string
}

// TranscodeState is what the transcode engine reports of one node: its
// operator pause and what holds it, as the worker last said. Nodes only shows
// it; it decides nothing on it.
type TranscodeState struct {
	Paused  bool   `json:"paused"`
	Mode    string `json:"mode,omitempty"`
	Since   string `json:"since,omitempty"`
	HeldBy  string `json:"held_by,omitempty"`
	Message string `json:"message,omitempty"`
}

// TranscodeView reads the transcode state of every node, by node name.
type TranscodeView interface {
	TranscodeStates(ctx context.Context) map[string]TranscodeState
}

// Options configures a Service. Only Path is required.
type Options struct {
	// Path is the SQLite file of the registry.
	Path     string
	Topology Topology
	// Transcode is optional; without it the node views carry no transcode state.
	Transcode TranscodeView
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Retention is how long an ended build is kept; nil or 0 is 30 days.
	Retention func() time.Duration
}

// DefaultRetention is the history kept when no retention is configured.
const DefaultRetention = 30 * 24 * time.Hour

// Service is the build registry.
type Service struct {
	reg       *registry
	topo      Topology
	tv        TranscodeView
	nowFn     func() time.Time
	retention func() time.Duration

	// logs holds the output tails of builds, in memory only.
	logs buildLogs
	// buildMu makes a heartbeat's update and log append one step against the
	// transitions that end a build.
	buildMu sync.Mutex

	// pauseLocks serialise the builds-pause toggle with a build create on the
	// node, so a build is either refused or registered before the pause.
	pauseMu    sync.Mutex
	pauseLocks map[string]*sync.Mutex

	// afterBuildsPausedCheck is a test seam: it runs in CreateBuild between the
	// builds-pause check and the insert, with the node's lock held.
	afterBuildsPausedCheck func(node string)

	startOnce sync.Once
}

// Open opens (creating it if needed) the registry database and returns the
// service. Close it when done.
func Open(opts Options) (*Service, error) {
	reg, err := openRegistry(opts.Path)
	if err != nil {
		return nil, err
	}
	s := &Service{reg: reg, topo: opts.Topology, tv: opts.Transcode, nowFn: opts.Now, retention: opts.Retention, pauseLocks: map[string]*sync.Mutex{}}
	return s, nil
}

// Close closes the database.
func (s *Service) Close() error { return s.reg.close() }

func (s *Service) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// node resolves a target to a configured node.
func (s *Service) node(target string) (string, bool) {
	if s.topo == nil {
		return "", false
	}
	return s.topo.Node(target)
}

func (s *Service) pauseLock(node string) *sync.Mutex {
	s.pauseMu.Lock()
	defer s.pauseMu.Unlock()
	m := s.pauseLocks[node]
	if m == nil {
		m = &sync.Mutex{}
		s.pauseLocks[node] = m
	}
	return m
}

func buildTTL(seconds int) (time.Duration, error) {
	switch {
	case seconds < 0:
		return 0, invalid("ttl_seconds must be positive")
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
		return "", invalid("%s is required", name)
	case utf8.RuneCountInString(v) > max:
		return "", invalid("%s must be at most %d characters", name, max)
	case strings.ContainsAny(v, "\r\n\x00"):
		return "", invalid("%s must be a single line", name)
	}
	return v, nil
}

// CreateBuild registers a build. It is running at once, whatever its priority,
// and holds nothing: on a node the worker's lease is what holds the node. The
// only refusal that depends on the node is the builds pause.
//
// Pause versus create: the node's lock orders the pause check against the
// toggle, so a record is either refused or registered before the pause. The
// worker already granted its lease locally (a lease is only ever refused by the
// builds pause, checked by the worker before it grants); the record is
// bookkeeping that follows the lease and may lag it or, after a refusal, be
// absent.
func (s *Service) CreateBuild(ctx context.Context, in BuildInput) (Build, error) {
	var b Build
	var err error
	if in.ID != "" && !buildClientID.MatchString(in.ID) {
		return b, invalid(`id must be "bld_" and 8 to 59 letters, digits, '_' or '-'`)
	}
	b.ID = in.ID
	if in.Target = strings.TrimSpace(in.Target); !buildTargetPattern.MatchString(in.Target) {
		return b, invalid("target is required: up to 64 characters of letters, digits, '.', '_' or '-'")
	}
	b.Target = s.canonicalTarget(in.Target)
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
		b.PreemptMode = PreemptSuspend
	case PreemptGraceful, PreemptImmediate, PreemptSuspend:
		b.PreemptMode = in.PreemptMode
	default:
		return b, invalid(`preempt_mode must be "suspend", "graceful" or "immediate"`)
	}
	switch in.Priority {
	case "", BuildPriorityNormal:
		b.Priority = BuildPriorityNormal
	case BuildPriorityLow:
		b.Priority = BuildPriorityLow
	default:
		return b, invalid(`priority must be "normal" or "low"`)
	}
	ttl, err := buildTTL(in.TTLSeconds)
	if err != nil {
		return b, err
	}
	node, isNode := s.node(b.Target)
	if isNode && in.Via != BuildViaNodeLease {
		return b, &NodeLeaseRequiredError{Node: node}
	}
	// A retry of a create already made is answered before any lock.
	if got, found, err := s.buildByClientID(b); found || err != nil {
		return got, err
	}
	if isNode {
		lock := s.pauseLock(node)
		lock.Lock()
		defer lock.Unlock()
	}
	// Again under the lock: two creates of one id serialise here.
	if got, found, err := s.buildByClientID(b); found || err != nil {
		return got, err
	}
	if isNode {
		paused, err := s.reg.paused(node)
		if err != nil {
			return Build{}, err
		}
		if paused {
			return Build{}, &BuildsPausedError{Node: node}
		}
		if s.afterBuildsPausedCheck != nil {
			s.afterBuildsPausedCheck(node)
		}
	}
	want := b
	b, err = s.reg.create(b, s.now(), ttl)
	if err != nil {
		// A target that is not a node has no lock: a create of the same id can
		// win the insert between the check and here.
		if got, found, ferr := s.buildByClientID(want); found {
			return got, ferr
		}
		return Build{}, err
	}
	return b, nil
}

// buildByClientID is the build a client id already names. found is false when
// the request has no id or the id is new; an id that names a build of another
// target or source is a BuildIDTakenError.
func (s *Service) buildByClientID(want Build) (Build, bool, error) {
	if want.ID == "" {
		return Build{}, false, nil
	}
	got, err := s.reg.get(want.ID)
	switch {
	case errors.Is(err, ErrBuildNotFound):
		return Build{}, false, nil
	case err != nil:
		return Build{}, true, err
	case got.Target != want.Target || got.Source != want.Source:
		return Build{}, true, &BuildIDTakenError{ID: want.ID}
	}
	return got, true, nil
}

// buildHeartbeatGap is a test seam: it runs between a heartbeat's update and
// its log append, to widen the window a missing lock would leave open.
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
// ErrBuildNotFound; an ended one gives ErrBuildEnded with its record. A running
// build whose expiry passed counts as ended too: it is marked lost here rather
// than left for the next tick, and is answered the same way. Otherwise it
// returns the build, unchanged.
func (s *Service) OpenBuild(ctx context.Context, id string) (Build, error) {
	cur, err := s.GetBuild(id)
	if err != nil {
		return Build{}, err
	}
	if !cur.Active() {
		return cur, ErrBuildEnded
	}
	if exp, perr := time.Parse(time.RFC3339, cur.ExpiresAt); perr == nil && !exp.After(s.now()) {
		if b, err := s.HeartbeatBuild(ctx, id, nil, 0); err != nil {
			return b, err
		}
	}
	return cur, nil
}

// HeartbeatBuild extends a running build's expiry and optionally replaces its
// progress line and ttl. A build that already ended gives ErrBuildEnded
// together with its record.
func (s *Service) HeartbeatBuild(ctx context.Context, id string, progress *string, ttlSeconds int, logLines ...string) (Build, error) {
	// Everything the caller sent is checked before anything is applied, and a
	// build that is gone or over answers as such whatever was wrong with it.
	ttl, err := checkHeartbeat(&progress, ttlSeconds, logLines)
	if err != nil {
		if cur, gerr := s.reg.get(id); gerr != nil {
			return Build{}, gerr
		} else if !cur.Active() {
			return cur, ErrBuildEnded
		}
		return Build{}, err
	}
	// The update and the append are one step against a finish, a cancel or a
	// loss, and against another heartbeat: lines stay in order and never land
	// on an ended build.
	s.buildMu.Lock()
	b, err := s.reg.heartbeat(id, s.now(), ttl, progress)
	if err == nil {
		if gap := buildHeartbeatGap.Load(); gap != nil {
			(*gap)()
		}
		s.logs.append(id, logLines, s.now(), s.buildEnded)
	}
	s.buildMu.Unlock()
	if errors.Is(err, ErrBuildExpired) {
		s.logs.wake(id)
	}
	return b, err
}

// FinishBuild ends a running build with a status (succeeded, failed or
// cancelled) and an optional exit code. Finishing a build that already ended
// changes nothing: it returns the stored record, and changed is false.
func (s *Service) FinishBuild(ctx context.Context, id, status string, exit *int) (b Build, changed bool, err error) {
	switch status {
	case BuildSucceeded, BuildFailed, BuildCancelled:
	default:
		return Build{}, false, invalid(`status must be "succeeded", "failed" or "cancelled"`)
	}
	s.buildMu.Lock()
	b, changed, err = s.reg.finish(id, status, exit, s.now())
	s.buildMu.Unlock()
	if err != nil {
		return Build{}, false, err
	}
	if changed {
		s.logs.wake(id)
	}
	return b, changed, nil
}

// CancelBuild is FinishBuild with status cancelled: the record is marked ended
// and the next heartbeat the build tool sends is answered "build already
// ended", which is how it learns to stop. It does not touch the process on the
// build machine. Cancelling a build that already ended changes nothing.
func (s *Service) CancelBuild(ctx context.Context, id string) (Build, bool, error) {
	return s.FinishBuild(ctx, id, BuildCancelled, nil)
}

// GetBuild returns one build.
func (s *Service) GetBuild(id string) (Build, error) { return s.reg.get(id) }

// ListBuilds returns builds: the active ones first, then the history, each
// newest first. The limit cuts the history, never an active build.
func (s *Service) ListBuilds(f BuildFilter) ([]Build, error) {
	f.Target = s.canonicalTarget(f.Target)
	return s.reg.list(f)
}

// BuildPage is one page of PageBuilds.
type BuildPage struct {
	Builds []Build
	// Next is the cursor of the next page; empty on the last page.
	Next string
}

// PageBuilds is the listing /nodes/v1 pages through: every matching build,
// newest first by creation order (the registry's insertion number), active and
// ended alike. Unlike ListBuilds it never reorders by status, so a build that
// finishes between two pages neither repeats nor disappears, and a cursor
// never goes stale: it is the insertion number of the last build of the
// previous page, valid whether or not that build still exists. limit must be
// 1 or more; cursor is "" for the first page, otherwise a Next of an earlier
// page (InvalidCursorError for anything else).
func (s *Service) PageBuilds(f BuildFilter, cursor string, limit int) (BuildPage, error) {
	if limit < 1 {
		return BuildPage{}, invalid("limit must be 1 or more")
	}
	var before int64
	if cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != cursor {
			return BuildPage{}, &InvalidCursorError{Cursor: cursor}
		}
		before = n
	}
	f.Target = s.canonicalTarget(f.Target)
	got, more, err := s.reg.page(f, before, limit)
	if err != nil {
		return BuildPage{}, err
	}
	p := BuildPage{Builds: got}
	if more {
		p.Next = strconv.FormatInt(got[len(got)-1].seq, 10)
	}
	return p, nil
}

// canonicalTarget is the configured node's own name when the target names one
// (what the topology resolves it to), else the target as given.
func (s *Service) canonicalTarget(target string) string {
	if target == "" {
		return ""
	}
	if name, ok := s.node(target); ok {
		return name
	}
	return target
}

// Reconcile is one pass of the housekeeping: running builds past their expiry
// become lost, waiting ones left by an earlier server are cancelled, output
// buffers of ended builds are released and history older than the retention is
// pruned.
func (s *Service) Reconcile(ctx context.Context) {
	now := s.now()
	s.buildMu.Lock()
	lost, err := s.reg.expire(now)
	s.buildMu.Unlock()
	if err != nil {
		log.Printf("builds: expiring: %v", err)
	}
	for _, b := range lost {
		s.logs.wake(b.ID)
		log.Printf("build %s on %s: no heartbeat before its expiry, marked lost", b.ID, b.Target)
	}
	// An earlier server could leave a build waiting; nothing promotes it now.
	s.buildMu.Lock()
	cancelled, err := s.reg.cancelWaiting(now)
	s.buildMu.Unlock()
	if err != nil {
		log.Printf("builds: cancelling legacy waiting builds: %v", err)
	}
	for _, b := range cancelled {
		s.logs.wake(b.ID)
		log.Printf("build %s on %s: %s", b.ID, b.Target, LegacyWaitingReason)
	}
	s.pruneBuildLogs()
	retention := DefaultRetention
	if s.retention != nil {
		if r := s.retention(); r > 0 {
			retention = r
		}
	}
	if n, err := s.reg.prune(now.Add(-retention)); err != nil {
		log.Printf("builds: pruning history: %v", err)
	} else if n > 0 {
		log.Printf("builds: pruned %d ended build(s) older than %s", n, retention)
	}
}

// Start runs Reconcile every interval (5 seconds when zero) until ctx ends. It
// returns a channel closed when the loop has stopped; wait on it before Close.
func (s *Service) Start(ctx context.Context, interval time.Duration) <-chan struct{} {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	done := make(chan struct{})
	s.startOnce.Do(func() {
		go func() {
			defer close(done)
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				s.Reconcile(ctx)
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	})
	return done
}

// ActiveOn is a node's running and waiting build records, oldest first. It is a
// record read only: nothing decides on it.
func (s *Service) ActiveOn(node string) []Build {
	node = s.canonicalTarget(node)
	active, err := s.reg.active()
	if err != nil {
		return nil
	}
	var out []Build
	for _, b := range active {
		if n, ok := s.node(b.Target); ok && n == node {
			out = append(out, b)
		}
	}
	return out
}

// BuildsPauses is the nodes whose builds are paused, by node name.
func (s *Service) BuildsPauses() (map[string]BuildPause, error) { return s.reg.listPauses() }

// BuildsPaused reports whether new builds on the node are refused.
func (s *Service) BuildsPaused(node string) (bool, error) {
	return s.reg.paused(s.canonicalTarget(node))
}

// SetBuildsPause pauses or resumes new builds on a node. Repeating a request
// changes nothing. It touches no build and no transcode pause. The node must be
// configured (UnknownNodeError otherwise).
func (s *Service) SetBuildsPause(ctx context.Context, node string, paused bool) error {
	name, ok := s.node(node)
	if !ok {
		return &UnknownNodeError{Node: node}
	}
	// The node's lock (the one CreateBuild takes) orders this against a create
	// that checks the flag, so a build is either refused or registered before
	// the pause.
	lock := s.pauseLock(name)
	lock.Lock()
	err := s.reg.setPause(name, paused, s.now())
	lock.Unlock()
	return err
}

// RunningBuild is a running build on a node, as a node's view lists it.
type RunningBuild struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	Holder    string `json:"holder,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Title     string `json:"title"`
	StartedAt string `json:"started_at,omitempty"`
	Progress  string `json:"progress,omitempty"`
	// The rest is the record's own state, so a reader needs no second call.
	Status          string `json:"status,omitempty"`
	Priority        string `json:"priority,omitempty"`
	PreemptMode     string `json:"preempt_mode,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
}

// NodeView is everything the registry knows of one configured node.
type NodeView struct {
	Name              string          `json:"name"`
	BuildsPaused      bool            `json:"builds_paused"`
	BuildsPausedSince string          `json:"builds_paused_since,omitempty"`
	RunningBuilds     []RunningBuild  `json:"running_builds"`
	Transcode         *TranscodeState `json:"transcode,omitempty"`
}

// Nodes lists every configured node, sorted by name. A builds-pause state that
// cannot be read reads as paused (the create path fails closed too).
func (s *Service) Nodes(ctx context.Context) ([]NodeView, error) {
	return s.nodes(ctx, true)
}

// NodesWithoutTranscode is Nodes without asking the TranscodeView: no node
// carries a transcode block.
func (s *Service) NodesWithoutTranscode(ctx context.Context) ([]NodeView, error) {
	return s.nodes(ctx, false)
}

func (s *Service) nodes(ctx context.Context, withTranscode bool) ([]NodeView, error) {
	if s.topo == nil {
		return []NodeView{}, nil
	}
	names := append([]string(nil), s.topo.Nodes()...)
	sort.Strings(names)
	pauses, perr := s.reg.listPauses()
	var states map[string]TranscodeState
	if withTranscode && s.tv != nil {
		states = s.tv.TranscodeStates(ctx)
	}
	out := make([]NodeView, 0, len(names))
	for _, name := range names {
		out = append(out, s.nodeView(name, pauses, perr, states))
	}
	return out, nil
}

// Node is one node's view; UnknownNodeError when it is not configured.
func (s *Service) Node(ctx context.Context, name string) (NodeView, error) {
	// The topology's own name for the node is what every table and view is
	// keyed by, whatever spelling the caller used.
	canon, ok := s.node(name)
	if !ok {
		return NodeView{}, &UnknownNodeError{Node: name}
	}
	name = canon
	pauses, perr := s.reg.listPauses()
	var states map[string]TranscodeState
	if s.tv != nil {
		states = s.tv.TranscodeStates(ctx)
	}
	return s.nodeView(name, pauses, perr, states), nil
}

func (s *Service) nodeView(name string, pauses map[string]BuildPause, perr error, states map[string]TranscodeState) NodeView {
	v := NodeView{Name: name, RunningBuilds: []RunningBuild{}}
	if p, ok := pauses[name]; ok {
		v.BuildsPaused, v.BuildsPausedSince = true, p.Since
	} else if perr != nil {
		v.BuildsPaused = true
	}
	for _, b := range s.ActiveOn(name) {
		if b.Status == BuildRunning {
			v.RunningBuilds = append(v.RunningBuilds, RunningBuild{
				ID: b.ID, Source: b.Source, Holder: b.Holder, Kind: b.Kind, Title: b.Title, StartedAt: b.StartedAt, Progress: b.Progress,
				Status: b.Status, Priority: b.Priority, PreemptMode: b.PreemptMode, CreatedAt: b.CreatedAt, LastHeartbeatAt: b.LastHeartbeatAt, ExpiresAt: b.ExpiresAt,
			})
		}
	}
	if st, ok := states[name]; ok {
		v.Transcode = &st
	}
	return v
}
