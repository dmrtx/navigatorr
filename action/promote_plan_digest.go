package action

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/store"
)

// A single-file approval names the exact plan the user reviewed. The digest
// covers what the replacement will touch (paths, hashes, sizes and the library
// items), so a plan that changed after the review cannot receive the earlier
// approval. The batch coordinator binds its members the same way.
type promotionPlanDigestKey struct{}

// PromotionPlanDigest is the digest of a promotion state as persisted in
// action state or outputs.
func PromotionPlanDigest(raw any) string {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	p := new(promotionState)
	if json.Unmarshal(encoded, p) != nil || p.OriginalSHA == "" || p.CandidateSHA == "" || p.OriginalPath == "" || p.CandidatePath == "" {
		return ""
	}
	return p.planDigest()
}

func (p *promotionState) planDigest() string {
	b, _ := json.Marshal(struct {
		Version        int    `json:"v"`
		SourceActionID string `json:"transcode_action_id"`
		Service        string `json:"service"`
		SeriesID       int    `json:"series_id"`
		MovieID        int    `json:"movie_id"`
		OriginalFileID int    `json:"original_file_id"`
		EpisodeIDs     []int  `json:"episode_ids"`
		OriginalPath   string `json:"original_path"`
		CandidatePath  string `json:"candidate_path"`
		OriginalSHA    string `json:"original_sha256"`
		CandidateSHA   string `json:"candidate_sha256"`
		OriginalBytes  int64  `json:"original_bytes"`
		CandidateBytes int64  `json:"candidate_bytes"`
	}{1, p.SourceActionID, p.Service, p.SeriesID, p.MovieID, p.OriginalFileID, p.EpisodeIDs, p.OriginalPath, p.CandidatePath, p.OriginalSHA, p.CandidateSHA, p.OriginalBytes, p.CandidateBytes})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ResumeReviewedPromotion approves one reviewed single-file replacement. The
// decision is bound to the digest of the plan the caller reviewed.
func (e *Engine) ResumeReviewedPromotion(ctx context.Context, id, digest string) (*ActionResult, error) {
	return e.Resume(context.WithValue(ctx, promotionPlanDigestKey{}, strings.TrimSpace(digest)), id, "approve", nil)
}

// WithPromotionPlanDigest carries the reviewed plan digest of an approval made
// through a generic resume entry point.
func WithPromotionPlanDigest(ctx context.Context, digest string) context.Context {
	if strings.TrimSpace(digest) == "" {
		return ctx
	}
	return context.WithValue(ctx, promotionPlanDigestKey{}, strings.TrimSpace(digest))
}

func verifyPromotionPlanDigest(ctx context.Context, p *promotionState) error {
	got, _ := ctx.Value(promotionPlanDigestKey{}).(string)
	if got == "" {
		return fmt.Errorf("approval must carry the digest of the reviewed replacement plan (plan_digest); review the replacement again")
	}
	want := p.planDigest()
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return fmt.Errorf("the replacement plan changed since it was reviewed; review it again")
	}
	return nil
}

// Reject a stale or missing digest before a resume mutates the instance, so a
// refused approval leaves the job waiting for a correct one.
func (e *Engine) checkReviewedPromotion(ctx context.Context, inst *store.ActionInstance, decision string) error {
	if inst.ActionName != "promote_transcode_candidate" || !strings.EqualFold(strings.TrimSpace(decision), "approve") || inst.Status != StatusWaitingDecision {
		return nil
	}
	var inputs, state map[string]any
	if json.Unmarshal([]byte(inst.InputsJSON), &inputs) != nil || json.Unmarshal([]byte(inst.StateJSON), &state) != nil {
		return fmt.Errorf("promotion state is unreadable")
	}
	if batchLinkedPromotion(inputs) {
		return nil
	}
	if state["promotion"] == nil {
		// Planning has not produced a baseline; the approval step refuses it.
		return nil
	}
	encoded, _ := json.Marshal(state["promotion"])
	p := new(promotionState)
	if json.Unmarshal(encoded, p) != nil || p.OriginalSHA == "" || p.CandidateSHA == "" {
		return fmt.Errorf("promotion integrity baseline is missing")
	}
	if p.Approved {
		return nil
	}
	return verifyPromotionPlanDigest(ctx, p)
}

func batchLinkedPromotion(inputs map[string]any) bool {
	return getString(inputs, "batch_promote_parent_id") != "" || getString(inputs, "batch_promote_item_key") != "" || getString(inputs, "batch_promote_digest") != ""
}
