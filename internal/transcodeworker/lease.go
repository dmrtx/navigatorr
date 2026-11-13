package transcodeworker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Build leases: local arbitration between builds and transcodes.
//
// A build (see tools/navigatorr-builds/nbuild in the infrastructure repo) asks
// the worker of the node it is about to use, over a Unix socket (lease_http.go),
// for a lease. The worker is the single decider: it grants the lease, freezes
// the encoders the way a suspend does (or, for a low priority lease, waits for
// the node to be idle and freezes nothing), and holds its queue while the lease
// lives. The build keeps running its own command; it only heartbeats the lease.
// A lease whose heartbeats stop expires by itself and the worker thaws the
// encoders and resumes the queue, with no network and no server involved.
//
// The state is a durable record in the state directory (build-leases.json),
// written atomically. Runners (separate processes, see suspend.go) read it the
// way they read the suspend record, so the freeze needs nothing from the
// socket server and survives its restart. A lease past its expiry never
// counts, whoever reads the record and whether or not the worker has marked it
// lost yet.
//
// The server learns about a lease through the existing builds API (see
// lease_report.go), best effort and in the background: the lease never waits
// for it, except that a new normal lease asks the server once, briefly, whether
// the operator paused builds on the node.

const (
	leaseFile = "build-leases.json"

	leaseDefaultTTL = 5 * time.Minute
	leaseMaxTTL     = 2 * time.Hour
	// leaseRestartGrace is the least life a lease has after the worker
	// restarts: the build's next heartbeat is not due before then.
	leaseRestartGrace = 30 * time.Second
	// leaseEndedKeep is how long an ended lease stays on record, so a late
	// heartbeat is answered "ended" and its server report can be retried.
	leaseEndedKeep = time.Hour

	leaseStatusWaiting   = "waiting"
	leaseStatusRunning   = "running"
	leaseStatusSucceeded = "succeeded"
	leaseStatusFailed    = "failed"
	leaseStatusCancelled = "cancelled"
	leaseStatusLost      = "lost"

	leaseFieldMax = 64
	leaseTitleMax = 200
	leaseProgMax  = 200
	leaseLogMax   = 256 // lines kept for the next server report
)

// errLeasesPaused is a new lease refused because the operator paused builds
// on the node.
var errLeasesPaused = errors.New("builds paused")

// leaseInvalidError is a request the caller can fix.
type leaseInvalidError struct{ Reason string }

func (e *leaseInvalidError) Error() string { return e.Reason }

var errLeaseNotFound = errors.New("build not found")

// errLeaseEnded accompanies the record of a lease that is over.
var errLeaseEnded = errors.New("build already ended")

type leaseRecord struct {
	ID          string `json:"id"`
	ServerID    string `json:"server_id,omitempty"`
	Source      string `json:"source"`
	Holder      string `json:"holder,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Title       string `json:"title"`
	Priority    string `json:"priority"`
	PreemptMode string `json:"preempt_mode"`
	Status      string `json:"status"`
	// Freeze is whether the lease freezes the encoders while it runs: a
	// normal lease does (suspend, or immediate, which the server handles
	// further), a graceful or low one only holds the queue.
	Freeze          bool       `json:"freeze"`
	Progress        string     `json:"progress,omitempty"`
	TTLSeconds      int        `json:"ttl_seconds"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	LastHeartbeatAt time.Time  `json:"last_heartbeat_at"`
	ExpiresAt       time.Time  `json:"expires_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	// Reported is whether the server was told the lease ended (or never knew
	// it, so there is nothing to tell).
	Reported bool `json:"reported,omitempty"`
	// Confirmed is whether the encoders were seen frozen for the lease (true
	// from the start for one that freezes nothing): until then it is not
	// answered "running". EndReason says why the worker ended it itself.
	Confirmed bool   `json:"confirmed,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
}

func (l *leaseRecord) active() bool {
	return l.Status == leaseStatusWaiting || l.Status == leaseStatusRunning
}

// holds is whether the lease keeps the queue held now.
func (l *leaseRecord) holds(now time.Time) bool {
	return l.Status == leaseStatusRunning && l.ExpiresAt.After(now)
}

type leaseFileData struct {
	// FreezeGeneration names the freeze the running leases impose: set when the
	// first freezing lease starts, zero while none does. Runners report the
	// generation they froze under, like for a suspend.
	FreezeGeneration int64          `json:"freeze_generation,omitempty"`
	Leases           []*leaseRecord `json:"leases"`
}

// leaseState is the lease bookkeeping of a Worker.
type leaseState struct {
	mu sync.Mutex
	// create serializes LeaseCreate from the id lookup to the answer: builds
	// are rare, and one at a time leaves no race between two asks of one id.
	create sync.Mutex
	// pending is what the next server report carries, per lease id.
	pending map[string]*leasePending
	// now is the clock; tests replace it.
	now      func() time.Time
	reporter leaseReporter
	kick     chan struct{}
}

type leasePending struct {
	progress *string
	log      []string
	dirty    bool
	lastSent time.Time
}

func (s *leaseState) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (w *Worker) leasePath() string {
	if w == nil || w.cfg == nil || w.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.StateDir, leaseFile)
}

// readLeaseFile reads the record. A missing one is empty; an unreadable one
// is an error (callers decide: runners freeze, the manager heals corruption).
func (w *Worker) readLeaseFile() (leaseFileData, error) {
	path := w.leasePath()
	if path == "" {
		return leaseFileData{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leaseFileData{}, nil
	}
	if err != nil {
		return leaseFileData{}, err
	}
	if strings.TrimSpace(string(data)) == "" {
		return leaseFileData{}, fmt.Errorf("%w: empty", errLeaseCorrupt)
	}
	var f leaseFileData
	if err := json.Unmarshal(data, &f); err != nil {
		return leaseFileData{}, fmt.Errorf("%w: %v", errLeaseCorrupt, err)
	}
	return f, nil
}

// errLeaseCorrupt is a record that is empty or not JSON, as opposed to one that
// could not be read at all.
var errLeaseCorrupt = errors.New("lease record is corrupt")

// leasesEnabled is whether this worker offers leases.
func (w *Worker) leasesEnabled() bool {
	return w != nil && w.cfg != nil && strings.TrimSpace(w.cfg.BuildNode) != ""
}

// leaseFreeze is what the runners read: whether a lease freezes the encoders
// now and under which generation. Like the suspend record, an unreadable lease
// record freezes: a frozen encode is recoverable, a build that shares its CPU
// with an encode is what the lease exists to prevent. Without any record
// nothing is frozen.
func (w *Worker) leaseFreeze() (bool, int64) {
	f, err := w.readLeaseFile()
	if err != nil {
		// Without leases nothing would ever heal the record: ignore it.
		return w.leasesEnabled(), unreadableSuspendGeneration
	}
	now := time.Now()
	for _, l := range f.Leases {
		if l.holds(now) && l.Freeze {
			if f.FreezeGeneration > unreadableSuspendGeneration {
				return true, f.FreezeGeneration
			}
			return true, unreadableSuspendGeneration
		}
	}
	return false, 0
}

// leaseHolds is whether a lease keeps the queue held now.
func (w *Worker) leaseHolds() bool {
	f, err := w.readLeaseFile()
	if err != nil {
		return w.leasesEnabled()
	}
	now := time.Now()
	for _, l := range f.Leases {
		if l.holds(now) {
			return true
		}
	}
	return false
}

// freezeState is the freeze runners apply: the server's suspend record, or,
// when there is none, the one the leases impose.
func (w *Worker) freezeState() (bool, int64) {
	if on, g := w.suspendState(); on {
		return true, g
	}
	return w.leaseFreeze()
}

// saveLeasesLocked writes the record atomically and settles the freeze
// generation. The caller holds leases.mu.
func (w *Worker) saveLeasesLocked(f *leaseFileData, now time.Time) error {
	path := w.leasePath()
	if path == "" {
		return errors.New("worker has no state_dir to store the leases")
	}
	freezing := false
	for _, l := range f.Leases {
		if l.holds(now) && l.Freeze {
			freezing = true
		}
	}
	switch {
	case !freezing:
		f.FreezeGeneration = 0
	case f.FreezeGeneration <= unreadableSuspendGeneration:
		f.FreezeGeneration = now.UnixNano()
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "build-leases-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// loadLeasesLocked reads the record for a change. A record that is empty or not
// JSON is replaced by an empty one at once: runners fail closed on it, so
// leaving it would keep the node held. Any other read error (I/O, permission)
// is returned and nothing is rewritten: the live leases may be fine.
func (w *Worker) loadLeasesLocked() (leaseFileData, error) {
	f, err := w.readLeaseFile()
	if errors.Is(err, errLeaseCorrupt) {
		log.Printf("build leases: record unusable, starting over: %v", err)
		empty := leaseFileData{}
		if serr := w.saveLeasesLocked(&empty, w.leases.clock()); serr != nil {
			return leaseFileData{}, serr
		}
		return empty, nil
	}
	return f, err
}

var clientLeaseID = regexp.MustCompile(`^bld_[A-Za-z0-9_-]{8,56}$`)

// leaseFreezeWait bounds how long a new normal lease waits for the running
// encoders to confirm they are frozen. A var so tests can shorten it.
var leaseFreezeWait = 10 * time.Second

func newLeaseID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("bld_l%x", time.Now().UnixNano())
	}
	return "bld_l" + hex.EncodeToString(b[:])
}

func leaseText(name, v string, max int, required bool) (string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "" && required:
		return "", &leaseInvalidError{name + " is required"}
	case utf8.RuneCountInString(v) > max:
		return "", &leaseInvalidError{fmt.Sprintf("%s must be at most %d characters", name, max)}
	case strings.ContainsAny(v, "\r\n\x00"):
		return "", &leaseInvalidError{name + " must be a single line"}
	}
	return v, nil
}

func leaseTTL(seconds int) (time.Duration, error) {
	switch {
	case seconds < 0:
		return 0, &leaseInvalidError{"ttl_seconds must be positive"}
	case seconds == 0:
		return leaseDefaultTTL, nil
	case int64(seconds) > int64(leaseMaxTTL/time.Second):
		return leaseMaxTTL, nil
	}
	return time.Duration(seconds) * time.Second, nil
}

// LeaseRequest is a request for a lease.
type LeaseRequest struct {
	// ID is an optional client-chosen id ("bld_" and 8 to 56 letters, digits,
	// '_' or '-'): asking again with the same id returns the lease it made
	// instead of a second one, so a caller whose answer was lost can retry.
	ID          string
	Source      string
	Holder      string
	Kind        string
	Title       string
	PreemptMode string // "", suspend, graceful, immediate
	Priority    string // "", normal, low
	Progress    string
	TTL         time.Duration // 0 is the default; tests pass short ones
	TTLSeconds  int           // used when TTL is zero
}

// checkLeaseRequest validates and normalises a request into a waiting record.
func (w *Worker) checkLeaseRequest(in LeaseRequest) (*leaseRecord, error) {
	var err error
	l := &leaseRecord{ID: newLeaseID()}
	if in.ID != "" {
		if !clientLeaseID.MatchString(in.ID) {
			return nil, &leaseInvalidError{`id must be "bld_" and 8 to 56 letters, digits, '_' or '-'`}
		}
		l.ID = in.ID
	}
	if l.Source, err = leaseText("source", in.Source, leaseFieldMax, true); err != nil {
		return nil, err
	}
	if l.Holder, err = leaseText("holder", in.Holder, leaseFieldMax, false); err != nil {
		return nil, err
	}
	if l.Kind, err = leaseText("kind", in.Kind, leaseFieldMax, false); err != nil {
		return nil, err
	}
	if l.Title, err = leaseText("title", in.Title, leaseTitleMax, true); err != nil {
		return nil, err
	}
	if l.Progress, err = leaseText("progress", in.Progress, leaseProgMax, false); err != nil {
		return nil, err
	}
	switch in.PreemptMode {
	case "":
		l.PreemptMode = "suspend"
	case "suspend", "graceful", "immediate":
		l.PreemptMode = in.PreemptMode
	default:
		return nil, &leaseInvalidError{`preempt_mode must be "suspend", "graceful" or "immediate"`}
	}
	switch in.Priority {
	case "", "normal":
		l.Priority = "normal"
	case "low":
		l.Priority = "low"
		l.PreemptMode = "graceful" // a low lease never preempts
	default:
		return nil, &leaseInvalidError{`priority must be "normal" or "low"`}
	}
	l.Freeze = l.PreemptMode != "graceful"
	ttl := in.TTL
	if ttl == 0 {
		if ttl, err = leaseTTL(in.TTLSeconds); err != nil {
			return nil, err
		}
	}
	l.TTLSeconds = int(ttl / time.Second)
	if l.TTLSeconds < 1 {
		l.TTLSeconds = 1
	}
	l.Status = leaseStatusWaiting
	l.Confirmed = !l.Freeze
	return l, nil
}

func (l *leaseRecord) ttl() time.Duration {
	if l.TTLSeconds <= 0 {
		return leaseDefaultTTL
	}
	return time.Duration(l.TTLSeconds) * time.Second
}

// grantLocked makes a waiting lease run. The caller holds leases.mu and, so no
// scheduler pass starts a job between the check and the write, the capacity
// lock.
func (w *Worker) grantLocked(f *leaseFileData, l *leaseRecord, now time.Time) error {
	t := now.UTC()
	l.Status = leaseStatusRunning
	l.StartedAt = &t
	l.LastHeartbeatAt = t
	l.ExpiresAt = now.Add(l.ttl()).UTC()
	if !l.Confirmed {
		// Born unconfirmed: the wait for the freeze must not eat its ttl.
		l.ExpiresAt = l.ExpiresAt.Add(leaseFreezeWait)
	}
	return w.saveLeasesLocked(f, now)
}

// errLeaseFreeze is a normal lease the running encoders did not freeze for in
// time: it ended at once (nothing is held) and the answer is a refusal.
var errLeaseFreeze = errors.New("the encoders did not freeze in time")

// LeaseCreate asks for a lease. It runs alone (one create at a time) from the
// lookup of a client id to the answer. A normal lease is answered "running"
// only once every running encoder reported itself frozen (up to
// leaseFreezeWait); if one did not, the lease is ended as cancelled and
// errLeaseFreeze returned, with no server call: the report pass tells the
// server later. Asking again with the same id gets the stored outcome, never
// "running" for a lease whose freeze is not confirmed. A low lease waits for an
// idle node and is promoted by the tick. The returned record is a copy.
func (w *Worker) LeaseCreate(in LeaseRequest) (leaseRecord, error) {
	s := &w.leases
	l, err := w.checkLeaseRequest(in)
	if err != nil {
		return leaseRecord{}, err
	}
	s.create.Lock()
	defer s.create.Unlock()
	if in.ID != "" {
		if got, found, err := w.leaseKnown(l.ID); found || err != nil {
			return got, err
		}
	}
	if l.Priority == "low" {
		return w.leaseInsert(l, false)
	}
	// The server decides only whether builds are paused: a network call, with
	// no lock but the create one. An unreachable server does not stop a build.
	if rep := s.reporter; rep != nil {
		id, err := rep.Register(l)
		if errors.Is(err, errLeasesPaused) {
			return leaseRecord{}, err
		}
		if err == nil {
			l.ServerID = id
		}
	}
	got, err := w.leaseInsert(l, true)
	if err != nil || !got.Freeze {
		return got, err
	}
	return w.leaseConfirm(got.ID)
}

// leaseKnown is the stored outcome of a client id. found is false for an id
// never seen.
func (w *Worker) leaseKnown(id string) (rec leaseRecord, found bool, err error) {
	s := &w.leases
	s.mu.Lock()
	f, err := w.loadLeasesLocked()
	for _, e := range f.Leases {
		if err == nil && e.ID == id {
			rec, found = *e, true
		}
	}
	s.mu.Unlock()
	switch {
	case err != nil:
		return rec, true, err
	case !found:
		return rec, false, nil
	case !rec.active() && rec.EndReason == "freeze":
		return rec, true, errLeaseFreeze
	case !rec.active():
		return rec, true, errLeaseEnded
	case rec.Confirmed:
		return rec, true, nil
	}
	// Granted but never confirmed (the worker died in between): confirm now.
	rec, err = w.leaseConfirm(id)
	return rec, true, err
}

// leaseInsert stores l: waiting (low) or running (granted under the capacity
// lock, so no job starts between the check and the write). The clock is read
// under the locks.
func (w *Worker) leaseInsert(l *leaseRecord, grant bool) (leaseRecord, error) {
	s := &w.leases
	if grant {
		capLock, err := acquireCapacityLock(filepath.Clean(w.cfg.StateDir))
		if err != nil {
			return leaseRecord{}, err
		}
		defer capLock.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		return leaseRecord{}, err
	}
	l.CreatedAt, l.LastHeartbeatAt = now.UTC(), now.UTC()
	l.ExpiresAt = now.Add(l.ttl()).UTC()
	f.Leases = append(f.Leases, l)
	if grant {
		err = w.grantLocked(&f, l, now)
	} else {
		err = w.saveLeasesLocked(&f, now)
	}
	if err != nil {
		return leaseRecord{}, err
	}
	w.leaseKickReporter()
	return *l, nil
}

// leaseConfirm waits for the running encoders to report themselves frozen and
// then confirms the lease (its ttl starts now) or, on a timeout, ends it.
func (w *Worker) leaseConfirm(id string) (leaseRecord, error) {
	deadline := time.Now().Add(leaseFreezeWait)
	frozen := false
	for {
		active, n, err := w.suspendCounts()
		if frozen = err == nil && n >= active; frozen || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := &w.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		return leaseRecord{}, err
	}
	for _, l := range f.Leases {
		if l.ID != id {
			continue
		}
		if !l.active() {
			return *l, errLeaseEnded
		}
		if frozen {
			l.Confirmed = true
			l.LastHeartbeatAt = now.UTC()
			l.ExpiresAt = now.Add(l.ttl()).UTC()
		} else {
			t := now.UTC()
			l.Status, l.EndedAt, l.EndReason = leaseStatusCancelled, &t, "freeze"
		}
		if err := w.saveLeasesLocked(&f, now); err != nil {
			return leaseRecord{}, err
		}
		w.leaseKickReporter()
		if !frozen {
			return *l, errLeaseFreeze
		}
		return *l, nil
	}
	return leaseRecord{}, errLeaseNotFound
}

// leasePrunable is whether an ended lease can leave the record: an hour after
// it ended when its report is settled or there is nobody to report to; a
// report that keeps failing is given up after a day.
func (w *Worker) leasePrunable(l *leaseRecord, now time.Time) bool {
	if l.active() || l.EndedAt == nil {
		return false
	}
	age := now.Sub(*l.EndedAt)
	return age > 24*leaseEndedKeep || (age > leaseEndedKeep && (l.Reported || w.leases.reporter == nil))
}

// leaseExpireLocked marks every active lease past its expiry lost and drops
// ended ones that outlived their keep. It reports whether it changed anything.
func (w *Worker) leaseExpireLocked(f *leaseFileData, now time.Time) bool {
	changed := false
	kept := f.Leases[:0]
	for _, l := range f.Leases {
		if l.active() && !l.ExpiresAt.After(now) {
			t := now.UTC()
			l.Status, l.EndedAt = leaseStatusLost, &t
			changed = true
			log.Printf("build lease %s: no heartbeat before its expiry, marked lost", l.ID)
		}
		if w.leasePrunable(l, now) {
			changed = true
			delete(w.leases.pending, l.ID)
			continue
		}
		kept = append(kept, l)
	}
	f.Leases = kept
	return changed
}

// leaseLostLocked marks the expired leases lost and saves the record, for a
// caller that found one past its expiry before the tick did.
func (w *Worker) leaseLostLocked(f *leaseFileData, now time.Time) {
	w.leaseExpireLocked(f, now)
	if err := w.saveLeasesLocked(f, now); err != nil {
		log.Printf("build leases: not saved: %v", err)
	}
	w.leaseKickReporter()
}

// leaseNodeIdleLocked is whether a low lease may start: nothing runs or waits
// in the worker, nobody holds it, and no other lease runs.
func (w *Worker) leaseNodeIdleLocked(f *leaseFileData, now time.Time) bool {
	if w.Held() || w.Suspended() {
		return false
	}
	for _, l := range f.Leases {
		if l.holds(now) {
			return false
		}
	}
	if n, err := w.countActiveJobs(""); err != nil || n > 0 {
		return false
	}
	if q, err := w.listQueuedJobsObserved(); err != nil || len(q) > 0 {
		return false
	}
	return true
}

// leaseTick is the worker's periodic step: it marks the leases that missed
// their heartbeats lost (the encoders thaw when no other lease freezes) and
// promotes the oldest waiting low lease when the node is idle.
func (w *Worker) leaseTick() {
	s := &w.leases
	now := s.clock()
	// Most ticks have nothing to do: look before taking the capacity lock. An
	// unreadable record is work: loading it heals it.
	if f, err := w.readLeaseFile(); err == nil {
		work := false
		for _, l := range f.Leases {
			if l.Status == leaseStatusWaiting || (l.active() && !l.ExpiresAt.After(now)) || w.leasePrunable(l, now) {
				work = true
			}
		}
		if !work {
			return
		}
	}
	// The idle check and the grant are one step against a scheduler pass.
	capLock, err := acquireCapacityLock(filepath.Clean(w.cfg.StateDir))
	if err != nil {
		return
	}
	defer capLock.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	now = s.clock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		log.Printf("build leases: not read: %v", err)
		return
	}
	changed := w.leaseExpireLocked(&f, now)
	sort.SliceStable(f.Leases, func(i, j int) bool { return f.Leases[i].CreatedAt.Before(f.Leases[j].CreatedAt) })
	for _, l := range f.Leases {
		if l.Status != leaseStatusWaiting {
			continue
		}
		if !w.leaseNodeIdleLocked(&f, now) {
			break
		}
		if err := w.grantLocked(&f, l, now); err != nil {
			log.Printf("build lease %s: not granted: %v", l.ID, err)
			break
		}
		changed = false // grantLocked saved
		w.leaseKickReporter()
		break // one at a time: the next sees the node held
	}
	if changed {
		if err := w.saveLeasesLocked(&f, now); err != nil {
			log.Printf("build leases: not saved: %v", err)
		}
	}
	w.leaseKickReporter()
}

// LeaseGet returns a lease. An ended one comes back with errLeaseEnded.
func (w *Worker) LeaseGet(id string) (leaseRecord, error) {
	s := &w.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		return leaseRecord{}, err
	}
	for _, l := range f.Leases {
		if l.ID == id {
			if !l.active() {
				return *l, errLeaseEnded
			}
			if now := s.clock(); !l.ExpiresAt.After(now) {
				w.leaseLostLocked(&f, now)
				return *l, errLeaseEnded
			}
			return *l, nil
		}
	}
	return leaseRecord{}, errLeaseNotFound
}

// LeaseHeartbeat extends a lease and records its progress, its output lines
// and a new ttl. An ended lease answers errLeaseEnded with its record.
func (w *Worker) LeaseHeartbeat(id string, progress *string, ttlSeconds int, lines []string) (leaseRecord, error) {
	s := &w.leases
	var ttl time.Duration
	if ttlSeconds != 0 {
		var err error
		if ttl, err = leaseTTL(ttlSeconds); err != nil {
			return leaseRecord{}, err
		}
	}
	if progress != nil {
		p, err := leaseText("progress", *progress, leaseProgMax, false)
		if err != nil {
			return leaseRecord{}, err
		}
		progress = &p
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) > 2000 || strings.ContainsAny(line, "\x00") {
			return leaseRecord{}, &leaseInvalidError{"log lines must be text of at most 2000 characters"}
		}
	}
	if len(lines) > 64 {
		return leaseRecord{}, &leaseInvalidError{"log must have at most 64 lines"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		return leaseRecord{}, err
	}
	for _, l := range f.Leases {
		if l.ID != id {
			continue
		}
		if !l.active() {
			return *l, errLeaseEnded
		}
		if !l.ExpiresAt.After(now) {
			// Past its expiry it is over, like on the server: lost, not revived.
			w.leaseLostLocked(&f, now)
			return *l, errLeaseEnded
		}
		if ttl > 0 {
			l.TTLSeconds = int(ttl / time.Second)
		}
		l.LastHeartbeatAt = now.UTC()
		l.ExpiresAt = now.Add(l.ttl()).UTC()
		if progress != nil {
			l.Progress = *progress
		}
		if err := w.saveLeasesLocked(&f, now); err != nil {
			return leaseRecord{}, err
		}
		p := s.pendingFor(id)
		if progress != nil {
			p.progress = progress
		}
		p.log = append(p.log, lines...)
		if len(p.log) > leaseLogMax {
			p.log = p.log[len(p.log)-leaseLogMax:]
		}
		p.dirty = true
		w.leaseKickReporter()
		return *l, nil
	}
	return leaseRecord{}, errLeaseNotFound
}

func (s *leaseState) pendingFor(id string) *leasePending {
	if s.pending == nil {
		s.pending = map[string]*leasePending{}
	}
	p := s.pending[id]
	if p == nil {
		p = &leasePending{}
		s.pending[id] = p
	}
	return p
}

// LeaseFinish ends a lease with a status. The queue and the encoders go on
// when no other lease holds them. Finishing an ended lease changes nothing.
func (w *Worker) LeaseFinish(id, status string, exit *int) (leaseRecord, error) {
	switch status {
	case leaseStatusSucceeded, leaseStatusFailed, leaseStatusCancelled:
	default:
		return leaseRecord{}, &leaseInvalidError{`status must be "succeeded", "failed" or "cancelled"`}
	}
	return w.leaseEnd(id, status, exit)
}

func (w *Worker) leaseEnd(id, status string, exit *int) (leaseRecord, error) {
	s := &w.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		return leaseRecord{}, err
	}
	for _, l := range f.Leases {
		if l.ID != id {
			continue
		}
		if !l.active() {
			return *l, nil
		}
		if !l.ExpiresAt.After(now) {
			// Too late to say how it went: it was lost first.
			w.leaseLostLocked(&f, now)
			return *l, nil
		}
		t := now.UTC()
		l.Status, l.EndedAt, l.ExitCode = status, &t, exit
		if err := w.saveLeasesLocked(&f, now); err != nil {
			return leaseRecord{}, err
		}
		w.leaseKickReporter()
		return *l, nil
	}
	return leaseRecord{}, errLeaseNotFound
}

// leaseRestart runs once when the worker starts.
func (w *Worker) leaseRestart() {
	// A restart keeps the leases: the freeze is in the record, the builds
	// are still running. They get a short grace so a heartbeat that was due
	// during the restart is not lost with it.
	s := &w.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	f, err := w.loadLeasesLocked()
	if err != nil {
		log.Printf("build leases: not read: %v", err)
		return
	}
	changed := false
	for _, l := range f.Leases {
		if !l.active() {
			continue
		}
		if min := now.Add(min(leaseRestartGrace, l.ttl())).UTC(); l.ExpiresAt.After(now) && l.ExpiresAt.Before(min) {
			l.ExpiresAt = min
			changed = true
		}
	}
	if w.leaseExpireLocked(&f, now) || changed {
		if err := w.saveLeasesLocked(&f, now); err != nil {
			log.Printf("build leases: not saved: %v", err)
		}
	}
}
