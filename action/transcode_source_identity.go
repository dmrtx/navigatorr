package action

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// Source identity without a coordinator hash.
//
// Reading a whole original over the network only to hash it is not free, and
// the worker reads it anyway when it copies the file to its disk, hashing the
// bytes as they stream. So for a worker that advertises LearnSourceDigest the
// preflight records only what a stat gives (size and modification time, under
// originalStatKey) and submits with no digest. The worker reports the digest it
// computed while staging in the job status; the first status that carries it
// is pinned as original_sha256 and never changes. Rungs after the first send
// the pinned digest, and accept_result checks the original's size and mtime
// against the recorded stat instead of reading it again.
//
// An action that holds original_sha256 and no originalStatKey was admitted
// before this flow (or ran against a worker that does not advertise it): it
// keeps the coordinator hash and the full re-hash at accept.

// originalStatKey is the state key holding the original's stat at preflight.
const originalStatKey = "original_stat"

// originalDigestConflictKey records a worker digest that disagrees with the
// pinned one. It is an integrity failure: the media is not what it was.
const originalDigestConflictKey = "original_sha256_conflict"

// originalIntegrityCheckKey says how original_integrity was established:
// "metadata" (size and mtime against the preflight stat) or "sha256" (a full
// re-read). Absent on actions that predate it, which always re-read.
const originalIntegrityCheckKey = "original_integrity_check"

// recordOriginalStat stores the size and modification time the preflight saw.
// The time is a string so its nanoseconds survive a JSON round trip.
func recordOriginalStat(state map[string]any, fi os.FileInfo) {
	state[originalStatKey] = map[string]any{"size_bytes": fi.Size(), "mod_time": fi.ModTime().UTC().Format(time.RFC3339Nano)}
}

// recordedOriginalStat reads originalStatKey back.
func recordedOriginalStat(state map[string]any) (size int64, mod time.Time, ok bool) {
	m, _ := state[originalStatKey].(map[string]any)
	if m == nil {
		return 0, time.Time{}, false
	}
	mod, err := time.Parse(time.RFC3339Nano, getString(m, "mod_time"))
	if err != nil {
		return 0, time.Time{}, false
	}
	return getInt64(m, "size_bytes"), mod, true
}

// hasOriginalBaseline reports whether work may be submitted: a digest from the
// coordinator's hash, or a recorded stat that the worker's digest will complete.
func hasOriginalBaseline(state map[string]any) bool {
	if getString(state, "original_sha256") != "" {
		return true
	}
	_, _, ok := recordedOriginalStat(state)
	return ok
}

func workerLearnsSourceDigest(ctx context.Context, worker transcode.Executor) bool {
	caps, err := worker.Capabilities(ctx)
	return err == nil && caps.LearnSourceDigest
}

// pinOriginalSHA records the digest a worker status reported. The first one is
// pinned into original_sha256 and original.sha256; it never changes, and a
// different digest later (or one that is not a digest) is an integrity failure
// left for the next step to act on.
func pinOriginalSHA(ec *ExecutionContext, raw string) {
	if raw == "" {
		return
	}
	sha, err := transcode.NormalizeSourceSHA256(raw)
	if err != nil || sha == "" {
		ec.State[originalDigestConflictKey] = fmt.Sprintf("worker reported an unusable source digest %q", raw)
		return
	}
	pinned := getString(ec.State, "original_sha256")
	if pinned == "" {
		ec.State["original_sha256"] = sha
		ec.Outputs["original_sha256"] = sha
		if orig, ok := ec.State["original"].(map[string]any); ok {
			orig["sha256"] = sha
		}
		return
	}
	if pinned != sha {
		ec.State[originalDigestConflictKey] = fmt.Sprintf("worker reported source sha256 %s, pinned %s", sha, pinned)
	}
}

// originalDigestConflict is the failure for a recorded digest conflict.
func originalDigestConflict(ec *ExecutionContext) (StepResult, bool) {
	msg := getString(ec.State, originalDigestConflictKey)
	if msg == "" {
		return StepResult{}, false
	}
	ec.State["failure_classification"] = string(resilience.SourceChanged)
	return StepResult{Status: StepFailed, Error: "integrity violation: " + msg + " (fail closed)"}, true
}

// verifyOriginalMetadata compares the original's size and mtime with the stat
// recorded at preflight. It reads no media.
func verifyOriginalMetadata(ec *ExecutionContext, origPath string) *StepResult {
	size, mod, ok := recordedOriginalStat(ec.State)
	if !ok {
		return &StepResult{Status: StepFailed, Error: "original integrity baseline missing (fail closed)"}
	}
	fi, err := os.Stat(origPath)
	if err != nil {
		return &StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: original media %s is no longer accessible: %v", origPath, err)}
	}
	if fi.Size() != size || !fi.ModTime().Equal(mod) {
		ec.State["failure_classification"] = string(resilience.SourceChanged)
		return &StepResult{Status: StepFailed, Error: fmt.Sprintf("integrity violation: original file %s changed since it was inspected (size %d -> %d, modified %s -> %s)", origPath, size, fi.Size(), mod.UTC().Format(time.RFC3339Nano), fi.ModTime().UTC().Format(time.RFC3339Nano))}
	}
	return nil
}
