package transcode

import "time"

// DrainStatus is the state of worker drain: while it is on, no new worker job
// is admitted, the jobs already on a worker finish and publish, and the status
// says when every worker is idle enough to be deployed. It is shared by the
// maintenance UI and the MCP tools.
type DrainStatus struct {
	Enabled  bool   `json:"enabled"`
	Since    string `json:"since,omitempty"`
	Revision int64  `json:"revision"`
	// Running counts the admitted worker jobs that are queued on a worker or
	// encoding; a job queued on a worker still starts there when a slot frees.
	Running int `json:"running"`
	// Waiting is how many of Running are queued on a worker and not encoding
	// yet.
	Waiting int `json:"waiting,omitempty"`
	// PublicationPending counts finished encodes whose output could not be
	// saved. The worker keeps them as open jobs, so they block a deploy until
	// they are saved (Try again) or cancelled.
	PublicationPending int `json:"publication_pending"`
	// Queued counts the work not admitted: children held by the drain and
	// batch files not started yet.
	Queued         int  `json:"queued"`
	ReadyForDeploy bool `json:"ready_for_deploy"`
	// Message says in a sentence what is left to wait for.
	Message string `json:"message"`
	// NotReady lists, one short sentence each, what keeps ready_for_deploy
	// false while drain is on; it is empty once the workers can be deployed.
	NotReady []string      `json:"not_ready,omitempty"`
	Workers  []DrainWorker `json:"workers,omitempty"`
	// Jobs lists the open jobs, publication-pending ones first.
	Jobs      []DrainJob `json:"jobs,omitempty"`
	CheckedAt time.Time  `json:"checked_at"`
}

// DrainWorker is one worker's share of the open jobs.
type DrainWorker struct {
	Name               string `json:"name"`
	Reachable          bool   `json:"reachable"`
	Running            int    `json:"running"`
	Waiting            int    `json:"waiting,omitempty"`
	PublicationPending int    `json:"publication_pending"`
	ReadyForDeploy     bool   `json:"ready_for_deploy"`
	Reason             string `json:"reason,omitempty"`
	// InFlight lists the files still running on this worker or encoded but
	// not saved yet: what a deploy of this worker would interrupt.
	InFlight []DrainJob `json:"in_flight,omitempty"`
}

// DrainJob is one open job, held job or unstarted batch file.
type DrainJob struct {
	ActionID string `json:"action_id"`
	ParentID string `json:"parent_id,omitempty"`
	Worker   string `json:"worker,omitempty"`
	File     string `json:"file,omitempty"`
	// Path is the file's full path, for opening it in Files.
	Path string `json:"path,omitempty"`
	// State is "running", "publication_pending" or "queued".
	State string `json:"state"`
	// Waiting marks a running job that is only queued on its worker.
	Waiting bool `json:"waiting,omitempty"`
}

// Drain job states.
const (
	DrainJobRunning            = "running"
	DrainJobPublicationPending = "publication_pending"
	DrainJobQueued             = "queued"
)
