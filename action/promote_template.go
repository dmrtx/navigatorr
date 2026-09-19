package action

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
)

// Promotion is a separate, explicitly approved action. transcode_media remains
// candidate-only; configuring destructive actions does not implicitly approve
// importing any of its outputs.
func (e *Engine) registerPromoteTranscodeTemplate() {
	e.RegisterTemplate(ActionTemplate{
		Name: "promote_transcode_candidate", Version: 1, Destructive: true,
		AutoReconcile:  true,
		Description:    "Promotes a completed, validated transcode into Sonarr after explicit approval, preserving a verified recovery copy until import, old-file cleanup, rename and rescan are verified. Persists external command intents and never blindly resubmits uncertain imports.",
		RequiredInputs: []string{"transcode_action_id", "series_id"}, OptionalInputs: []string{"service"},
		Steps: []StepDefinition{
			{Name: "plan_promotion", Description: "Verify original and candidate, and resolve all episodes sharing the original file", Run: e.stepPromotePlan},
			{Name: "approve_promotion", Description: "Present the exact replacement for an explicit approve decision", Run: e.stepPromoteApprove},
			{Name: "preserve_original", Description: "Create and verify a recovery copy before any Sonarr mutation", Run: e.stepPromotePreserve},
			{Name: "import_candidate", Description: "Import once and reconcile Sonarr episode IDs, physical candidate SHA and streams", Run: e.stepPromoteImport},
			{Name: "remove_old_file", Description: "Reverify adoption and integrity before removing the old episodeFile", Run: e.stepPromoteRemoveOld},
			{Name: "rename_candidate", Description: "Move the active file out of the temporary candidate directory through Sonarr", Run: e.stepPromoteRename},
			{Name: "rescan_library", Description: "Rescan and verify Sonarr's final library state", Run: e.stepPromoteRescan},
			{Name: "finalize_promotion", Description: "Verify one active file per affected episode, record savings and remove the recovery copy", Run: e.stepPromoteFinalize},
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
	SourceActionID         string                       `json:"transcode_action_id"`
	Service                string                       `json:"service"`
	SeriesID               int                          `json:"series_id"`
	SeriesPath             string                       `json:"series_path"`
	OriginalPath           string                       `json:"original_path"`
	CandidatePath          string                       `json:"candidate_path"`
	OriginalSHA            string                       `json:"original_sha256"`
	CandidateSHA           string                       `json:"candidate_sha256"`
	OriginalBytes          int64                        `json:"original_bytes"`
	CandidateBytes         int64                        `json:"candidate_bytes"`
	OriginalFileID         int                          `json:"original_episode_file_id"`
	EpisodeIDs             []int                        `json:"episode_ids"`
	Quality                json.RawMessage              `json:"quality"`
	Languages              json.RawMessage              `json:"languages"`
	ReleaseGroup           string                       `json:"release_group,omitempty"`
	Approved               bool                         `json:"approved"`
	BackupPath             string                       `json:"recovery_path,omitempty"`
	BackupVerified         bool                         `json:"recovery_verified"`
	RecoveryCopyOwner      string                       `json:"recovery_copy_owner,omitempty"`
	RecoveryCopyComplete   bool                         `json:"recovery_copy_complete,omitempty"`
	NewFileID              int                          `json:"new_episode_file_id,omitempty"`
	NewPath                string                       `json:"new_path,omitempty"`
	Commands               map[string]*promotionCommand `json:"commands,omitempty"`
	DeleteSentAt           string                       `json:"delete_sent_at,omitempty"`
	OldRemoved             bool                         `json:"old_removed"`
	RecoveryCleanupStarted bool                         `json:"recovery_cleanup_started,omitempty"`
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
	if p.SourceActionID == "" || p.OriginalSHA == "" || p.CandidateSHA == "" || p.OriginalFileID <= 0 || len(p.EpisodeIDs) == 0 {
		return nil, fmt.Errorf("promotion integrity baseline is missing")
	}
	if p.Commands == nil {
		p.Commands = make(map[string]*promotionCommand)
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
	return StepResult{Status: StepFailed, Error: err.Error()}, nil
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
	if !p.Approved {
		switch strings.ToLower(ec.Decision) {
		case "approve":
			p.Approved = true
		case "reject", "cancel":
			return promoteFailed(fmt.Errorf("candidate promotion rejected; original and candidate remain untouched"))
		default:
			return StepResult{Status: StepWaitingDecision, WaitingReason: "Approve the verified candidate and affected Sonarr episodes before library replacement", WaitingOptions: []WaitingOption{{Decision: "approve", Description: "Promote this candidate, preserve recovery copy until verified, and remove the old library file"}, {Decision: "reject", Description: "Keep original and candidate unchanged"}}, Outputs: map[string]any{"promotion": p}}, nil
		}
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted}, nil
}
