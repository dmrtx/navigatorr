package action

import (
	"fmt"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

// suspendedByTitleMax keeps a build's title readable in a waiting reason.
const suspendedByTitleMax = 80

// suspendedBy says what froze the encodes of a worker: its answer is the
// "suspended_by" output of the action and the words of its waiting reason.
//
//	kind "build"    a running build holds the node (id, title, source, holder,
//	                started_at when the server knows the build)
//	kind "operator" a suspend the operator set on the node
//	kind "worker"   the worker froze it for a lease the server holds no record
//	                of (it was not told about the build, or not yet)
func (e *Engine) suspendedBy(worker string) (map[string]any, string) {
	cause := map[string]any{"kind": "worker"}
	if worker != "" {
		cause["node"] = worker
	}
	switch e.NodeHeldBy(worker) {
	case "operator":
		cause["kind"] = "operator"
		return cause, "its node is paused by the operator and the encode continues on resume"
	case "builds":
		cause["kind"] = "build"
		if b, ok := e.holdingBuild(worker); ok {
			cause["id"], cause["title"], cause["source"] = b.ID, b.Title, b.Source
			if b.Holder != "" {
				cause["holder"] = b.Holder
			}
			if b.StartedAt != "" {
				cause["started_at"] = b.StartedAt
			}
			return cause, fmt.Sprintf("a build holds its node (%s, from %s) and the encode continues when it ends", clipTitle(b.Title), b.Source)
		}
		return cause, "a build holds its node and the encode continues when it ends"
	}
	return cause, "its worker froze it for a build the server has no record of, and the encode continues when that ends"
}

// holdingBuild is the running build that holds a node: the one that has run
// the longest, as the node's hold counts from the first.
func (e *Engine) holdingBuild(worker string) (transcode.NodeBuild, bool) {
	var best transcode.NodeBuild
	found := false
	for _, b := range e.NodeBuilds(worker) {
		if b.Status != store.BuildRunning {
			continue
		}
		if !found || (b.StartedAt != "" && (best.StartedAt == "" || b.StartedAt < best.StartedAt)) {
			best, found = b, true
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
