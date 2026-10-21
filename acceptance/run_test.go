package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
	"github.com/jakenesler/navigatorr/transcode"
)

// result is a finished transcode_media action with the parts the assertions
// and the report read already decoded.
type result struct {
	res      *action.ActionResult
	goal     *action.TranscodeExplanation
	evidence *transcode.FinalQualityEvidence
	budget   *action.SizeBudgetRecord
	wall     time.Duration
}

// decode round-trips a loosely typed action output into a struct.
func decode(from any, into any) bool {
	if from == nil {
		return false
	}
	b, err := json.Marshal(from)
	return err == nil && json.Unmarshal(b, into) == nil
}

// pick reads key from the action's outputs, then its state.
func pick(res *action.ActionResult, key string) any {
	if v := res.Outputs[key]; v != nil {
		return v
	}
	return res.State[key]
}

// settled reports an action that nothing more will happen to without a person
// or a worker event. Decision waits are final for these cases: a transcode
// that needs attention is a failure of the pipeline to finish by itself.
func settled(status string) bool {
	switch status {
	case action.StatusRunning, action.StatusPending, action.StatusWaitingExternal:
		return false
	}
	return true
}

// await polls the action until it settles or the deadline passes, logging
// the worker phase as it changes so a long run shows it is alive.
func (s *suite) await(t *testing.T, id string, timeout time.Duration) *action.ActionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var last string
	for {
		res, err := s.engine.Status(ctx, id)
		if err != nil {
			t.Fatalf("reading action %s: %v", id, err)
		}
		if settled(res.Status) {
			return res
		}
		if p := progressLine(res); p != last {
			last = p
			t.Logf("  %s", p)
		}
		select {
		case <-ctx.Done():
			_, _ = s.engine.Cancel(context.Background(), id, "acceptance timeout")
			t.Fatalf("timed out after %s waiting for %s (%s)\nworker log tail:\n%s", timeout, id, last, tail(s.workerLog.String(), 3000))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func progressLine(res *action.ActionResult) string {
	phase := getStr(res.State, "transcode_phase")
	if phase == "" {
		if wp, ok := res.State["work_progress"].(map[string]any); ok {
			phase = getStr(wp, "phase")
		}
	}
	return fmt.Sprintf("%s step %d/%d %s", res.Status, res.CurrentStep+1, res.TotalSteps, phase)
}

func getStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// transcode runs one transcode_media request to its end.
func (s *suite) transcode(t *testing.T, path string, inputs map[string]any, timeout time.Duration) *result {
	t.Helper()
	in := map[string]any{"path": path}
	for k, v := range inputs {
		in[k] = v
	}
	start := time.Now()
	r, err := s.engine.Run(context.Background(), "transcode_media", in)
	if err != nil {
		t.Fatalf("starting transcode_media: %v", err)
	}
	res := s.await(t, r.ID, timeout)
	out := &result{res: res, goal: action.ExplainTranscode(res), wall: time.Since(start)}
	var ev transcode.FinalQualityEvidence
	if decode(pick(res, "quality_evidence"), &ev) && ev.Verdict != "" {
		out.evidence = &ev
	}
	var b action.SizeBudgetRecord
	if decode(pick(res, "size_budget"), &b) && b.BudgetBytes > 0 {
		out.budget = &b
	}
	return out
}

// reason is the skip or rejection code of the action, if any.
func (r *result) reason() string {
	if c := getStr(r.res.State, "reason_code"); c != "" {
		return c
	}
	return getStr(r.res.Outputs, "reason_code")
}

// skipReason is why the frozen size budget was not used, if it was not.
func (r *result) skipReason() string {
	s, _ := pick(r.res, "size_budget_skip_reason").(string)
	return s
}

func (r *result) candidate() string {
	return getStr(r.res.Outputs, "candidate_path")
}

// describe is what a failing assertion prints about the action.
func (r *result) describe() string {
	msg := fmt.Sprintf("status=%s error=%q outcome=%q reason=%q", r.res.Status, r.res.Error, r.outcome(), r.reason())
	if r.res.WaitingReason != "" {
		msg += fmt.Sprintf(" waiting=%q", r.res.WaitingReason)
	}
	if r.goal != nil {
		msg += fmt.Sprintf(" message=%q", r.goal.Message)
	}
	if r.skipReason() != "" {
		msg += fmt.Sprintf(" size_budget_skip=%q", r.skipReason())
	}
	if r.budget != nil {
		msg += fmt.Sprintf(" size_budget.message=%q", r.budget.Message)
	}
	return msg
}

func (r *result) outcome() string {
	if r.goal == nil {
		return ""
	}
	return r.goal.Outcome
}

func (s *suite) candidatesDirName() string { return ".navigatorr-candidates" }

// within reports whether path lies under dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && !strings.HasPrefix(rel, "..")
}

func pickStr(res *action.ActionResult, key string) string {
	s, _ := pick(res, key).(string)
	return s
}
