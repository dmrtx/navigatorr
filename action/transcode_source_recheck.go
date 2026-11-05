package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// A worker that reports its source as damaged has read the file through its
// own copy or network path, and a damaged read is not the same as a damaged
// file: the same bytes can read cleanly from the NAS. A job blamed on the file
// is never offered a retry, so before it is left that way the coordinator reads
// the file itself, packet by packet, with the demuxer and the damage test the
// worker used. Only a clean read of an unchanged file overturns the worker; any
// other outcome (damage found again, the read failing, the file changing under
// the scan) leaves the worker's verdict standing.

const (
	// sourceRecheckKey holds the record of the coordinator's re-read.
	sourceRecheckKey = "source_recheck"
	// recheckTimeout bounds the re-read: it reads the whole video stream of
	// a file that can be tens of gigabytes over the network.
	recheckTimeout = 20 * time.Minute
)

// workerDamageWords are the phrases that make a failure read as the file's
// own damage (SourceDamaged); a verdict that overturns it must not carry them.
var workerDamageWords = regexp.MustCompile(`(?i)ffmpeg_input_corrupt:?|moov atom not found`)

// recheckDamagedSource re-reads the source of a job the worker failed as
// damaged. It returns the class and message the job should fail with, and
// whether they replace the worker's.
func (e *Engine) recheckDamagedSource(ctx context.Context, ec *ExecutionContext, workerMessage string) (class, message string, overturned bool) {
	if !SourceDamaged(workerMessage) {
		return "", "", false
	}
	record := stateObject(ec.State, sourceRecheckKey)
	if verdict := getString(record, "verdict"); verdict != "" {
		// Already read for this failure.
		if verdict == "intact" {
			return string(resilience.WorkerSourceReadFault), intactMessage(record), true
		}
		return "", "", false
	}
	path, videoIndex := recheckTarget(ec)
	if path == "" {
		return "", "", false
	}
	record = map[string]any{"path": path, "worker_report": truncateText(workerMessage, 600), "checked_at": time.Now().UTC().Format(time.RFC3339)}
	defer func() { ec.State[sourceRecheckKey] = record }()

	before, err := os.Stat(path)
	if err != nil {
		record["verdict"], record["detail"] = "unchecked", fmt.Sprintf("the file could not be inspected: %v", err)
		return "", "", false
	}
	scanCtx, cancel := context.WithTimeout(ctx, recheckTimeout)
	defer cancel()
	damage, err := mediainspect.ScanDemuxDamage(scanCtx, e.deps.Ffprobe, path, videoIndex)
	if err != nil {
		record["verdict"], record["detail"] = "unchecked", fmt.Sprintf("the coordinator's read failed: %v", err)
		return "", "", false
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		record["verdict"], record["detail"] = "unchecked", "the file changed while it was read"
		return "", "", false
	}
	record["packets"], record["demuxer_errors"] = damage.Packets, damage.Errors
	switch {
	case damage.Errors > 0:
		record["verdict"], record["detail"] = "damaged", damage.First
		return "", "", false
	case damage.Packets == 0:
		record["verdict"], record["detail"] = "unchecked", "the stream has no packets to read"
		return "", "", false
	}
	record["verdict"] = "intact"
	return string(resilience.WorkerSourceReadFault), intactMessage(record), true
}

// intactMessage is the failure of a job whose file the coordinator read
// cleanly. It keeps what the worker said, minus the words that would make the
// job read as a damaged file again.
func intactMessage(record map[string]any) string {
	report := workerDamageWords.ReplaceAllString(getString(record, "worker_report"), "[worker read error]")
	return fmt.Sprintf("the worker reported the source file as damaged, but the coordinator read the same file end to end (%d video packets) without a single demuxer error and the file did not change, so the file is intact and the worker's own read of it was at fault. Running it again is safe. The worker said: %s",
		getInt(record, "packets"), report)
}

// recheckTarget is the original's path as the coordinator sees it and the
// index of the video the worker scanned.
func recheckTarget(ec *ExecutionContext) (path string, videoIndex int) {
	path = getString(ec.State, "resolved_path")
	var original struct {
		Path  string `json:"path"`
		Video []struct {
			Index int `json:"index"`
		} `json:"video"`
	}
	if b, err := json.Marshal(ec.State["original"]); err == nil {
		_ = json.Unmarshal(b, &original)
	}
	if path == "" {
		path = original.Path
	}
	if len(original.Video) > 0 {
		videoIndex = original.Video[0].Index
	}
	return path, videoIndex
}

func truncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
