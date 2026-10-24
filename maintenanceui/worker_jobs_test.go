package maintenanceui

import (
	"testing"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/store"
	"github.com/jakenesler/navigatorr/transcode"
)

func TestActionOfWorkerJob(t *testing.T) {
	for job, want := range map[string]string{
		"job-act-transcode-media-1":         "act-transcode-media-1",
		"job-act-transcode-media-1-retry-2": "act-transcode-media-1",
		"other-1":                           "",
		"":                                  "",
	} {
		if got := actionOfWorkerJob(job); got != want {
			t.Fatalf("%q belongs to %q, want %q", job, got, want)
		}
	}
}

// A slot says which encode its job runs and how fast it goes; what the action
// does not know, and a worker that went quiet, add nothing.
func TestActivityJobsNameTheirEncodeAndSpeed(t *testing.T) {
	s, _ := testUI(t)
	st := s.engine.Deps().Store
	running := map[string]any{
		"mode_policy":    map[string]any{"version": action.ModePolicyV4, "recipe": map[string]any{"kind": "quality_ladder", "video_codec": "libx265"}},
		"quality_ladder": map[string]any{"video_codec": "libx265", "quality_values": []int{22, 19, 16}, "rung": 1, "outcome": "encoding"},
		"speed":          4.94, "fps": 118.36,
	}
	quiet := map[string]any{"speed": 3.0, "fps": 90.0, "progress_is_stale": true}
	for id, state := range map[string]map[string]any{"act-running": running, "act-quiet": quiet} {
		seedOperation(t, st, id, "transcode_media", store.ActionStatusWaitingExternal, map[string]any{}, map[string]any{}, state)
	}
	activity := transcode.WorkerActivity{Jobs: []transcode.WorkerActivityJob{
		{ID: "job-act-running", Kind: "transcode"},
		{ID: "job-act-quiet-retry-1", Kind: "transcode"},
		{ID: "job-unknown", Kind: "transcode"},
		{ID: "job-act-running", Kind: "podcast"},
	}}
	s.describeActivityJobs(&activity)
	jobs := activity.Jobs
	if jobs[0].Encoding != "x265 CRF 19" || jobs[0].Speed == nil || *jobs[0].Speed != 4.94 || jobs[0].FPS == nil || *jobs[0].FPS != 118.36 {
		t.Fatalf("a running encode lost its facts: %+v", jobs[0])
	}
	if jobs[1].Speed != nil || jobs[1].FPS != nil || jobs[1].Encoding != "" {
		t.Fatalf("a worker that went quiet still reports a speed: %+v", jobs[1])
	}
	if jobs[2].Encoding != "" || jobs[2].Speed != nil {
		t.Fatalf("a job with no action was described: %+v", jobs[2])
	}
	if jobs[3].Encoding != "" || jobs[3].Speed != nil {
		t.Fatalf("a podcast job was described: %+v", jobs[3])
	}
}
