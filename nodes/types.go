package nodes

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Build statuses. A build is running until its owner finishes it (succeeded,
// failed or cancelled) or its heartbeat lapses (lost).
const (
	// BuildWaiting is no longer created: builds never wait. Old rows (and a
	// worker's own leases) may still carry it; it is read as an active build
	// and expires like a running one.
	BuildWaiting   = "waiting"
	BuildRunning   = "running"
	BuildSucceeded = "succeeded"
	BuildFailed    = "failed"
	BuildCancelled = "cancelled"
	BuildLost      = "lost"
)

// Build priorities. The registry only records them: what a priority means on
// a node is decided by the node's worker.
const (
	BuildPriorityNormal = "normal"
	BuildPriorityLow    = "low"
)

// Preempt modes, stored as given. They change nothing here.
const (
	PreemptGraceful  = "graceful"
	PreemptImmediate = "immediate"
	PreemptSuspend   = "suspend"
)

const (
	// BuildDefaultTTL is how long a build lives without a heartbeat when the
	// request names no ttl_seconds.
	BuildDefaultTTL = 15 * time.Minute
	// BuildMaxTTL caps ttl_seconds on create and on heartbeat.
	BuildMaxTTL = 2 * time.Hour
	// BuildDefaultLimit is how many ended builds a list returns without a limit.
	BuildDefaultLimit = 100
	// BuildMinTTL is the window a heartbeat without a ttl falls back to only
	// when the build's own window cannot be read.
	BuildMinTTL = time.Minute

	buildFieldMax = 64
	buildTitleMax = 200
	buildProgMax  = 200
)

// BuildViaNodeLease is the "via" a node's worker sends when it registers one of
// its own leases. A create that targets a configured node without it holds
// nothing there, so it is refused (NodeLeaseRequiredError).
const BuildViaNodeLease = "node_lease"

// Stable codes of the refusals.
const (
	NodeLeaseRequiredCode = "node_lease_required"
	BuildIDTakenCode      = "build_id_taken"
	BuildsPausedCode      = "builds_paused"
)

// LegacyWaitingReason is the progress an old waiting build ends with.
const LegacyWaitingReason = "server-side waiting builds were removed; retry"

// ErrBuildNotFound and ErrBuildEnded are what the build calls report for an
// unknown build and for a change to a build that already ended.
var (
	ErrBuildNotFound = errors.New("build not found")
	ErrBuildEnded    = errors.New("build already ended")
)

// ErrBuildExpired is what a heartbeat reports when it arrives after the build's
// expiry but before the reconcile tick marked it lost. The build is marked lost
// right there; the error wraps ErrBuildEnded.
var ErrBuildExpired = fmt.Errorf("%w: heartbeat after its expiry, marked lost", ErrBuildEnded)

var errNoDB = errors.New("nodes registry is not open")

// Build is one job another tool runs on a machine: a gradle build, an
// integration test, a warm-up. Target is free text; when it names a configured
// node the build is about that node (its worker holds the lease). The record
// stays as history after it ends.
type Build struct {
	ID          string `json:"id"`
	Target      string `json:"target"`
	Source      string `json:"source"`
	Holder      string `json:"holder"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	PreemptMode string `json:"preempt_mode"`
	Priority    string `json:"priority"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	// StartedAt is when the build began to run: its creation.
	StartedAt       string `json:"started_at,omitempty"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
	ExpiresAt       string `json:"expires_at"`
	EndedAt         string `json:"ended_at,omitempty"`
	Progress        string `json:"progress,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	// HoldsTranscodeNode is kept for compatibility and is always false: the
	// registry never holds a node for a build, the node's worker does.
	HoldsTranscodeNode bool `json:"holds_transcode_node"`
}

// Active is whether the build is still running (or, on an old row, waiting).
func (b Build) Active() bool { return b.Status == BuildRunning || b.Status == BuildWaiting }

// BuildFilter narrows ListBuilds. Empty fields match everything.
type BuildFilter struct {
	Target string
	Status string
	Source string
	// Limit applies to the ended history only; 0 means 100, negative means none.
	Limit int
}

// BuildInput is a request to register a build.
type BuildInput struct {
	// ID is an optional client-chosen id ("bld_" and 8 to 59 letters, digits,
	// '_' or '-'). It makes the create idempotent: asking again with the same
	// id, target and source returns that build as it is now (even ended) and
	// creates nothing.
	ID          string
	Target      string
	Source      string
	Holder      string
	Kind        string
	Title       string
	PreemptMode string
	// Priority is "normal" (default) or "low". Stored, never acted on here.
	Priority string
	Progress string
	// Via is "node_lease" when the node's worker registers its own lease.
	Via string
	// TTLSeconds is how long the build lives without a heartbeat; 0 is the default.
	TTLSeconds int
}

var (
	buildTargetPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	buildClientID      = regexp.MustCompile(`^bld_[A-Za-z0-9_-]{8,59}$`)
)

// InvalidError is a request the caller can fix.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

// BuildIDTakenError is a create whose client id already names a build of
// another target or source.
type BuildIDTakenError struct{ ID string }

func (e *BuildIDTakenError) Error() string { return "id " + e.ID + " already names another build" }

// NodeLeaseRequiredError is a create for a configured node that did not come
// from that node's worker: the registry holds no node for a build.
type NodeLeaseRequiredError struct{ Node string }

func (e *NodeLeaseRequiredError) Error() string {
	return "builds on node " + e.Node + " are leased by its worker: run nbuild on that node"
}

// BuildsPausedError is a new build refused because builds are paused on its node.
type BuildsPausedError struct{ Node string }

func (e *BuildsPausedError) Error() string { return "builds paused on " + e.Node }

// UnknownNodeError is a request for a node that is not configured.
type UnknownNodeError struct{ Node string }

func (e *UnknownNodeError) Error() string { return "unknown node " + e.Node }
