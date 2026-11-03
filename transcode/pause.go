package transcode

import "time"

// NodePauseStatus is one node's pause: the operator's switch that takes the
// node out of service so its CPU is free for other work, and what is left
// before it is. It is shared by the maintenance UI and the workers list.
type NodePauseStatus struct {
	Node string `json:"node"`
	// Mode is "graceful" (the encodes already running finish), "immediate"
	// (they go back to the transcode queue now) or "" when the node is not
	// paused.
	Mode      string `json:"mode"`
	Paused    bool   `json:"paused"`
	Since     string `json:"since,omitempty"`
	Reachable bool   `json:"reachable"`
	// Running counts the jobs still on the node: encoding there, or sent and
	// not started yet. Immediate mode brings it to zero by itself.
	Running int `json:"running"`
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
