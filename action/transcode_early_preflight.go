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
	res, err := e.stepTranscodePreflight(withEarlyPreflight(ctx), check)
	if err != nil || ctx.Err() != nil {
		// Interrupted: the next poll checks again.
		return StepResult{}, false
	}
	switch {
	case res.Status == StepFailed && earlyRejection(res.Error):
		// The failure is the action's; a retry checks again.
		delete(ec.State, earlyPreflightKey)
		return res, true
	case res.Status == StepCompleted && getBool(check.State, "skip_transcode") && earlySkipReasons[getString(check.State, "reason_code")]:
		// The goal may have rewritten the request (Save space routes HDR to
		// compact), so the skip keeps the inputs it was decided with.
		replaceMap(ec.Inputs, check.Inputs)
		replaceMap(ec.State, check.State)
		ec.State[earlyPreflightKey] = true
		ec.State[earlySkipOutputsKey] = res.Outputs
		return res, true
	}
	ec.State[earlyPreflightKey] = true
	return StepResult{}, false
}

// earlySkipReasons are the skips no worker changes: the file's dynamic range
// or video shape against its goal. A skip for a measurement the check could
// not make (compact frame evidence) or for a worker capability (HFR quality)
// is left to the real preflight.
var earlySkipReasons = map[string]bool{
	"hdr_requires_compact":             true,
	"hdr_single_video_stream_required": true,
	"unsupported_source":               true,
}

// earlySkipOutputsKey keeps the outputs of a skip the early check decided,
// for a preflight step that runs again before the action moves on.
const earlySkipOutputsKey = "early_skip_outputs"

// earlySkipped is a conversion its early check skipped: it sends no work, so
// it needs no worker, and its preflight only repeats the skip.
func earlySkipped(state map[string]any) bool {
	return getBool(state, "skip_transcode") && getBool(state, earlyPreflightKey) && state[earlySkipOutputsKey] != nil && !holdsWorkerJob(state)
}

func replaceMap(dst, src map[string]any) {
	for k := range dst {
		delete(dst, k)
	}
	for k, v := range src {
		dst[k] = v
	}
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
