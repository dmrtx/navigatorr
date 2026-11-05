package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// A worker that reports its source as damaged has read the file through its
// own copy or network path, and a damaged read is not the same as a damaged
// file: the same bytes can read cleanly from the NAS. A job blamed on the file
// is never offered a retry, so before it is left that way the coordinator reads
// the file itself, packet by packet, with the demuxer and the damage test the
// worker used. Only a clean, complete read of the file the job started with
// overturns the worker; any other outcome (damage found again, a message the
// check cannot rule out, a read that stopped early or failed, a file that is
// not the one the job began with or changed under the scan) leaves the
// worker's verdict standing.

const (
	// sourceRecheckKey holds the record of the coordinator's re-read.
	sourceRecheckKey = "source_recheck"
	// recheckTimeout bounds the re-read: it reads the whole video stream of
	// a file that can be tens of gigabytes over the network.
	recheckTimeout = 20 * time.Minute
	// recheckCoverageSlack is how many seconds short of the video's measured
	// length a complete read may end: the last picture's end is measured, not
	// stated.
	recheckCoverageSlack = 2.0
	// workerScanReport is what the worker's pre-encode scan of the video says.
	// An encode that stops on corrupt input (ffmpeg's own "invalid data")
	// carries the same class without it: a packet scan does not decode, so it
	// cannot clear that.
	workerScanReport = "reading its video the demuxer reported"
)

// recheckSlack is the slack a read of a video of videoSeconds may fall short
// by: never more than half of it, so a video shorter than the slack still has
// to be read for the better part of its length.
func recheckSlack(videoSeconds float64) float64 {
	return math.Min(recheckCoverageSlack, videoSeconds/2)
}

// workerDamageWords are the phrases that make a failure read as the file's
// own damage (SourceDamaged); a verdict that overturns it must not carry them.
var workerDamageWords = regexp.MustCompile(`(?i)ffmpeg_input_corrupt:?|moov atom not found`)

// recheckable is whether a worker's failure is a claim about the file that a
// packet scan can answer: the worker's own scan of the video, or an index the
// worker could not find in its copy of an MP4.
func recheckable(workerMessage string) bool {
	return strings.Contains(workerMessage, workerScanReport) ||
		SourceDamaged(workerMessage) && strings.Contains(workerMessage, "moov atom not found")
}

// recheckDamagedSource re-reads the source of a job the worker failed as
// damaged. It returns the class and message the job should fail with, and
// whether they replace the worker's. A read cut short by the caller's context
// records nothing: the caller sees the context's error and tries again.
func (e *Engine) recheckDamagedSource(ctx context.Context, ec *ExecutionContext, workerMessage string) (class, message string, overturned bool) {
	if !recheckable(workerMessage) {
		return "", "", false
	}
	jobID := getString(ec.State, "job_id")
	// Already read for this failure; a read that could not judge is made again,
	// and so is a clean one of a file that is no longer the one it read.
	switch recheckVerdict(ec.State, jobID) {
	case "intact":
		if coordinatorFoundSourceIntact(ec.State, jobID) {
			record, _ := ec.State[sourceRecheckKey].(map[string]any)
			return string(resilience.WorkerSourceReadFault), intactMessage(record), true
		}
	case "damaged":
		return "", "", false
	}
	record := map[string]any{"job_id": jobID, "worker_report": truncateText(workerMessage, 600), "checked_at": time.Now().UTC().Format(time.RFC3339)}
	defer func() {
		if ctx.Err() == nil {
			ec.State[sourceRecheckKey] = record
		}
	}()
	unchecked := func(format string, args ...any) (string, string, bool) {
		record["verdict"], record["detail"] = "unchecked", fmt.Sprintf(format, args...)
		return "", "", false
	}

	target, why := recheckTargetOf(ec)
	if why != "" {
		return unchecked("%s", why)
	}
	record["path"] = target.path
	before, err := os.Stat(target.path)
	if err != nil {
		return unchecked("the file could not be inspected: %v", err)
	}
	if !target.sameFile(before) {
		record["file_changed"] = true
		return unchecked("the file is not the one the job started with (it is %d bytes, modified %s)", before.Size(), before.ModTime().UTC().Format(time.RFC3339))
	}
	release, err := e.acquireHashSlot(ctx)
	if err != nil {
		return unchecked("no read slot was free: %v", err)
	}
	defer release()
	scanCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	damage, err := mediainspect.ScanDemuxDamage(scanCtx, e.deps.Ffprobe, target.path, target.videoIndex)
	if err != nil {
		return unchecked("the coordinator's read failed: %v", err)
	}
	// A file recorded by size alone is told apart from a rewrite of the same
	// length by what the read itself saw: its size and time, before and after.
	after, err := os.Stat(target.path)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !target.sameFile(after) {
		return unchecked("the file changed while it was read")
	}
	record["packets"], record["demuxer_errors"], record["seconds_read"], record["video_seconds"] = damage.Packets, damage.Errors, damage.Seconds, target.videoSeconds
	switch {
	// A run that had a failed read stopped where the read did: what it printed
	// before, damage included, may be the failure's own aftermath.
	case damage.ReadFaults > 0:
		return unchecked("the demuxer's read of the file failed %d time(s), so the read stopped early and judged nothing; the first message this check could not rule out: %s", damage.ReadFaults, damage.FirstUnrecognised)
	case damage.Errors > 0:
		record["verdict"], record["detail"] = "damaged", damage.First
		return "", "", false
	case damage.Unrecognised > 0:
		return unchecked("the demuxer printed %d message(s) this check cannot rule out as damage, the first: %s", damage.Unrecognised, damage.FirstUnrecognised)
	case damage.Packets == 0:
		return unchecked("the stream has no packets to read")
	case damage.Discontinuous:
		return unchecked("the stream's timestamps jump back, so the seconds read cannot be compared with the video's length")
	case damage.Seconds <= 0:
		return unchecked("the packets carried no timestamps, so how much of the video was read cannot be measured")
	case damage.Seconds < target.videoSeconds-recheckSlack(target.videoSeconds):
		return unchecked("the read covered %.0f of the video's %.0f seconds", damage.Seconds, target.videoSeconds)
	}
	// Cut short after the last packet: the verdict would not be stored, so the
	// caller must not act on it either.
	if ctx.Err() != nil {
		return "", "", false
	}
	// The verdict is about this file as the read saw it.
	record["verdict"], record["file_size"], record["file_mod"] = "intact", before.Size(), before.ModTime().UTC().Format(time.RFC3339Nano)
	return string(resilience.WorkerSourceReadFault), intactMessage(record), true
}

// recheckVerdict is the verdict of the coordinator's re-read of this job's
// source, "" when it has none (a record of another attempt does not count).
func recheckVerdict(state map[string]any, jobID string) string {
	record, _ := state[sourceRecheckKey].(map[string]any)
	if jobID == "" || getString(record, "job_id") != jobID {
		return ""
	}
	return getString(record, "verdict")
}

// coordinatorFoundSourceIntact is whether the coordinator read this job's
// source whole and clean after the worker called it damaged, so the worker's
// class is not the file's, and the file is still the one it read: a verdict
// kept across a failed retry or a resubmission says nothing about a file that
// changed or went away since.
func coordinatorFoundSourceIntact(state map[string]any, jobID string) bool {
	if recheckVerdict(state, jobID) != "intact" {
		return false
	}
	target, why := recheckTargetOf(&ExecutionContext{State: state})
	if why != "" {
		return false
	}
	fi, err := os.Stat(target.path)
	if err != nil || !target.sameFile(fi) {
		return false
	}
	// The file the read saw, not only the file the job started with.
	record, _ := state[sourceRecheckKey].(map[string]any)
	mod, err := time.Parse(time.RFC3339Nano, getString(record, "file_mod"))
	return err == nil && fi.Size() == getInt64(record, "file_size") && fi.ModTime().Equal(mod)
}

// verdictFileChanged is whether this job's file was read clean and is no longer
// the file that was read: the job is blamed on neither the worker nor the file
// but on a file that has since changed.
func verdictFileChanged(state map[string]any, jobID string) bool {
	if recheckVerdict(state, jobID) != "intact" || coordinatorFoundSourceIntact(state, jobID) {
		return false
	}
	// A file that cannot be reached now (a share that dropped for a moment) has
	// not been shown to differ from the one that was read.
	target, why := recheckTargetOf(&ExecutionContext{State: state})
	if why != "" {
		return false
	}
	_, err := os.Stat(target.path)
	return err == nil || errors.Is(err, os.ErrNotExist)
}

// storedFailureText is the message inside a failed job's stored error.
func storedFailureText(errorJSON string) string {
	text := strings.TrimSpace(errorJSON)
	var stored struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(text), &stored) == nil && stored.Error != "" {
		return stored.Error
	}
	return text
}

// CanRecheckSource is whether a failed job that blames its file is still worth
// a Retry: the blame is the worker's scan of the video, which the coordinator
// has not found damaged itself, and Retry reads the file again before it
// decides. A job the coordinator already read as damaged is not, nor is one
// that recorded nothing to tell its file by (a preflight that never read it).
func CanRecheckSource(errorJSON string, state map[string]any) bool {
	// The failure must still read as the file's damage: one the coordinator
	// already overturned keeps the worker's words but not its class.
	text := storedFailureText(errorJSON)
	if !SourceDamaged(text) || !recheckable(text) || recheckVerdict(state, getString(state, "job_id")) == "damaged" {
		return false
	}
	_, why := recheckTargetOf(&ExecutionContext{State: state})
	return why == ""
}

// damagedFileRefusal is why Retry will not run a file that is damaged.
const damagedFileRefusal = "this file is damaged, so running it again fails the same way; replace the file, then choose a goal for it"

// recheckOnRetry decides a Retry of a job blamed on its file. A worker's scan
// report is checked by reading the file here, now: only a whole, clean read
// lets the retry go on, with the record kept for the retry to follow. Any other
// outcome refuses it, with what the read found. The record is kept in ec and in
// the stored job, so the retry continues from it and a refused one is not read
// again.
func (e *Engine) recheckOnRetry(ctx context.Context, inst *store.ActionInstance, ec *ExecutionContext) error {
	message := storedFailureText(inst.ErrorJSON)
	if !recheckable(message) {
		return errors.New(damagedFileRefusal)
	}
	// A verdict kept for this failure is reused as it is; anything else is a
	// new read, and its record is stored.
	jobID := getString(ec.State, "job_id")
	reused := recheckVerdict(ec.State, jobID) == "damaged" || coordinatorFoundSourceIntact(ec.State, jobID)
	class, text, overturned := e.recheckDamagedSource(ctx, ec, message)
	if err := ctx.Err(); err != nil && !overturned {
		return err
	}
	if !reused {
		if overturned {
			appendFailureHistory(ec, "recheck", class, text)
		}
		inst.StateJSON = toJSON(ec.State)
		if err := e.updateInstance(ctx, inst); err != nil {
			return err
		}
	}
	if overturned {
		return nil
	}
	record, _ := ec.State[sourceRecheckKey].(map[string]any)
	if getString(record, "verdict") == "damaged" {
		return fmt.Errorf("%s (Navigatorr read it again and found the damage too: %s)", damagedFileRefusal, getString(record, "detail"))
	}
	// The file the worker read is gone: what it said is no claim about this one.
	if getBool(record, "file_changed") {
		return fmt.Errorf("the file changed after this job started, so the worker's report says nothing about the file now there; choose a goal for it again (%s)", getString(record, "detail"))
	}
	return fmt.Errorf("%s (Navigatorr read it again to check the worker's report but could not clear the file: %s)", damagedFileRefusal, getString(record, "detail"))
}

// intactMessage is the failure of a job whose file the coordinator read
// cleanly. It keeps what the worker said, minus the words that would make the
// job read as a damaged file again.
func intactMessage(record map[string]any) string {
	report := workerDamageWords.ReplaceAllString(getString(record, "worker_report"), "[worker read error]")
	return fmt.Sprintf("the worker reported the source file as damaged, but the coordinator read the same file end to end (%d video packets, %.0f of %.0f seconds) without a single demuxer error and the file did not change, so the file is intact and the worker's own read of it was at fault. Running it again is safe. The worker said: %s",
		getInt(record, "packets"), getFloat(record, "seconds_read"), getFloat(record, "video_seconds"), report)
}

// recheckTarget is the file the job started with, as the coordinator can tell
// it from another, and the video the worker scanned.
type recheckTarget struct {
	path         string
	videoIndex   int
	videoSeconds float64
	size         int64
	// mod is the modification time the job recorded; zero when only the size
	// was recorded.
	mod time.Time
}

// sameFile is whether fi is the file the job recorded.
func (t recheckTarget) sameFile(fi os.FileInfo) bool {
	return fi.Size() == t.size && (t.mod.IsZero() || fi.ModTime().Equal(t.mod))
}

// recheckTargetOf reads the job's record of its original. why says what is
// missing when the file cannot be told from another one.
func recheckTargetOf(ec *ExecutionContext) (t recheckTarget, why string) {
	var rep mediainspect.DetailedReport
	if b, err := json.Marshal(ec.State["original"]); err == nil {
		_ = json.Unmarshal(b, &rep)
	}
	t.path = getString(ec.State, "resolved_path")
	if t.path == "" {
		t.path = rep.Path
	}
	if t.path == "" {
		return t, "the job recorded no path for the file"
	}
	// The programme video, not a cover picture before it.
	video := rep.MainVideo()
	if len(video) == 0 {
		return t, "the job recorded no programme video to read"
	}
	t.videoIndex, t.videoSeconds = video[0].Index, rep.VideoSeconds
	if t.videoSeconds <= 0 {
		return t, "the original's video length was not measured, so a read that stopped early cannot be told from a whole one"
	}
	var ok bool
	if t.size, t.mod, ok = recordedOriginalStat(ec.State); !ok {
		t.size = rep.SizeBytes
	}
	if t.size <= 0 {
		return t, "the job recorded no size for the file, so it cannot be told from another"
	}
	return t, ""
}

func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
