package transcodeworker

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/jakenesler/navigatorr/transcode"
)

type encoderPhaseContextKey struct{}

func reportEncoderPhase(ctx context.Context, phase string) error {
	if report, ok := ctx.Value(encoderPhaseContextKey{}).(func(string) error); ok {
		return report(phase)
	}
	return nil
}

// runEncoderCommand runs an encode once. An x265 ABR plan (Plan.X265) is
// encoded in a single pass at its bitrate even when it says two_pass: plans
// keep the flag so older workers and frozen policies still accept them, but an
// analysis pass costs a large part of a whole encode, runs without progress,
// and buys little. The encode is size-watched (see runWatchedEncode).
func runEncoderCommand(ctx context.Context, cmd *exec.Cmd, plan *transcode.Plan) error {
	if plan.X265 != nil {
		if err := plan.X265.Validate(plan.VideoCodec, plan.Quality); err != nil {
			return err
		}
		if err := reportEncoderPhase(ctx, "encoding"); err != nil {
			return err
		}
	}
	return runWatchedEncode(ctx, cmd)
}

// withX265Pass adds an x265 pass to -x265-params; only the worker's two-pass
// self-check uses it.
func withX265Pass(args []string, pass int) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-x265-params" {
			out[i+1] += fmt.Sprintf(":pass=%d:stats=x265.log", pass)
			return out
		}
	}
	return out
}
