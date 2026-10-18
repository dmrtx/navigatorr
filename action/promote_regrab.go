package action

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/jakenesler/navigatorr/arrservice"
)

// CompactRegrabWarning is shown before any compact replacement is approved.
// A compact file is SDR at a lower resolution than the release it replaces,
// so Sonarr or Radarr may treat it as an upgrade target and grab the
// original release again. Navigatorr reports how the item is tracked but
// never changes monitoring or quality profiles itself.
const CompactRegrabWarning = "Compact replacements are SDR at a lower resolution than the original. Radarr or Sonarr may see the new file as below the quality profile's cutoff and download the original release again. Before approving, unmonitor the item or move it to a quality profile whose cutoff the compact file meets. Navigatorr does not change these settings."

// arrTracking is a read-only snapshot of how Sonarr or Radarr tracks an
// item when a compact replacement is planned.
type arrTracking struct {
	Monitored      *bool  `json:"arr_monitored,omitempty"`
	QualityProfile string `json:"arr_quality_profile,omitempty"`
}

// readArrTracking reads the monitored flag and quality profile of a series
// or movie. It is advisory: a failed read leaves the fields empty and never
// blocks the promotion, whose own checks are unchanged.
func readArrTracking(ctx context.Context, svc *arrservice.Service, endpoint string) arrTracking {
	var t arrTracking
	if svc == nil {
		return t
	}
	data, err := svc.Get(ctx, endpoint, nil)
	if err != nil {
		return t
	}
	var item struct {
		Monitored        *bool `json:"monitored"`
		QualityProfileID int   `json:"qualityProfileId"`
	}
	if json.Unmarshal(data, &item) != nil {
		return t
	}
	t.Monitored = item.Monitored
	t.QualityProfile = arrQualityProfileName(ctx, svc, item.QualityProfileID)
	return t
}

func arrQualityProfileName(ctx context.Context, svc *arrservice.Service, id int) string {
	if id <= 0 {
		return ""
	}
	label := "#" + strconv.Itoa(id)
	data, err := svc.Get(ctx, "/api/v3/qualityprofile/"+strconv.Itoa(id), nil)
	if err != nil {
		return label
	}
	var profile struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &profile) != nil || profile.Name == "" {
		return label
	}
	return profile.Name
}

// batchPromotionCompact reports whether any member was produced by the
// compact goal; a per-file goal change can make one compact inside a batch
// whose own goal is not.
func (e *Engine) batchPromotionCompact(plan *batchPromotionPlan) bool {
	for _, m := range plan.Members {
		child, err := e.deps.Store.GetActionInstance(m.SourceAction)
		if err != nil || child == nil {
			continue
		}
		if getString(parseExecutionContext(child, e).State, "mode") == "compact" {
			return true
		}
	}
	return false
}

// batchPromotionNotice is persisted with a compact batch's approval so the
// review shows the warning and the series' tracking next to the file list.
func (e *Engine) batchPromotionNotice(ctx context.Context, plan *batchPromotionPlan) map[string]any {
	if !e.batchPromotionCompact(plan) {
		return nil
	}
	notice := map[string]any{"regrab_warning": CompactRegrabWarning}
	if planService(plan) == "sonarr" && plan.SeriesID > 0 && e.deps.Registry != nil {
		if svc, err := e.deps.Registry.Get("sonarr"); err == nil {
			t := readArrTracking(ctx, svc, "/api/v3/series/"+strconv.Itoa(plan.SeriesID))
			if t.Monitored != nil {
				notice["arr_monitored"] = *t.Monitored
			}
			if t.QualityProfile != "" {
				notice["arr_quality_profile"] = t.QualityProfile
			}
		}
	}
	return notice
}
