package action

import (
	"context"
	"errors"

	"github.com/jakenesler/navigatorr/transcode"
)

// Builds pause.
//
// A node's builds pause is a second switch beside its transcode pause, and the
// two are independent: any combination is valid. While builds are paused on a
// node, CreateBuild refuses a new build whose target is that node, with
// BuildsPausedError; nothing is created and no hold is taken. Builds that are
// already registered (running or waiting) are not touched, and neither
// pausing nor resuming changes them. Other nodes and targets that are not
// nodes are unaffected. The pause is one durable row per node (see
// store.SetBuildPause), so a restart keeps it.

// BuildsPausedCode is the stable machine code of the refusal.
const BuildsPausedCode = "builds_paused"

// BuildsPausedError is a new build refused because builds are paused on its
// node.
type BuildsPausedError struct{ Node string }

func (e *BuildsPausedError) Error() string { return "builds paused on " + e.Node }

// SetNodeBuildsPause pauses or resumes new builds on a node and returns the
// node's status afterwards. It never touches the transcode pause or any build.
// Repeating a request changes nothing.
func (e *Engine) SetNodeBuildsPause(ctx context.Context, node string, paused bool) (transcode.NodePauseStatus, error) {
	if e.deps.Store == nil {
		return transcode.NodePauseStatus{}, errors.New("store is required for builds pause")
	}
	w, ok := e.deps.Workers.Get(node)
	if !ok {
		return transcode.NodePauseStatus{}, &transcode.WorkerNotConfiguredError{Name: node}
	}
	// The node's request lock (the one withNodeLock takes) orders this against a create that checks the
	// flag, so a build is either refused or registered before the pause.
	lock := &e.workerStateFor(w.Name).pauseMu
	lock.Lock()
	err := e.deps.Store.SetBuildPause(w.Name, paused)
	lock.Unlock()
	if err != nil {
		return transcode.NodePauseStatus{}, err
	}
	return e.NodePauseStatuses(ctx)[w.Name], nil
}

// buildsPausedOn reports whether new builds on the node are refused. The node's
// request lock is held by the caller of the create path.
func (e *Engine) buildsPausedOn(node string) (bool, error) {
	return e.deps.Store.BuildsPaused(node)
}

// NodeBuildsPaused reports whether new builds on the named node are refused. It
// reads stored state only. An unreadable state reads as paused, as the create
// path fails closed on a read error too.
func (e *Engine) NodeBuildsPaused(name string) bool {
	if e.deps.Store == nil {
		return false
	}
	paused, err := e.deps.Store.BuildsPaused(e.resolveWorker(name))
	return paused || err != nil
}
