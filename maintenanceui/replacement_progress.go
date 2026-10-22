package maintenanceui

import (
	"path/filepath"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

// A batch replacement is one approved plan whose members are replaced a few at
// a time. These helpers say, per member, where it is in that sequence, in a
// small fixed vocabulary the page can phrase for a person, and why a failed
// one failed. Nothing here changes a plan or an approval.

// Phases of one file's replacement, in order. The page names each.
const (
	phaseWaiting     = "waiting"      // approved, not started
	phaseChecking    = "checking"     // verifying the original and the new file
	phaseBackup      = "backup"       // copying the original aside
	phaseBackupCheck = "backup_check" // verifying that copy
	phaseSwap        = "swap"         // putting the new file in place
	phaseFinalCheck  = "final_check"  // checking the new file in the library
	phaseDone        = "done"
	phaseFailed      = "failed"
)

type planMember struct {
	ItemKey, Episode, SourceAction, OriginalPath, CandidatePath string
}

// batchPlanState reads the prepared replacement plan out of a batch's state:
// its members in order and whether it was approved. No plan means no members.
func batchPlanState(state map[string]any) (members []planMember, approved bool) {
	plan := operationMap(state["batch_promotion_plan"])
	if plan == nil {
		return nil, false
	}
	raw, _ := plan["members"].([]any)
	for _, entry := range raw {
		m := operationMap(entry)
		members = append(members, planMember{operationString(m["item_key"]), operationString(m["episode"]), operationString(m["transcode_action_id"]), operationString(m["original_path"]), operationString(m["candidate_path"])})
	}
	return members, state["batch_promotion_approved"] == true
}

// replacementPlanState says what a prepared plan means for one candidate that
// has no replacement job yet: "review" while the plan waits for approval,
// "queued" once it is approved and the file has not started, "" otherwise.
func replacementPlanState(batchState map[string]any, transcodeActionID string) string {
	members, approved := batchPlanState(batchState)
	for _, m := range members {
		if m.SourceAction == transcodeActionID {
			if approved {
				return "queued"
			}
			return "review"
		}
	}
	return ""
}

func memberName(m planMember) string {
	if m.Episode != "" {
		return m.Episode
	}
	return strings.TrimSuffix(filepath.Base(m.OriginalPath), filepath.Ext(m.OriginalPath))
}

// replacementPhase places one replacement job in the sequence. The
// coordinator's own whole-file reads name the backup stages; otherwise the
// workflow step does.
func (s *Server) replacementPhase(r operationRecord) (phase string, percent *int, read, total int64) {
	switch r.inst.Status {
	case store.ActionStatusCompleted:
		return phaseDone, nil, 0, 0
	case store.ActionStatusFailed, store.ActionStatusCancelled:
		return phaseFailed, nil, 0, 0
	case store.ActionStatusPending:
		return phaseWaiting, nil, 0, 0
	}
	work := operationMap(r.state["work_progress"])
	switch operationString(work["phase"]) {
	case "copying_recovery":
		phase = phaseBackup
	case "verifying_recovery":
		phase = phaseBackupCheck
	}
	if phase != "" {
		read, total = int64(operationNumber(work["bytes_read"])), int64(operationNumber(work["total_bytes"]))
		if total > 0 {
			p := int(float64(min(read, total)) / float64(total) * 100)
			percent = &p
		}
		return phase, percent, read, total
	}
	step := ""
	if tmpl, ok := s.engine.GetTemplate(r.inst.ActionName); ok && r.inst.CurrentStep >= 0 && r.inst.CurrentStep < len(tmpl.Steps) {
		step = tmpl.Steps[r.inst.CurrentStep].Name
	}
	switch step {
	case "import_candidate", "remove_old_file", "rename_candidate", "rescan_library":
		return phaseSwap, nil, 0, 0
	case "finalize_promotion":
		return phaseFinalCheck, nil, 0, 0
	}
	return phaseChecking, nil, 0, 0
}

// plainReplacementReason says why a replacement stopped, as a phrase that
// follows "because" or "failed:" and starts without an article. The server's
// own text stays available as the detail.
func plainReplacementReason(detail string) string {
	text := strings.ToLower(detail)
	switch {
	case strings.Contains(text, "audio bitrate mismatch"):
		return "final check before replacing found an audio mismatch"
	case strings.Contains(text, "promotion validation"):
		return "final check before replacing did not pass"
	case strings.Contains(text, "no space") || strings.Contains(text, "disk full"):
		return "there was not enough free space for the backup copy"
	case strings.Contains(text, "sha-256") || strings.Contains(text, "sha256") || strings.Contains(text, "digest") || strings.Contains(text, "changed"):
		return "a file changed while it was being replaced"
	case strings.Contains(text, "sonarr") || strings.Contains(text, "radarr") || strings.Contains(text, "episodefile") || strings.Contains(text, "rescan") || strings.Contains(text, "import"):
		return "library did not accept the new file"
	}
	return "replacement did not finish"
}

// replacementFailure describes a failed or cancelled replacement job.
type replacementFailure struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
	// OriginalState says where the original is, decided from what the
	// replacement actually did: originalInPlace (not moved, overwritten or
	// removed), originalInRecovery (a recovery copy holds it) or originalUnclear
	// (it may have been changed and the state does not say where it is).
	OriginalState  string `json:"original_state"`
	OriginalIntact bool   `json:"original_intact"`
}

const (
	originalInPlace    = "in_place"
	originalInRecovery = "recovery"
	originalUnclear    = "unclear"
)

// promotionStep is the index of a named step of the replacement workflow, or
// -1 when the template is not known.
func (s *Server) promotionStep(name string) int {
	tmpl, ok := s.engine.GetTemplate("promote_transcode_candidate")
	if !ok {
		return -1
	}
	for i, step := range tmpl.Steps {
		if step.Name == name {
			return i
		}
	}
	return -1
}

// originalState says where the original is after a replacement stopped, from
// the promotion's own state and never from the failure text.
//   - In place: nothing moved, published or removed. In copy mode the workflow
//     had not gone past preserve_original, which only copies. In move-aside mode
//     preserve_original makes no copy and the original moves only at publish or
//     removal, so it is still in place until import_candidate has finished
//     without a move marker.
//   - In recovery: copy mode past preserve_original (a same-name publish renames
//     the candidate over the original without setting any older flag, so the
//     step decides), or the original was moved aside into its recovery entry.
//   - Unclear: anything else. It is never reported as in place.
func (s *Server) originalState(r operationRecord) string {
	p := operationMap(operationValue(r, "promotion"))
	if p == nil {
		return originalInPlace
	}
	preserve, importStep := s.promotionStep("preserve_original"), s.promotionStep("import_candidate")
	if preserve < 0 {
		return originalUnclear
	}
	step, mode := r.inst.CurrentStep, operationString(p["recovery_mode"])
	changed := p["old_removed"] == true || p["moved_aside"] == true || operationString(p["delete_sent_at"]) != "" || operationString(p["move_aside_started_at"]) != "" || r.state["filesystem_publish_started"] == true || r.state["filesystem_published"] == true
	if !changed {
		if step <= preserve || (mode == "move_aside" && importStep >= 0 && step <= importStep) {
			return originalInPlace
		}
	}
	if p["moved_aside"] == true || (mode == "copy" && step > preserve) {
		return originalInRecovery
	}
	return originalUnclear
}

func (s *Server) failureOf(r operationRecord) *replacementFailure {
	detail := batchReasonText(r.inst.ErrorJSON, true)
	reason := plainReplacementReason(detail)
	if r.inst.Status == store.ActionStatusCancelled {
		reason = "replacement was cancelled"
	}
	state := s.originalState(r)
	return &replacementFailure{Reason: reason, Detail: detail, OriginalState: state, OriginalIntact: state == originalInPlace}
}

// replacementRetryTarget names the action whose Retry can re-run a failed
// replacement, or "" when none can. It follows the engine's retry guard:
//   - A replacement that failed in plan_promotion belongs to its failed batch,
//     which refuses a direct retry. Retrying the batch resumes it at its
//     replacement step without encoding, reuses the approved plan and retries
//     every replacement of the batch that is in that state.
//   - A replacement that failed in approve_promotion has no retry: the batch does
//     not re-run it and the engine refuses a direct retry, because the approved
//     plan no longer matches.
//   - A replacement that failed later is retried on its own.
//
// A batch that has not failed has nothing to retry yet.
func (s *Server) replacementRetryTarget(batch, promotion store.ActionInstance) string {
	if promotion.Status != store.ActionStatusFailed || batch.Status != store.ActionStatusFailed {
		return ""
	}
	plan, approve := s.promotionStep("plan_promotion"), s.promotionStep("approve_promotion")
	if plan < 0 || approve < 0 {
		return ""
	}
	switch {
	case promotion.CurrentStep <= plan:
		return batch.ID
	case promotion.CurrentStep <= approve:
		return ""
	}
	return promotion.ID
}

// batchPromotionFailure finds a file's failure in the batch's own promotion
// output, whose entries read "<item key>: <error>". It covers a file whose
// promotion never started, so it has no action of its own to read.
func batchPromotionFailure(outputs map[string]any, itemKey string) string {
	failed, _ := operationMap(outputs["batch_promotion"])["failed"].([]any)
	prefix := itemKey + ": "
	for _, entry := range failed {
		if text, ok := entry.(string); ok && strings.HasPrefix(text, prefix) {
			return strings.TrimPrefix(text, prefix)
		}
	}
	return ""
}

// replacementWhere says where a replacement put things: the new file, and the
// original's backup while one exists. A backup is removed once the new file is
// verified; it stays after a failure or an uncertain outcome until it is
// removed from the recovery list. There is no time limit.
type replacementWhere struct {
	NewPath    string `json:"new_path,omitempty"`
	BackupPath string `json:"backup_path,omitempty"`
	// BackupRemoved is true once the backup was deleted after verification.
	BackupRemoved bool `json:"backup_removed,omitempty"`
}

func whereOf(r operationRecord) *replacementWhere {
	p := operationMap(operationValue(r, "promotion"))
	if p == nil {
		return nil
	}
	w := &replacementWhere{NewPath: operationString(p["new_path"]), BackupPath: operationString(p["recovery_path"])}
	if final := operationString(r.outputs["final_path"]); final != "" {
		w.NewPath = final
	}
	if r.outputs["recovery_retained"] == false {
		w.BackupPath, w.BackupRemoved = "", true
	}
	if w.NewPath == "" && w.BackupPath == "" && !w.BackupRemoved {
		return nil
	}
	return w
}

// replacementMemberView is one planned file's progress.
type replacementMemberView struct {
	ItemKey       string              `json:"item_key"`
	TranscodeID   string              `json:"transcode_action_id"`
	Name          string              `json:"name"`
	Phase         string              `json:"phase"`
	Percent       *int                `json:"percent,omitempty"`
	BytesRead     int64               `json:"bytes_read,omitempty"`
	TotalBytes    int64               `json:"total_bytes,omitempty"`
	FreedBytes    *int64              `json:"freed_bytes,omitempty"`
	ReplacementID string              `json:"replacement_action_id,omitempty"`
	Failure       *replacementFailure `json:"failure,omitempty"`
}

// replacementProgress is what an approved (or waiting) batch plan is doing.
type replacementProgress struct {
	Approved bool `json:"approved"`
	Total    int  `json:"total"`
	Done     int  `json:"done"`
	Failed   int  `json:"failed"`
	// FreesBytes is what the whole plan frees once replaced; FreedBytes is what
	// the finished files have freed so far.
	FreesBytes int64 `json:"frees_bytes,omitempty"`
	FreedBytes int64 `json:"freed_bytes,omitempty"`
	// BatchRetryCount is how many failed files one Retry of the batch re-runs.
	BatchRetryCount int `json:"batch_retry_count,omitempty"`
	// Retryable counts failed files that have a Retry of their own (through the
	// batch or on their own promotion); BatchRecordedFailed counts failures that
	// only the batch recorded. When no file is retryable and only the batch
	// recorded failures, a transient error can still be retried from the batch.
	Retryable           int                     `json:"retryable,omitempty"`
	BatchRecordedFailed int                     `json:"batch_recorded_failed,omitempty"`
	Members             []replacementMemberView `json:"members"`
}

// batchReplacementProgress is nil when the batch has no prepared plan.
func (s *Server) batchReplacementProgress(batch operationRecord, byID map[string]operationRecord, replacements map[string]string) *replacementProgress {
	members, approved := batchPlanState(batch.state)
	if len(members) == 0 {
		return nil
	}
	out := &replacementProgress{Approved: approved, Total: len(members), Members: make([]replacementMemberView, 0, len(members))}
	for _, m := range members {
		view := replacementMemberView{ItemKey: m.ItemKey, TranscodeID: m.SourceAction, Name: memberName(m), Phase: phaseWaiting}
		if source, ok := byID[m.SourceAction]; ok {
			out.FreesBytes += replacementFactsOf(source).SavedBytes
		}
		if id := replacements[m.SourceAction]; id != "" && approved {
			if pr, ok := byID[id]; ok {
				view.ReplacementID = id
				phase, percent, read, total := s.replacementPhase(pr)
				view.Phase, view.Percent, view.BytesRead, view.TotalBytes = phase, percent, read, total
				switch phase {
				case phaseDone:
					out.Done++
					if pr.savings.RealizedSavedBytes != nil {
						view.FreedBytes = pr.savings.RealizedSavedBytes
					} else if p := operationMap(operationValue(pr, "promotion")); operationNumber(p["original_bytes"]) > 0 && operationNumber(p["candidate_bytes"]) > 0 {
						freed := int64(operationNumber(p["original_bytes"]) - operationNumber(p["candidate_bytes"]))
						view.FreedBytes = &freed
					}
					if view.FreedBytes != nil {
						out.FreedBytes += *view.FreedBytes
					}
				case phaseFailed:
					out.Failed++
					view.Failure = s.failureOf(pr)
					switch target := s.replacementRetryTarget(batch.inst, pr.inst); target {
					case "":
					case batch.inst.ID:
						out.BatchRetryCount++
						out.Retryable++
					default:
						out.Retryable++
					}
				}
			}
		}
		if view.Failure == nil && approved && batch.inst.Status == store.ActionStatusFailed && view.ReplacementID == "" {
			// The batch recorded this file's failure although its promotion never
			// produced an action.
			if detail := batchPromotionFailure(batch.outputs, m.ItemKey); detail != "" {
				view.Phase, view.Failure = phaseFailed, &replacementFailure{Reason: plainReplacementReason(detail), Detail: detail, OriginalState: originalInPlace, OriginalIntact: true}
				out.Failed++
				out.BatchRecordedFailed++
			}
		}
		out.Members = append(out.Members, view)
	}
	return out
}
