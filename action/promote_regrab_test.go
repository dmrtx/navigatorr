package action

import (
	"context"
	"strings"
	"testing"
)

// markCompactSource turns the harness's source transcode into a compact one.
func markCompactSource(t *testing.T, h *promotionHarness) {
	t.Helper()
	inst, err := h.st.GetActionInstance("source-transcode")
	if err != nil || inst == nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	ec.State["mode"] = "compact"
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
}

func TestCompactPromotionWarnsAboutRegrab(t *testing.T) {
	for _, tc := range []struct {
		name      string
		movie     bool
		episodes  []bool
		monitored bool
	}{
		{"sonarr monitored episode", false, []bool{true, false}, true},
		{"sonarr unmonitored episodes", false, []bool{false, false}, false},
		{"radarr monitored movie", true, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *promotionHarness
			if tc.movie {
				h = moviePromotionHarness(t)
			} else {
				h = newPromotionHarness(t)
				for i, m := range tc.episodes {
					h.episodes[i].Monitored = m
				}
			}
			h.tracking = map[string]any{"monitored": true, "qualityProfileId": 4}
			markCompactSource(t, h)
			inputs := map[string]any{"transcode_action_id": "source-transcode", "series_id": 1}
			if tc.movie {
				inputs = map[string]any{"transcode_action_id": "source-transcode", "movie_id": 1, "service": "radarr"}
			}
			r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", inputs)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != StatusWaitingDecision || !strings.Contains(r.WaitingReason, CompactRegrabWarning) {
				t.Fatalf("compact approval lacks the re-grab warning: %s %q", r.Status, r.WaitingReason)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil {
				t.Fatal(err)
			}
			if p.RegrabWarning != CompactRegrabWarning || p.Monitored == nil || *p.Monitored != tc.monitored || p.QualityProfile != "HD-1080p" {
				t.Fatalf("tracking %+v monitored=%v", p.arrTracking, p.Monitored)
			}
			if h.imports != 0 || len(h.mutationOrder) != 0 {
				t.Fatalf("approval step changed the library: %v", h.mutationOrder)
			}
		})
	}
}

func TestPromotionWithoutCompactHasNoRegrabWarning(t *testing.T) {
	h := newPromotionHarness(t)
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "series_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.WaitingReason, "Radarr or Sonarr may") || p.RegrabWarning != "" || p.Monitored != nil {
		t.Fatalf("non-compact promotion warned: %q %+v", r.WaitingReason, p.arrTracking)
	}
}

func TestBatchPromotionNoticeForCompactMembers(t *testing.T) {
	h := newPromotionHarness(t)
	h.tracking = map[string]any{"monitored": false, "qualityProfileId": 4}
	plan := &batchPromotionPlan{BatchID: "batch", SeriesID: 1, Members: []batchPromotionMember{{ItemKey: "one", SourceAction: "source-transcode"}}}
	if n := h.engine.batchPromotionNotice(context.Background(), plan); n != nil {
		t.Fatalf("non-compact batch warned: %v", n)
	}
	markCompactSource(t, h)
	n := h.engine.batchPromotionNotice(context.Background(), plan)
	if n == nil || n["regrab_warning"] != CompactRegrabWarning || n["arr_monitored"] != false || n["arr_quality_profile"] != "HD-1080p" {
		t.Fatalf("compact batch notice %v", n)
	}
	if len(h.mutationOrder) != 0 {
		t.Fatalf("notice changed the library: %v", h.mutationOrder)
	}
}

// The batch approval counts the candidates a size budget encode left below
// the quality target, so an approval answered outside the web review says so.
func TestBatchPromotionCountsBelowTargetMembers(t *testing.T) {
	h := newPromotionHarness(t)
	plan := &batchPromotionPlan{BatchID: "batch", SeriesID: 1, Members: []batchPromotionMember{{ItemKey: "one", SourceAction: "source-transcode"}, {ItemKey: "gone", SourceAction: "missing"}}}
	if n := h.engine.batchPromotionBelowTarget(plan); n != 0 {
		t.Fatalf("counted %d without a budget encode", n)
	}
	inst, err := h.st.GetActionInstance("source-transcode")
	if err != nil || inst == nil {
		t.Fatal(err)
	}
	ec := parseExecutionContext(inst, h.engine)
	ec.State[sizeBudgetKey] = &SizeBudgetRecord{BudgetBytes: 1_000_000_000, Outcome: SizeBudgetBelowTarget}
	inst.StateJSON = toJSON(ec.State)
	if err := h.st.UpdateActionInstance(*inst); err != nil {
		t.Fatal(err)
	}
	if n := h.engine.batchPromotionBelowTarget(plan); n != 1 {
		t.Fatalf("counted %d below-target members, want 1", n)
	}
}

// The review prepared from the batch dialog stores the notice for exactly its
// own plan: a compact selection gets the warning and the series' tracking,
// and a notice left by an earlier plan never survives into a plan that needs
// none, or into one it does not describe.
func TestPrepareBatchPromotionStoresTheRegrabNotice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		compact bool
	}{{"compact", true}, {"not compact", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPromotionHarness(t)
			h.tracking = map[string]any{"monitored": false, "qualityProfileId": 4}
			if tc.compact {
				markCompactSource(t, h)
			}
			id := seedFinishedReviewBatch(t, h, "sonarr")
			inst, err := h.st.GetActionInstance(id)
			if err != nil || inst == nil {
				t.Fatal(err)
			}
			ec := parseExecutionContext(inst, h.engine)
			ec.State["batch_promotion_notice"] = map[string]any{"regrab_warning": "stale", "plan_digest": "sha256:earlier", "arr_monitored": true}
			inst.StateJSON = toJSON(ec.State)
			if err := h.st.UpdateActionInstance(*inst); err != nil {
				t.Fatal(err)
			}
			review, err := h.engine.BatchCandidates(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.engine.PrepareBatchPromotion(context.Background(), id, review.Version, "request-one", []string{"epfile-101"}); err != nil {
				t.Fatal(err)
			}
			after, _ := h.st.GetActionInstance(id)
			state := parseExecutionContext(after, h.engine).State
			plan := getBatchPromotionPlan(state["batch_promotion_plan"])
			notice, _ := state["batch_promotion_notice"].(map[string]any)
			if !tc.compact {
				if notice != nil {
					t.Fatalf("stale notice survived a plan that needs none: %v", notice)
				}
				return
			}
			if notice["regrab_warning"] != CompactRegrabWarning || notice["plan_digest"] != plan.Digest || notice["arr_monitored"] != false || notice["arr_quality_profile"] != "HD-1080p" {
				t.Fatalf("notice %v for plan %s", notice, plan.Digest)
			}
			if len(h.mutationOrder) != 0 {
				t.Fatalf("preparing the review changed the library: %v", h.mutationOrder)
			}
		})
	}
}
