package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/store"
)

// Promotion is a separate, explicitly approved action. transcode_media remains
// candidate-only; configuring destructive actions does not implicitly approve
// importing any of its outputs.
func (e *Engine) registerPromoteTranscodeTemplate() {
	e.RegisterTemplate(ActionTemplate{
		Name: "promote_transcode_candidate", Version: 2, Destructive: true,
		AutoReconcile:   true,
		ImmutableInputs: true,
		Description:     "Replaces a completed, validated transcode locally with service=filesystem (no library IDs), or promotes it into Sonarr/Radarr, after explicit approval, keeping the verified original recoverable (moved into a private recovery directory, or copied when it cannot be moved) until import, old-file cleanup, rename and rescan are verified. Persists external command intents and never blindly resubmits uncertain imports.",
		RequiredInputs:  []string{"transcode_action_id"}, OptionalInputs: []string{"service", "series_id", "movie_id", "batch_promote_parent_id", "batch_promote_item_key", "batch_promote_digest"},
		Steps: []StepDefinition{
			{Name: "plan_promotion", Description: "Verify original and candidate, and resolve the movie or all episodes sharing the original file", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromotePlan)},
			{Name: "approve_promotion", Description: "Present the exact replacement for an explicit approve decision", Run: e.stepPromoteApprove},
			{Name: "preserve_original", Description: "Verify the original and prepare its recovery before any library mutation", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromotePreserve)},
			{Name: "import_candidate", Description: "Import once and reconcile library associations, physical candidate SHA and streams", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromoteImport)},
			{Name: "remove_old_file", Description: "Reverify adoption and integrity before removing the old library file", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromoteRemoveOld)},
			{Name: "rename_candidate", Description: "Move the active file out of the temporary candidate directory through the library service", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromoteRename)},
			{Name: "rescan_library", Description: "Rescan and verify the final library state", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromoteRescan)},
			{Name: "finalize_promotion", Description: "Verify one active file per affected library item, record savings and remove the recovery copy", Run: e.measureCoordinatorPhase("coordinator_promotion", e.stepPromoteFinalize)},
		},
	})
}

type promotionCommand struct {
	ID      int            `json:"id,omitempty"`
	SentAt  string         `json:"sent_at,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
	Failed  bool           `json:"failed,omitempty"`
	Done    bool           `json:"done,omitempty"`
}

type promotionState struct {
	MovieID                int    `json:"movie_id,omitempty"`
	SourceActionID         string `json:"transcode_action_id"`
	Service                string `json:"service"`
	SeriesID               int    `json:"series_id"`
	SeriesPath             string `json:"series_path"`
	OriginalPath           string `json:"original_path"`
	CandidatePath          string `json:"candidate_path"`
	OriginalSHA            string `json:"original_sha256"`
	CandidateSHA           string `json:"candidate_sha256"`
	OriginalBytes          int64  `json:"original_bytes"`
	CandidateBytes         int64  `json:"candidate_bytes"`
	CandidateMediaVerified bool   `json:"candidate_media_verified,omitempty"`
	// CandidateRetained marks a candidate that is still on its worker when the
	// plan is made: its size and SHA-256 are the worker's attestation, and
	// preserve_original has the worker upload it before anything else.
	// CandidateUploaded says that upload finished.
	CandidateRetained                 bool                         `json:"candidate_retained,omitempty"`
	CandidateUploaded                 bool                         `json:"candidate_uploaded,omitempty"`
	ExpectedVideoCodec                string                       `json:"expected_video_codec,omitempty"`
	OriginalFileID                    int                          `json:"original_episode_file_id"`
	EpisodeIDs                        []int                        `json:"episode_ids"`
	Quality                           json.RawMessage              `json:"quality"`
	Languages                         json.RawMessage              `json:"languages"`
	ReleaseGroup                      string                       `json:"release_group,omitempty"`
	Approved                          bool                         `json:"approved"`
	BackupPath                        string                       `json:"recovery_path,omitempty"`
	BackupVerified                    bool                         `json:"recovery_verified"`
	RecoveryVerifiedBeforeReplacement bool                         `json:"recovery_verified_before_replacement,omitempty"`
	RecoveryCleanupCompleted          bool                         `json:"recovery_cleanup_completed,omitempty"`
	NewFileID                         int                          `json:"new_episode_file_id,omitempty"`
	NewPath                           string                       `json:"new_path,omitempty"`
	Commands                          map[string]*promotionCommand `json:"commands,omitempty"`
	DeleteSentAt                      string                       `json:"delete_sent_at,omitempty"`
	OldRemoved                        bool                         `json:"old_removed"`
	RecoveryCleanupStarted            bool                         `json:"recovery_cleanup_started,omitempty"`
	// Persisted with RecoveryCleanupStarted, before cleanup.truncate_before_delete
	// empties the verified recovery copy: a resumed cleanup that finds the
	// copy empty finishes the unlink instead of verifying it again.
	RecoveryEmptyStarted bool `json:"recovery_empty_started,omitempty"`
	// What emptying the recovery copy before its unlink did: "emptied" or
	// "skipped: <reason>"; empty when it was off or not tried.
	RecoveryTruncate string `json:"recovery_truncate,omitempty"`
	// How the recovery copy was made; see promotionCopyData.
	RecoveryCopy *promotionCopyResult `json:"recovery_copy,omitempty"`
	// move_aside renames the original into the recovery directory right
	// before the old library file is removed; copy (and legacy "") copies it
	// before import. See promotionMoveAside.
	RecoveryMode string `json:"recovery_mode,omitempty"`
	// Why that mode was chosen (a fixed code, never a path).
	RecoveryModeReason string `json:"recovery_mode_reason,omitempty"`
	// What a failed mount probe observed: its step, the filesystem type and
	// the error, never a path. Empty for every other reason.
	RecoveryModeDetail string `json:"recovery_mode_detail,omitempty"`
	// Fingerprints of full hashes, kept for identity checks across waits;
	// see promotionVerifyByIdentity.
	OriginalFingerprint  *promotionFingerprint `json:"original_fingerprint,omitempty"`
	BackupFingerprint    *promotionFingerprint `json:"recovery_fingerprint,omitempty"`
	CandidateFingerprint *promotionFingerprint `json:"candidate_fingerprint,omitempty"`
	// RecoveryProof says what proves a moved-aside original is the same
	// file: "helper" (the NAS helper's identity, OriginalHelper and
	// BackupHelper) or empty (the mount's fingerprints above).
	RecoveryProof      string       `json:"recovery_proof,omitempty"`
	OriginalHelper     *helperProof `json:"original_helper,omitempty"`
	BackupHelper       *helperProof `json:"recovery_helper,omitempty"`
	MoveAsideStartedAt string       `json:"move_aside_started_at,omitempty"`
	MovedAside         bool         `json:"moved_aside,omitempty"`
	// MoveAsideRefused keeps why the rename was refused and the promotion
	// switched to a recovery copy.
	MoveAsideRefused string `json:"move_aside_refused,omitempty"`
	// MoveBackStartedAt is the durable intent to rename a moved-aside
	// original back to its library path after a same-name publish failed.
	MoveBackStartedAt string `json:"move_back_started_at,omitempty"`
	// The library removed the original itself before it could be moved aside,
	// so there was never anything to recover.
	OriginalRemovedByLibrary bool `json:"original_removed_by_library,omitempty"`
	// Set for compact candidates only: the re-grab warning and a read-only
	// snapshot of how Sonarr/Radarr tracks the item.
	RegrabWarning string `json:"regrab_warning,omitempty"`
	// OriginalStatMod is the original's modification time when plan found it
	// unchanged since a transcode whose digest was pinned at staging. Set, the
	// original is proved by size and this time and never read again.
	// RecoverySample is the digest over sampled ranges of the original that its
	// recovery copy must reproduce. See promote_cheap.go.
	OriginalStatMod string `json:"original_stat_mod_time,omitempty"`
	RecoverySample  string `json:"recovery_sample_sha256,omitempty"`
	arrTracking
}

func loadPromotion(ec *ExecutionContext) (*promotionState, error) {
	p := new(promotionState)
	b, err := json.Marshal(ec.State["promotion"])
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, err
	}
	if p.SourceActionID == "" || p.OriginalSHA == "" || p.CandidateSHA == "" || (p.Service != "filesystem" && (p.OriginalFileID <= 0 || len(p.EpisodeIDs) == 0)) {
		return nil, fmt.Errorf("promotion integrity baseline is missing")
	}
	if p.Commands == nil {
		p.Commands = make(map[string]*promotionCommand)
	}
	if err := promotionBackupPathMatches(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (e *Engine) savePromotion(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	// Persist before sending a side effect. The ordinary end-of-step checkpoint
	// is too late when a process exits or an HTTP response is lost.
	ec.State["promotion"] = p
	ec.Outputs["promotion"] = p
	return e.persistExecutionState(ctx, ec)
}

func promoteFailed(err error) (StepResult, error) {
	if errors.Is(err, errActionBusy) {
		return promoteWait("Waiting for batch coordination before replacement")
	}
	if _, busy := err.(*promotionBusyError); busy {
		return promoteWait(err.Error())
	}
	return StepResult{Status: StepFailed, Error: err.Error()}, nil
}

type promotionBusyError struct {
	blocker store.PromotionSeriesOwnerInfo
}

func (e *promotionBusyError) Error() string {
	what := "Waiting for the other promotion of this Sonarr series to finish or be recovered"
	if e.blocker.ActionID == "" {
		return what
	}
	name := e.blocker.ActionName
	if name == "" {
		name = "action"
	}
	if e.blocker.Status == "" {
		return fmt.Sprintf("%s: blocked by %s %s", what, name, e.blocker.ActionID)
	}
	return fmt.Sprintf("%s: blocked by %s %s (%s)", what, name, e.blocker.ActionID, e.blocker.Status)
}

func (e *Engine) claimPromotionSeries(ec *ExecutionContext, service string, seriesID int) error {
	owner := ec.InstanceID
	if parent := getString(ec.Inputs, "batch_promote_parent_id"); parent != "" {
		owner = parent
	}
	holder, err := e.deps.Store.ClaimPromotionSeriesOwner(service, seriesID, owner)
	if err != nil {
		return err
	}
	if holder.ActionID != owner {
		return &promotionBusyError{blocker: holder}
	}
	return nil
}

func promoteWait(reason string) (StepResult, error) {
	return StepResult{Status: StepWaitingExternal, WaitingReason: reason, WaitingCondition: "sonarr_promotion_pending"}, nil
}

func (e *Engine) promotionService(p *promotionState) (*arrservice.Service, error) {
	if e.deps.Registry == nil {
		return nil, fmt.Errorf("Sonarr registry is not configured")
	}
	return e.deps.Registry.Get(p.Service)
}

func (e *Engine) promotionMutation(ec *ExecutionContext) (*promotionState, *arrservice.Service, error) {
	p, err := loadPromotion(ec)
	if err != nil {
		return nil, nil, err
	}
	if !e.AllowDestructive() {
		return nil, nil, fmt.Errorf("allow_destructive must be enabled for candidate promotion")
	}
	if !p.Approved {
		return nil, nil, fmt.Errorf("candidate promotion has not been approved")
	}
	if e.deps.Fs == nil {
		return nil, nil, fmt.Errorf("filesystem resolver is required")
	}
	if getString(ec.Inputs, "batch_promote_parent_id") != "" {
		allowRecovery := getString(ec.State, "promotion_recovery_parent_digest") == getString(ec.Inputs, "batch_promote_digest")
		if err := e.verifyBatchPromotionApprovalState(ec, p, allowRecovery); err != nil {
			return nil, nil, err
		}
	}
	if p.Service == "filesystem" {
		if err := e.claimPromotionSeries(ec, "filesystem-destination:"+p.NewPath, 1); err != nil {
			return nil, nil, err
		}
		return p, nil, nil
	}
	if err := e.claimPromotionSeries(ec, p.Service, p.SeriesID); err != nil {
		return nil, nil, err
	}
	svc, err := e.promotionService(p)
	return p, svc, err
}

func withinPromotionPath(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func temporaryPromotionPath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part == ".navigatorr-candidates" {
			return true
		}
	}
	return false
}

func (e *Engine) stepPromoteApprove(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, err := loadPromotion(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if !e.AllowDestructive() {
		return promoteFailed(fmt.Errorf("allow_destructive must be enabled for candidate promotion"))
	}
	if batchLinkedPromotion(ec.Inputs) {
		if err := e.verifyBatchPromotionApproval(ec, p); err != nil {
			return promoteFailed(err)
		}
		p.Approved = true
	}
	if !p.Approved {
		switch strings.ToLower(ec.Decision) {
		case "approve":
			// resume rejects a stale digest before the decision is stored; this
			// keeps the step safe for any other path that supplies one.
			if err := verifyPromotionPlanDigest(ctx, p); err != nil {
				return StepResult{Status: StepWaitingDecision, WaitingReason: err.Error(), WaitingOptions: promotionApprovalOptions(), Outputs: map[string]any{"promotion": p}}, nil
			}
			p.Approved = true
		case "reject", "cancel":
			// A candidate still on its worker is released with the rejected plan;
			// one on the share stays, as it always has.
			return promoteFailedFinal(fmt.Errorf("candidate promotion rejected; original and candidate remain untouched")), nil
		default:
			reason := "Approve the verified candidate and affected library items before replacement"
			if p.RegrabWarning != "" {
				reason += ". " + p.RegrabWarning
			}
			return StepResult{Status: StepWaitingDecision, WaitingReason: reason, WaitingOptions: promotionApprovalOptions(), Outputs: map[string]any{"promotion": p}}, nil
		}
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted}, nil
}

func promotionApprovalOptions() []WaitingOption {
	return []WaitingOption{{Decision: "approve", Description: "Promote this candidate, preserve recovery copy until verified, and remove the old library file. Requires plan_digest from the reviewed plan"}, {Decision: "reject", Description: "Keep original and candidate unchanged"}}
}
