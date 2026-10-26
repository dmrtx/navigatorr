package maintenanceui

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
)

var workerJobRetrySuffix = regexp.MustCompile(`-retry-\d+$`)

// stepSuffix is the "(step 1 of 3)" a recipe's name ends with; a slot shows
// the encode itself, and the step is the batch row's to tell.
var stepSuffix = regexp.MustCompile(` \(step \d+ of \d+\)$`)

// actionOfWorkerJob is the action a worker job runs for: "job-<action>", or
// "job-<action>-retry-N" after a manual retry.
func actionOfWorkerJob(jobID string) string {
	id, ok := strings.CutPrefix(jobID, "job-")
	if !ok {
		return ""
	}
	return workerJobRetrySuffix.ReplaceAllString(id, "")
}

// describeActivityJobs adds to each running job what only its action knows: the
// encode it was given and how fast the encoder is going. A job without an action
// (a worker started by hand, a podcast) is left as the worker reported it.
func (s *Server) describeActivityJobs(activity *transcode.WorkerActivity) {
	st := s.engine.Deps().Store
	if st == nil {
		return
	}
	for i := range activity.Jobs {
		job := &activity.Jobs[i]
		id := actionOfWorkerJob(job.ID)
		if job.Kind == "podcast" || id == "" {
			continue
		}
		inst, err := st.GetActionInstanceIfExists(id)
		if err != nil || inst == nil {
			continue
		}
		job.ActionID = id
		var state map[string]any
		if json.Unmarshal([]byte(inst.StateJSON), &state) != nil {
			continue
		}
		if step, ok := action.RecipeStep(state); ok {
			job.Encoding = stepSuffix.ReplaceAllString(step, "")
		}
		if stale, _ := state["progress_is_stale"].(bool); stale {
			continue
		}
		if speed, ok := state["speed"].(float64); ok && speed > 0 {
			job.Speed = &speed
		}
		if fps, ok := state["fps"].(float64); ok && fps > 0 {
			job.FPS = &fps
		}
	}
}
