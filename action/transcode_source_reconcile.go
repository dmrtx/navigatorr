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
	CompletedAt    time.Time
}

// sameLibraryFile reports whether two paths are the same file apart from the
// extension (a replacement may change it) and its letter case (the shares are
// case-insensitive).
func sameLibraryFile(a, b string) bool {
	if filepath.Dir(a) != filepath.Dir(b) && !strings.EqualFold(filepath.Dir(a), filepath.Dir(b)) {
		return false
	}
	stem := func(p string) string {
		base := filepath.Base(p)
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	return strings.EqualFold(stem(a), stem(b))
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
	rows, err := e.deps.Store.ListActionInstancesMentioning("promote_transcode_candidate", needle, []string{store.ActionStatusCompleted}, nil)
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
		if p.NewPath == "" || !sameLibraryFile(p.OriginalPath, path) {
			continue
		}
		done := parseStoreTime(row.UpdatedAt)
		if !done.After(admitted) {
			// Finished before this job existed: the job was admitted against
			// the file as the promotion left it.
			continue
		}
		rec := &replacementRecord{PromotionID: row.ID, SourceActionID: p.SourceActionID, OriginalPath: p.OriginalPath, NewPath: p.NewPath, CompletedAt: done}
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
	if _, err := os.Stat(rec.NewPath); err != nil {
		// The replacement the history names is not there either: nothing
		// proves the work is done, so the caller reports the plain failure.
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

// replacedSincePicked reports a promotion that replaced path after the job was
// queued, for a job that carries explicit stream picks: indexes that still
// exist can silently point at other tracks in a replaced file.
func (e *Engine) replacedSincePicked(ctx context.Context, ec *ExecutionContext, path string) (StepResult, bool) {
	if ctx.Err() != nil {
		return StepResult{}, false
	}
	if ec.Inputs[inputKeepAudioStreams] == nil && ec.Inputs[inputKeepSubtitleStreams] == nil {
		return StepResult{}, false
	}
	rec, _ := e.replacementSince(path, ec.InstanceID)
	return e.skipReplacedSource(ec, rec, "the audio/subtitle streams chosen for this job belong to the file before it was replaced")
}
