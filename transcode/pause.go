package transcode

import (
	"errors"
	"time"
)

// ErrSuspendUnavailable is a suspend the node could not take: it is
// unreachable, or its worker predates the suspend. Nothing was changed.
var ErrSuspendUnavailable = errors.New("suspend unavailable")

// ErrSuspendNotUndone is a suspend the node did not confirm and whose undo
// also failed: unlike ErrSuspendUnavailable, the pause IS stored, as suspend.
// The node keeps its queue held, and a Resume (or another mode) lifts it.
var ErrSuspendNotUndone = errors.New("suspend not confirmed and not undone")

// NodePauseStatus is one node's pause: the operator's switch that takes the
// node out of service so its CPU is free for other work, and what is left
// before it is. It is shared by the maintenance UI and the workers list.
type NodePauseStatus struct {
	Node string `json:"node"`
	// Mode is "graceful" (the encodes already running finish), "immediate"
	// (they go back to the transcode queue now), "suspend" (they are frozen
	// where they are and continue on resume) or "" when the node is not
	// paused.
	Mode      string `json:"mode"`
	Paused    bool   `json:"paused"`
	Since     string `json:"since,omitempty"`
	Reachable bool   `json:"reachable"`
	// Running counts the jobs still on the node: encoding there, or sent and
	// not started yet. Immediate mode brings it to zero by itself.
	Running int `json:"running"`
	// Suspended counts the encodes frozen on the node by a suspend. They are
	// not in Running: the node's CPU is free while they wait.
	Suspended int `json:"suspended,omitempty"`
	// PublicationPending counts finished encodes the node could not save yet;
	// they keep the node busy until they are saved.
	PublicationPending int `json:"publication_pending"`
	// Idle is true when the node is paused and nothing runs on it, so the
	// machine can be used for something else.
	Idle bool `json:"idle"`
	// Message says in a sentence what is left to wait for.
	Message   string    `json:"message"`
	CheckedAt time.Time `json:"checked_at"`
}
