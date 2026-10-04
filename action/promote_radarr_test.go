package action

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/config"
)

func moviePromotionHarness(t *testing.T) *promotionHarness {
	t.Helper()
	h := newPromotionHarness(t)
	h.movie = true
	h.episodes = []promotionEpisode{{ID: 1, SeriesID: 1, EpisodeFileID: 101}}
	old := h.files[101]
	old.MovieID = 1
	old.SeriesID = 0
	h.files[101] = old
	deps := h.engine.Deps()
	deps.Config.Services = map[string]config.ServiceConfig{"radarr": deps.Config.Services["sonarr"]}
	deps.Registry = arrservice.NewRegistry(deps.Config)
	h.engine = NewEngine(deps)
	h.engine.registerPromoteTranscodeTemplate()
	return h
}

func runMoviePromotion(t *testing.T, h *promotionHarness) *ActionResult {
	t.Helper()
	r, err := h.engine.Run(context.Background(), "promote_transcode_candidate", map[string]any{"transcode_action_id": "source-transcode", "movie_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRadarrPromotionApprovalAndRecovery(t *testing.T) {
	for _, dropped := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "lost-import-response"}[dropped], func(t *testing.T) {
			h := moviePromotionHarness(t)
			h.dropImportResponse = dropped
			r := runMoviePromotion(t, h)
			if r.Status != StatusWaitingDecision || h.imports != 0 {
				t.Fatalf("approval bypassed: %+v", r)
			}
			r = h.resume(r.ID, "approve")
			h.restart()
			r = h.finish(r)
			if r.Status != StatusCompleted {
				t.Fatalf("status=%s error=%s waiting=%s", r.Status, r.Error, r.WaitingReason)
			}
			if h.imports != 1 || strings.Join(h.mutationOrder, ",") != "import,delete,rename,rescan" {
				t.Fatalf("duplicate or out-of-order mutations: %v", h.mutationOrder)
			}
			p, err := loadPromotion(&ExecutionContext{State: r.State})
			if err != nil {
				t.Fatal(err)
			}
			if p.MovieID != 1 || !p.RecoveryCleanupCompleted || len(h.files) != 1 || h.episodes[0].EpisodeFileID != 202 {
				t.Fatalf("wrong movie state: %+v", p)
			}
			if _, err := os.Stat(p.BackupPath); !os.IsNotExist(err) {
				t.Fatal("recovery copy not cleaned after verified success")
			}
		})
	}
}

func TestRadarrPromotionRejectsIdentityDriftBeforeDelete(t *testing.T) {
	h := moviePromotionHarness(t)
	r := runMoviePromotion(t, h)
	// The exact old-record read immediately before DELETE drifts to another movie.
	h.poisonOldMovie = true
	r = h.resume(r.ID, "approve")
	r = h.finish(r)
	if r.Status != StatusFailed || h.deletes != 0 {
		t.Fatalf("deleted foreign movie record: status=%s deletes=%d error=%s", r.Status, h.deletes, r.Error)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.BackupPath); err != nil {
		t.Fatal("lost recovery copy after identity drift", err)
	}
}
