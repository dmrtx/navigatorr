package action

import (
	"os"
	"strings"
	"time"
)

// SubmittedSourceBytesKey is the state key holding the size of a
// transcode_media job's file when the job was submitted. The preflight's own
// record of the original (state "original", size_bytes) replaces it as the
// file's size once it exists; until then the job lists can show how big the
// file is while it waits in the queue. It is a display figure only: nothing
// compares the original against it.
const SubmittedSourceBytesKey = "submitted_source_bytes"

// submittedStatTimeout bounds the submission stat: a stalled network mount
// leaves the job without a size, never holds up its admission.
var submittedStatTimeout = 2 * time.Second

// submittedStat is os.Stat; tests stand in a stat that never answers.
var submittedStat = os.Stat

// submittedSourceBytes stats a transcode_media job's file when a new job is
// created (callers reach it only after their idempotency lookups). The
// request was already judged, so its path is inside the read roots; the stat
// is one metadata call. Anything else, a file it cannot stat, or a stat that
// does not answer within submittedStatTimeout gives 0, and the job is listed
// without a size until its preflight records one.
func (e *Engine) submittedSourceBytes(name string, inputs map[string]any) int64 {
	if name != "transcode_media" {
		return 0
	}
	return e.boundedFileSize(getString(inputs, "path"))
}

// boundedFileSize is the size of the regular file at raw (a path inside the
// read roots), or 0 when it cannot say: no resolver, a file it cannot stat,
// or a stat that does not answer within submittedStatTimeout.
func (e *Engine) boundedFileSize(raw string) int64 {
	if e.deps.Fs == nil {
		return 0
	}
	raw = strings.TrimSpace(raw)
	// Buffered: a stat that answers after the timeout must not leak a goroutine
	// blocked on the send.
	done := make(chan int64, 1)
	go func() {
		path, err := e.deps.Fs.ResolveRead(raw)
		if err != nil {
			done <- 0
			return
		}
		fi, err := submittedStat(path)
		if err != nil || !fi.Mode().IsRegular() {
			done <- 0
			return
		}
		done <- fi.Size()
	}()
	timer := time.NewTimer(submittedStatTimeout)
	defer timer.Stop()
	select {
	case n := <-done:
		return n
	case <-timer.C:
		return 0
	}
}
