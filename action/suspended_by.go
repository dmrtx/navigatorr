package action

import (
	"fmt"

	"github.com/jakenesler/navigatorr/transcode"
)

// suspendedByTitleMax keeps a build's title readable in a waiting reason.
const suspendedByTitleMax = 80

// suspendedBy says what froze the encodes of a worker: its answer is the
// "suspended_by" output of the action and the words of its waiting reason.
//
//	kind "operator" a suspend the operator set on the node
//	kind "build"    the worker reports a running lease (id, title, source,
//	                started_at), from its latest observation
//	kind "worker"   the worker froze it for something it did not report
//
// The server never decides on this: it only explains what the worker said.
func (e *Engine) suspendedBy(worker string) (map[string]any, string) {
	cause := map[string]any{"kind": "worker"}
	if worker != "" {
		cause["node"] = worker
	}
	if e.NodeHeldBy(worker) == "operator" {
		cause["kind"] = "operator"
		return cause, "its node is paused by the operator and the encode continues on resume"
	}
	if l, ok := e.holdingLease(worker); ok {
		cause["kind"] = "build"
		cause["id"], cause["title"], cause["source"] = l.ID, l.Title, l.Source
		if l.StartedAt != "" {
			cause["started_at"] = l.StartedAt
		}
		return cause, fmt.Sprintf("a build holds its node (%s, from %s) and the encode continues when it ends", clipTitle(l.Title), l.Source)
	}
	return cause, "its worker froze it for a build the server has no record of, and the encode continues when that ends"
}

// holdingLease is the running lease that holds a node as its worker last
// reported: the one that has run the longest.
func (e *Engine) holdingLease(worker string) (transcode.WorkerLease, bool) {
	var best transcode.WorkerLease
	found := false
	for _, l := range e.workerLeases(worker) {
		if l.Status != "running" {
			continue
		}
		if !found || (l.StartedAt != "" && (best.StartedAt == "" || l.StartedAt < best.StartedAt)) {
			best, found = l, true
		}
	}
	return best, found
}

func clipTitle(title string) string {
	runes := []rune(title)
	if len(runes) <= suspendedByTitleMax {
		return title
	}
	return string(runes[:suspendedByTitleMax-1]) + "…"
}
