package action

import (
	"os"
	"strings"
)

// SubmittedSourceBytesKey is the state key holding the size of a
// transcode_media job's file when the job was submitted. The preflight's own
// record of the original (state "original", size_bytes) replaces it as the
// file's size once it exists; until then the job lists can show how big the
// file is while it waits in the queue. It is a display figure only: nothing
// compares the original against it.
const SubmittedSourceBytesKey = "submitted_source_bytes"

// submittedSourceBytes stats a transcode_media job's file when it is
// submitted. The request was already judged, so its path is inside the read
// roots; the stat is one metadata call, the same class of work the path check
// itself does. Anything else, or a file it cannot stat, gives 0 and the job is
// listed without a size until its preflight records one.
func (e *Engine) submittedSourceBytes(name string, inputs map[string]any) int64 {
	if name != "transcode_media" || e.deps.Fs == nil {
		return 0
	}
	path, err := e.deps.Fs.ResolveRead(strings.TrimSpace(getString(inputs, "path")))
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}
