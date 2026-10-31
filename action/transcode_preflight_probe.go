package action

import (
	"context"
	"fmt"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// preflightProbeAttempts is how many times preflight inspects the source when
// the first inspection fails for a reason another attempt could change. The
// source is on network storage, where one stalled read says little about the
// file; a second look separates a hiccup from a file that cannot be read.
const preflightProbeAttempts = 2

// preflightProbeBackoff and preflightInspect are variables so tests can
// shorten the wait and script the inspection.
var (
	preflightProbeBackoff = 3 * time.Second
	preflightInspect      = mediainspect.InspectDetailed
	preflightVideoSeconds = mediainspect.MeasureVideoSeconds
)

// inspectSourceForPreflight inspects the original. A failure that could be
// transient (a timeout, or storage I/O errors in ffprobe's stderr) is tried
// once more after a short backoff; every other failure, a second failure
// included, is returned at once with its typed cause. Nothing here relaxes the
// fail-closed rule: an uninspected source never reaches planning.
func (e *Engine) inspectSourceForPreflight(ctx context.Context, ec *ExecutionContext, path string) (mediainspect.DetailedReport, error) {
	var (
		rep     mediainspect.DetailedReport
		err     error
		attempt int
	)
	for attempt = 1; attempt <= preflightProbeAttempts; attempt++ {
		rep, err = preflightInspect(ctx, e.deps.Ffprobe, path)
		if err == nil && rep.Probed {
			measureVideoSeconds(ctx, e.deps.Ffprobe, path, &rep)
			return rep, nil
		}
		if err == nil {
			// A report that claims no failure and no inspection cannot be trusted.
			return rep, fmt.Errorf("ffprobe returned no inspection")
		}
		probeErr, typed := mediainspect.AsProbeError(err)
		if !typed || !probeErr.Transient() || attempt == preflightProbeAttempts || ctx.Err() != nil {
			break
		}
		appendFailureHistory(ec, "preflight_probe", "probe_"+string(probeErr.Kind), fmt.Sprintf("attempt %d of %d: %v", attempt, preflightProbeAttempts, probeErr))
		timer := time.NewTimer(preflightProbeBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return rep, err
		case <-timer.C:
		}
	}
	if attempt > preflightProbeAttempts {
		attempt = preflightProbeAttempts
	}
	if attempt > 1 {
		err = fmt.Errorf("%w (after %d attempts)", err, attempt)
	}
	return rep, err
}

// measureVideoSeconds records how long the programme video runs, which the
// quality windows are planned on when the container runs longer
// (DetailedReport.WindowSeconds). It is best effort: a video that cannot be
// measured keeps the container's duration, as every source did before.
func measureVideoSeconds(ctx context.Context, ffprobe, path string, rep *mediainspect.DetailedReport) {
	video := rep.MainVideo()
	if len(video) == 0 {
		return
	}
	if seconds, err := preflightVideoSeconds(ctx, ffprobe, path, video[0].Index, rep.DurationSec); err == nil && seconds > 0 {
		rep.VideoSeconds = seconds
	}
}
