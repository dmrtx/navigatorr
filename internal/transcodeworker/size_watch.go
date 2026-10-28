package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// The size policy is otherwise checked only once the whole file is encoded
// (ValidateFinalSize after the encode), which can be hours after the file was
// already bound to miss it. While the encode runs the worker watches the
// output's size against the encode's progress and stops it when the file
// cannot pass, or almost certainly will not. The job then ends exactly as a
// post-encode size refusal does: the same PolicyRejectionError reason code,
// failure class policy_rejected, and the partial candidate removed. Such a job
// never started validating, which is how the coordinator tells a projected
// size from a finished file's.

// sizeWatchInterval is how often the encode's size and progress are read.
// Each read is a stat and a bounded tail of progress.txt, and a few seconds
// late only costs a few seconds of encoding.
var sizeWatchInterval = 5 * time.Second

const (
	// earlySizeProjectionMinFraction is how much of the file must be encoded
	// before its size is projected at all: earlier, a few minutes of unusual
	// content (an intro, a dark opening) decide the whole projection.
	earlySizeProjectionMinFraction = 0.25
	// earlySizeProjectionMargin is how far the projected final size must
	// exceed the largest size the policy accepts (25%) before the encode is
	// stopped. A projection assumes the rest of the file costs what the
	// encoded part did, and it rarely does: complexity varies along a film,
	// and credits are cheap. A false stop throws away a candidate that would
	// have passed, while a missed one only costs the time an encode took
	// before this watch existed, so the margin is generous. The projection is
	// already biased low, which is the safe direction: x265's lookahead and the
	// muxer's buffering keep the bytes written behind ffmpeg's out_time.
	earlySizeProjectionMargin = 0.25
)

// encodeSizeWatch is what the watch needs to know about one encode: the frozen
// policy and source size the final check would use, the source duration that
// turns out_time into a fraction, and the files ffmpeg writes.
type encodeSizeWatch struct {
	sourceBytes  int64
	durationSec  float64
	policy       *transcode.SizePolicy
	outputPath   string
	progressPath string
}

type sizeWatchContextKey struct{}

// withEncodeSizeWatch arms the watch for the encode run under ctx. Plans the
// watch cannot judge (no size policy, unknown source size or duration) and
// copied video, whose encode takes minutes, run unwatched.
func withEncodeSizeWatch(ctx context.Context, job *JobRecord, outputPath, progressPath string) context.Context {
	if job == nil || job.Plan == nil || job.Plan.SizePolicy == nil || job.SourceSizeBytes <= 0 || job.DurationSec <= 0 {
		return ctx
	}
	if strings.EqualFold(strings.TrimSpace(job.Plan.VideoCodec), transcode.VideoCodecCopy) {
		return ctx
	}
	if _, ok := transcode.MaxAcceptedCandidateBytes(job.SourceSizeBytes, job.Plan.SizePolicy); !ok {
		return ctx
	}
	return context.WithValue(ctx, sizeWatchContextKey{}, &encodeSizeWatch{
		sourceBytes:  job.SourceSizeBytes,
		durationSec:  job.DurationSec,
		policy:       job.Plan.SizePolicy,
		outputPath:   outputPath,
		progressPath: progressPath,
	})
}

func encodeSizeWatchFrom(ctx context.Context) *encodeSizeWatch {
	w, _ := ctx.Value(sizeWatchContextKey{}).(*encodeSizeWatch)
	return w
}

// encodeSizeRefusal is a size policy refusal made while the encode ran. It
// unwraps to the PolicyRejectionError the finished file would have met, so the
// job is refused as a post-encode check would refuse it. CandidateBytes is
// the reported candidate size: the projected final size once progress is
// known, else the bytes already written.
type encodeSizeRefusal struct {
	Rejection      *transcode.PolicyRejectionError
	CandidateBytes int64
}

func (e *encodeSizeRefusal) Error() string { return e.Rejection.Error() }
func (e *encodeSizeRefusal) Unwrap() error { return e.Rejection }

// decideEarlySizeRefusal decides whether an encode in progress can stop:
// written is the output's size now and encodedSec how much of the source's
// durationSec ffmpeg reports encoded (out_time; 0 when unknown). It returns
// nil to let the encode continue.
//
//   - Certain: more bytes are already written than the policy accepts. A file
//     only grows while it is encoded, so the finished one would be refused.
//   - Projected: from earlySizeProjectionMinFraction of the duration on, the
//     final size written/fraction exceeds the accepted maximum by more than
//     earlySizeProjectionMargin.
func decideEarlySizeRefusal(sourceBytes int64, durationSec float64, policy *transcode.SizePolicy, written int64, encodedSec float64) *encodeSizeRefusal {
	limit, ok := transcode.MaxAcceptedCandidateBytes(sourceBytes, policy)
	if !ok || written <= 0 {
		return nil
	}
	var fraction float64
	if durationSec > 0 && encodedSec > 0 {
		fraction = min(encodedSec/durationSec, 1)
	}
	projected := written
	if fraction > 0 {
		projected = max(written, int64(float64(written)/fraction))
	}
	accepts := fmt.Sprintf("the %s the frozen size policy accepts", formatSizeWatchBytes(limit))
	projection := fmt.Sprintf("projected %s after %d%% of the encode", formatSizeWatchBytes(projected), int(math.Floor(fraction*100)))
	var detail string
	switch {
	case written > limit && fraction > 0:
		detail = fmt.Sprintf("%s; %s already written, over %s", projection, formatSizeWatchBytes(written), accepts)
	case written > limit:
		detail = fmt.Sprintf("%s already written, over %s", formatSizeWatchBytes(written), accepts)
	case fraction >= earlySizeProjectionMinFraction && float64(projected) > float64(limit)*(1+earlySizeProjectionMargin):
		detail = fmt.Sprintf("%s (%s written), more than %.0f%% over %s", projection, formatSizeWatchBytes(written), earlySizeProjectionMargin*100, accepts)
	default:
		return nil
	}
	// The reason is the one the projected file would be refused for, from the
	// same check the finished file would meet.
	var rejected *transcode.PolicyRejectionError
	if !errors.As(transcode.ValidateFinalSize(sourceBytes, projected, policy), &rejected) {
		return nil
	}
	return &encodeSizeRefusal{
		Rejection:      &transcode.PolicyRejectionError{ReasonCode: rejected.ReasonCode, Message: "encode stopped early: " + detail},
		CandidateBytes: projected,
	}
}

func formatSizeWatchBytes(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%.0f MB", float64(n)/1e6)
}

// check reads the encode's current size and progress and decides on them.
func (w *encodeSizeWatch) check() *encodeSizeRefusal {
	info, err := os.Stat(w.outputPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	var encodedSec float64
	if m := ParseProgress(w.progressPath, w.durationSec); m.Valid {
		encodedSec = m.OutTimeSec
	}
	return decideEarlySizeRefusal(w.sourceBytes, w.durationSec, w.policy, info.Size(), encodedSec)
}

// runWatchedEncode runs the encode cmd like runCharged, watching its size when
// ctx carries an encodeSizeWatch. A refusal kills ffmpeg the way cancelling
// ctx would (cmd.Cancel, which for a process-group command kills the group)
// and returns the refusal. An encode that finishes on its own before the kill
// lands keeps its file for the post-encode check.
func runWatchedEncode(ctx context.Context, cmd *exec.Cmd) error {
	watch := encodeSizeWatchFrom(ctx)
	if watch == nil {
		return runCharged(ctx, cmd)
	}
	if cmd.WaitDelay == 0 {
		// Bounds Wait when a killed ffmpeg leaves its output pipes open.
		cmd.WaitDelay = 2 * time.Second
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var (
		refusal *encodeSizeRefusal
		wg      sync.WaitGroup
	)
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(sizeWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if r := watch.check(); r != nil {
					refusal = r
					if cmd.Cancel != nil {
						_ = cmd.Cancel()
					} else {
						_ = cmd.Process.Kill()
					}
					return
				}
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	wg.Wait()
	chargeCPU(ctx, cmd)
	if refusal != nil && err != nil && ctx.Err() == nil {
		return refusal
	}
	return err
}
