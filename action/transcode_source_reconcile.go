package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode/resilience"
)

// A conversion waits in the queue for hours or days. In that time the file it
// was admitted for can be replaced by an earlier conversion of the same file
// (a different extension included), moved, or deleted. A job whose source is
// gone because another action already did the work is not a failure: nothing
// is left to do for it. A job whose source is gone for any other reason, or
// whose frozen stream picks no longer fit the file, fails with a class that
// says so, instead of a message that reads like an encode problem.
//
// The evidence for "already done" is the library's own history: a completed
// promote_transcode_candidate whose original is this file (same folder and
// name, extension aside), finished after this job was admitted, and whose
// published file is still there. Nothing is guessed from the path alone.

const (
	// reasonSourceReplaced is the skip reason of a job whose file was replaced
	// by a completed promotion after the job was queued.
	reasonSourceReplaced = "source_replaced"
	// Outputs recording what replaced the file; the job itself keeps its id,
	// idempotency key and history.
	supersededByKey    = "superseded_by_action"
	replacementPathKey = "replacement_path"
)

// replacementRecord is a completed promotion that replaced a queued job's file.
type replacementRecord struct {
	PromotionID    string
	SourceActionID string
	OriginalPath   string
	NewPath        string
	NewBytes       int64
	CompletedAt    time.Time
}

// replacementSince finds the completed promotion that replaced path after the
// job was admitted. It returns nil when there is none, and an error only when
// the history could not be read (callers then keep the original failure).
func (e *Engine) replacementSince(path, instanceID string) (*replacementRecord, error) {
	if e.deps.Store == nil {
		return nil, nil
	}
	self, err := e.deps.Store.GetActionInstanceIfExists(instanceID)
	if err != nil || self == nil {
		return nil, err
	}
	admitted := parseStoreTime(self.CreatedAt)
	if admitted.IsZero() {
		return nil, nil
	}
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	encoded, err := json.Marshal(stem)
	if err != nil || len(encoded) < 3 {
		return nil, err
	}
	// The stem as it appears inside the stored JSON string.
	needle := string(encoded[1 : len(encoded)-1])
	rows, err := e.deps.Store.ListActionInstancesMentioningFold("promote_transcode_candidate", needle, []string{store.ActionStatusCompleted}, nil)
	if err != nil {
		return nil, err
	}
	var best *replacementRecord
	for _, row := range rows {
		var state struct {
			Promotion promotionState `json:"promotion"`
		}
		if json.Unmarshal([]byte(row.StateJSON), &state) != nil {
			continue
		}
		p := state.Promotion
		// The promotion's original must be this very file (letter case aside:
		// the shares ignore it). A sibling with the same name and another
		// extension is a different file, whatever replaced it.
		if p.NewPath == "" || !strings.EqualFold(p.OriginalPath, path) {
			continue
		}
		// The moment the replacement became final. Promotions from before
		// published_at existed only have the row's updated_at, stored to the
		// second and moved by any later save of the completed promotion; it
		// can only make an old promotion look newer, which the published-size
		// check and the exact-path match then still have to confirm.
		done, slack := parseStoreTime(p.PublishedAt), time.Duration(0)
		if done.IsZero() {
			// Stored to the second while admission is stored to the
			// nanosecond: a promotion that finished in the second of admission
			// may be on either side, so it counts as after.
			done, slack = parseStoreTime(row.UpdatedAt), time.Second
		}
		if !done.Add(slack).After(admitted) {
			// Finished before this job existed: the job was admitted against
			// the file as the promotion left it.
			continue
		}
		rec := &replacementRecord{PromotionID: row.ID, SourceActionID: p.SourceActionID, OriginalPath: p.OriginalPath, NewPath: p.NewPath, NewBytes: p.CandidateBytes, CompletedAt: done}
		if best == nil || rec.CompletedAt.After(best.CompletedAt) {
			best = rec
		}
	}
	return best, nil
}

// skipReplacedSource ends the preflight of a job whose file another action has
// already replaced. It is a completed, skipped job, not a failure: the work was
// done and the history keeps both actions.
func (e *Engine) skipReplacedSource(ec *ExecutionContext, rec *replacementRecord, why string) (StepResult, bool) {
	if rec == nil {
		return StepResult{}, false
	}
	fi, err := os.Stat(rec.NewPath)
	if err != nil || fi.IsDir() || (rec.NewBytes > 0 && fi.Size() != rec.NewBytes) {
		// The replacement the history names is not there, or is not the file
		// the promotion published (it was changed or substituted since):
		// nothing proves the work is done, so the caller reports the plain
		// outcome.
		return StepResult{}, false
	}
	ec.State["skip_transcode"] = true
	ec.State["reason_code"] = reasonSourceReplaced
	ec.State["resolved_path"] = rec.NewPath
	ec.State[supersededByKey] = rec.PromotionID
	ec.State[replacementPathKey] = rec.NewPath
	return StepResult{Status: StepCompleted, Outputs: map[string]any{
		"skipped":          true,
		"reason_code":      reasonSourceReplaced,
		"resolved_path":    rec.NewPath,
		supersededByKey:    rec.PromotionID,
		replacementPathKey: rec.NewPath,
		"message":          fmt.Sprintf("Nothing to do: %s. Promotion %s already replaced this file (source conversion %s) and the replacement is in place.", why, rec.PromotionID, rec.SourceActionID),
	}}, true
}

// missingSourceResult is the outcome of a queued job whose file cannot be
// statted. A file that is gone is told apart from one that cannot be inspected
// now, and a replacement by an earlier conversion is not an error at all.
func (e *Engine) missingSourceResult(ec *ExecutionContext, path string, statErr error) StepResult {
	if os.IsNotExist(statErr) {
		rec, _ := e.replacementSince(path, ec.InstanceID)
		if res, ok := e.skipReplacedSource(ec, rec, "the original no longer exists"); ok {
			return res
		}
		ec.State["failure_classification"] = string(resilience.SourceUnreachable)
		return StepResult{Status: StepFailed, Error: fmt.Sprintf("the original %q no longer exists, and no completed replacement of it was found; it was moved, renamed or deleted since this job was queued. Nothing was changed: queue a new conversion for the file where it is now", path)}
	}
	ec.State["failure_classification"] = string(resilience.SourceUnreachable)
	return StepResult{Status: StepFailed, Error: fmt.Sprintf("file %q not accessible: %v", path, statErr)}
}

// staleTrackPicksResult is the outcome of a job whose frozen keep_audio_streams
// or keep_subtitle_streams name streams that are not in the file. The picks were
// made for the file as it was when queued, so a file that was replaced since
// ends the job as done; otherwise the file changed in a way nothing explains,
// which is reported as a changed source and never retried with the old picks.
func (e *Engine) staleTrackPicksResult(ec *ExecutionContext, path string, picksErr error) StepResult {
	rec, _ := e.replacementSince(path, ec.InstanceID)
	if res, ok := e.skipReplacedSource(ec, rec, "the audio/subtitle streams chosen for this job belong to the file before it was replaced"); ok {
		return res
	}
	ec.State["failure_classification"] = string(resilience.SourceChanged)
	return StepResult{Status: StepFailed, Error: fmt.Sprintf("choosing the tracks to keep: %v; the streams were picked for the file as it was when this job was queued and the file has changed since. They were not applied to the new file: choose the tracks again", picksErr)}
}

// replacedSince reports a job whose file a completed promotion replaced after
// the job was queued, even when the file is still there under the same name:
// converting it again would re-encode a file that is already the result. A job
// with explicit stream picks is also stopped when the file was modified after
// it was queued without any promotion explaining it: indexes that still exist
// can point at other tracks of a different file.
func (e *Engine) replacedSince(ctx context.Context, ec *ExecutionContext, path string, fi os.FileInfo) (StepResult, bool) {
	if ctx.Err() != nil {
		return StepResult{}, false
	}
	rec, _ := e.replacementSince(path, ec.InstanceID)
	if res, ok := e.skipReplacedSource(ec, rec, "another conversion replaced this file after this job was queued"); ok {
		return res, true
	}
	if ec.Inputs[inputKeepAudioStreams] == nil && ec.Inputs[inputKeepSubtitleStreams] == nil {
		return StepResult{}, false
	}
	if e.deps.Store == nil {
		return StepResult{}, false
	}
	if self, err := e.deps.Store.GetActionInstanceIfExists(ec.InstanceID); err == nil && self != nil {
		if admitted := parseStoreTime(self.CreatedAt); !admitted.IsZero() && fi.ModTime().After(admitted.Add(picksModifiedSlack)) {
			ec.State["failure_classification"] = string(resilience.SourceChanged)
			return StepResult{Status: StepFailed, Error: fmt.Sprintf("choosing the tracks to keep: the file was modified at %s, after this job was queued at %s, and the streams were picked for the file as it was then. They were not applied to the new file: choose the tracks again", fi.ModTime().UTC().Format(time.RFC3339), admitted.UTC().Format(time.RFC3339))}, true
		}
	}
	return StepResult{}, false
}

// picksModifiedSlack absorbs the clock difference between the NAS and the
// coordinator; a change that only shows after it is a real one.
const picksModifiedSlack = 5 * time.Minute
