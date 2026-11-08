package action

import (
	"context"
	"encoding/json"
	"strings"
)

// earlyPreflightKey marks an action whose early check already ran while it
// waited for a worker, so it reads the file once per wait, not on every poll.
// The real preflight clears it, so a retry checks again.
const earlyPreflightKey = "early_preflight_checked"

type earlyPreflightCtxKey struct{}

func withEarlyPreflight(ctx context.Context) context.Context {
	return context.WithValue(ctx, earlyPreflightCtxKey{}, true)
}

func isEarlyPreflight(ctx context.Context) bool {
	early, _ := ctx.Value(earlyPreflightCtxKey{}).(bool)
	return early
}

// earlyPreflight runs the preflight of a conversion that is waiting for a
// worker, on a copy of its state, without a worker: the probe, track picks,
// mode rules and recipe resolve need none. Worker capabilities are taken as
// the most permissive answer and the source is not hashed, so the check can
// only find what no worker would change. It decides the action only then:
// a deterministic rejection fails it, and a file its goal skips is skipped
// now. Anything else, including a probe that failed and could pass later,
// leaves the action waiting for the real preflight on its worker.
func (e *Engine) earlyPreflight(ctx context.Context, ec *ExecutionContext) (StepResult, bool) {
	if ec.ActionName != "transcode_media" || getBool(ec.State, earlyPreflightKey) {
		return StepResult{}, false
	}
	check, err := cloneExecutionContext(ec)
	if err != nil {
		return StepResult{}, false
	}
	ec.State[earlyPreflightKey] = true
	res, err := e.stepTranscodePreflight(withEarlyPreflight(ctx), check)
	switch {
	case err != nil || ctx.Err() != nil:
		return StepResult{}, false
	case res.Status == StepFailed && earlyRejection(res.Error):
		return res, true
	case res.Status == StepCompleted && getBool(check.State, "skip_transcode"):
		for k := range ec.State {
			delete(ec.State, k)
		}
		for k, v := range check.State {
			ec.State[k] = v
		}
		ec.State[earlyPreflightKey] = true
		return res, true
	}
	return StepResult{}, false
}

// earlyRejection is a preflight failure that comes from the file and its goal
// alone: a rejected source class or a recipe that cannot be resolved for it.
func earlyRejection(msg string) bool {
	if _, ok := PermanentSourceRejection(msg); ok {
		return true
	}
	return strings.HasPrefix(msg, "resolving transcode profile ") || strings.HasPrefix(msg, "resolving ephemeral profile_config: ")
}

func cloneExecutionContext(ec *ExecutionContext) (*ExecutionContext, error) {
	data, err := json.Marshal(ec)
	if err != nil {
		return nil, err
	}
	check := &ExecutionContext{}
	if err := json.Unmarshal(data, check); err != nil {
		return nil, err
	}
	check.Engine = ec.Engine
	for _, m := range []*map[string]any{&check.Inputs, &check.State, &check.Outputs} {
		if *m == nil {
			*m = map[string]any{}
		}
	}
	return check, nil
}
