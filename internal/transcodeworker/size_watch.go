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

// sizeProjectionRule says when a plan's size is projected and how far over the
// accepted maximum the projection must land before the encode stops. A
// projection assumes the rest of the file costs what the encoded part did, and
// it rarely does: complexity varies along a film, and credits are cheap. A
// false stop throws away a candidate that would have passed (and on the
// quality ladder ends the goal), while a missed one only costs the time an
// encode took before this watch existed, so the rules are generous. The
// projection is already biased low, which is the safe direction: x265's
// lookahead and the muxer's buffering keep the bytes written behind ffmpeg's
// out_time.
type sizeProjectionRule struct {
	// minFraction is how much of the file must be encoded before its size is
	// projected at all: earlier, a few minutes of unusual content (an intro,
	// a dark opening) decide the whole projection.
	minFraction float64
	// margin is how far the projection must exceed the accepted maximum.
	margin float64
}

var (
	// rateControlledProjection is for plans with an average bitrate target,
	// whose rate control holds the bitrate near the target along the file.
	rateControlledProjection = sizeProjectionRule{minFraction: 0.25, margin: 0.25}
	// qualityModeProjection is for plans encoded at a constant quality (CRF,
	// VideoToolbox -q:v): their bitrate follows the content, and a grainy
	// quarter can run at 1.5 to 2 times the film's average.
	qualityModeProjection = sizeProjectionRule{minFraction: 0.5, margin: 0.5}
)

// sizeWatchPoint is the output's size when ffmpeg reported encodedSec.
type sizeWatchPoint struct {
	bytes      int64
	encodedSec float64
}

// encodeSizeWatch is what the watch needs to know about one encode: the frozen
// policy and source size the final check would use, the source duration that
// turns out_time into a fraction, and the files ffmpeg writes.
type encodeSizeWatch struct {
	sourceBytes  int64
	durationSec  float64
	policy       *transcode.SizePolicy
	rule         sizeProjectionRule
	outputPath   string
	progressPath string
	// base is the first point read with progress; nil until then. The bytes
	// written before it (Matroska writes attachments such as fonts ahead of
	// the first frame) are not scaled up with the encoded part.
	base *sizeWatchPoint
	// reserveBytes is the room the node admitted the job for (0: none). An
	// output past it is not compressing.
	reserveBytes int64
	// floor reads the node's free disk and the hard floor under it (ok false
	// when unknown); nil when the node keeps no floor.
	floor func() (free, hard int64, ok bool)
}

// diskFloorStop ends an encode whose disk fell under the hard floor. The job is
// not at fault: it goes back to the coordinator's queue.
type diskFloorStop struct{ free, hard int64 }

func (e *diskFloorStop) Error() string {
	return fmt.Sprintf("disk_floor: free disk fell to %s, under the %s hard floor; the encode stopped and the job goes back to the queue", formatSizeWatchBytes(e.free), formatSizeWatchBytes(e.hard))
}

type sizeWatchContextKey struct{}

// withEncodeSizeWatch arms the watch for the encode run under ctx. Plans the
// watch cannot judge (no size policy, unknown source size or duration) and
// copied video, whose encode takes minutes, run unwatched.
func withEncodeSizeWatch(ctx context.Context, job *JobRecord, outputPath, progressPath string) context.Context {
	if job == nil || job.Plan == nil || job.SourceSizeBytes <= 0 || job.DurationSec <= 0 {
		return ctx
	}
	if strings.EqualFold(strings.TrimSpace(job.Plan.VideoCodec), transcode.VideoCodecCopy) {
		return ctx
	}
	policy := job.Plan.SizePolicy
	if _, ok := transcode.MaxAcceptedCandidateBytes(job.SourceSizeBytes, policy); !ok {
		policy = nil
	}
	if policy == nil && job.RetainReserveBytes <= 0 {
		return ctx
	}
	rule := qualityModeProjection
	if job.Plan.AverageBitrateKbps > 0 {
		rule = rateControlledProjection
	}
	return context.WithValue(ctx, sizeWatchContextKey{}, &encodeSizeWatch{
		sourceBytes:  job.SourceSizeBytes,
		durationSec:  job.DurationSec,
		policy:       policy,
		rule:         rule,
		outputPath:   outputPath,
		progressPath: progressPath,
		reserveBytes: job.RetainReserveBytes,
	})
}

func encodeSizeWatchFrom(ctx context.Context) *encodeSizeWatch {
	w, _ := ctx.Value(sizeWatchContextKey{}).(*encodeSizeWatch)
	return w
}

// encodeSizeRefusal is a size policy refusal made while the encode ran. It
// unwraps to the PolicyRejectionError the finished file would have met, so the
// job is refused as a post-encode check would refuse it. CandidateBytes is
// the reported candidate size: the projected final size once the plan's rule
// allows a projection, else the bytes already written.
type encodeSizeRefusal struct {
	Rejection      *transcode.PolicyRejectionError
	CandidateBytes int64
}

func (e *encodeSizeRefusal) Error() string { return e.Rejection.Error() }
func (e *encodeSizeRefusal) Unwrap() error { return e.Rejection }

// decideEarlySizeRefusal decides whether an encode in progress can stop:
// written is the output's size now and encodedSec how much of the source's
// durationSec ffmpeg reports encoded (out_time; 0 when unknown). base is the
// first point read with progress (nil when none yet). It returns nil to let
// the encode continue.
//
//   - Certain: more bytes are already written than the policy accepts. A file
//     only grows while it is encoded, so the finished one would be refused.
//   - Projected: from rule.minFraction of the duration on, the final size
//     exceeds the accepted maximum by more than rule.margin. The final size is
//     what is written plus the rest of the duration at the rate the file grew
//     since base.
func decideEarlySizeRefusal(sourceBytes int64, durationSec float64, policy *transcode.SizePolicy, rule sizeProjectionRule, base *sizeWatchPoint, written int64, encodedSec float64) *encodeSizeRefusal {
	limit, ok := transcode.MaxAcceptedCandidateBytes(sourceBytes, policy)
	if !ok || written <= 0 {
		return nil
	}
	reported, fraction, projecting := projectSize(durationSec, base, written, encodedSec, rule)
	accepts := fmt.Sprintf("the %s the frozen size policy accepts", formatSizeWatchBytes(limit))
	projection := fmt.Sprintf("projected %s after %d%% of the encode", formatSizeWatchBytes(reported), int(math.Floor(fraction*100)))
	var detail string
	switch {
	case written > limit && projecting:
		detail = fmt.Sprintf("%s; %s already written, over %s", projection, formatSizeWatchBytes(written), accepts)
	case written > limit:
		detail = fmt.Sprintf("%s already written, over %s", formatSizeWatchBytes(written), accepts)
	case projecting && float64(reported) > float64(limit)*(1+rule.margin):
		detail = fmt.Sprintf("%s (%s written), more than %.0f%% over %s", projection, formatSizeWatchBytes(written), rule.margin*100, accepts)
	default:
		return nil
	}
	// The reason is the one the reported file would be refused for, from the
	// same check the finished file would meet.
	var rejected *transcode.PolicyRejectionError
	if !errors.As(transcode.ValidateFinalSize(sourceBytes, reported, policy), &rejected) {
		return nil
	}
	return &encodeSizeRefusal{
		Rejection:      &transcode.PolicyRejectionError{ReasonCode: rejected.ReasonCode, Message: "encode stopped early: " + detail},
		CandidateBytes: reported,
	}
}

// projectSize is the size the output is reported at: what is written, or the
// projected final size once the rule allows a projection.
func projectSize(durationSec float64, base *sizeWatchPoint, written int64, encodedSec float64, rule sizeProjectionRule) (reported int64, fraction float64, projecting bool) {
	if durationSec > 0 && encodedSec > 0 {
		fraction = min(encodedSec/durationSec, 1)
	}
	reported = written
	if fraction >= rule.minFraction && base != nil && encodedSec > base.encodedSec && written >= base.bytes {
		rate := float64(written-base.bytes) / (encodedSec - base.encodedSec)
		reported, projecting = max(written, written+int64(rate*max(durationSec-encodedSec, 0))), true
	}
	return reported, fraction, projecting
}

// decideReserveRefusal stops an encode whose output passes the room the node
// admitted it for, or is projected to by more than the rule's margin: it is not
// compressing. It says how big the output is against the source.
func decideReserveRefusal(sourceBytes, reserve int64, durationSec float64, rule sizeProjectionRule, base *sizeWatchPoint, written int64, encodedSec float64) *encodeSizeRefusal {
	if reserve <= 0 || written <= 0 {
		return nil
	}
	reported, fraction, projecting := projectSize(durationSec, base, written, encodedSec, rule)
	if written <= reserve && !(projecting && float64(reported) > float64(reserve)*(1+rule.margin)) {
		return nil
	}
	pct := func(n int64) int { return int(math.Round(float64(n) * 100 / float64(max(sourceBytes, 1)))) }
	detail := fmt.Sprintf("not compressing: the output is %s (%d%% of the %s source) after %d%% of the encode, past the %s (%d%% of the source) this job was given room for",
		formatSizeWatchBytes(reported), pct(reported), formatSizeWatchBytes(sourceBytes), int(math.Floor(fraction*100)), formatSizeWatchBytes(reserve), pct(reserve))
	return &encodeSizeRefusal{
		Rejection:      &transcode.PolicyRejectionError{ReasonCode: "insufficient_savings", Message: "encode stopped early: " + detail},
		CandidateBytes: reported,
	}
}

func formatSizeWatchBytes(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%.0f MB", float64(n)/1e6)
}

// check reads the encode's current size and progress and decides on them.
func (w *encodeSizeWatch) check() error {
	if w.floor != nil {
		if free, hard, ok := w.floor(); ok && free < hard {
			return &diskFloorStop{free: free, hard: hard}
		}
	}
	if w.policy == nil && w.reserveBytes <= 0 {
		return nil
	}
	info, err := os.Stat(w.outputPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	var encodedSec float64
	if m := ParseProgress(w.progressPath, w.durationSec); m.Valid {
		encodedSec = m.OutTimeSec
		if w.base == nil {
			w.base = &sizeWatchPoint{bytes: info.Size(), encodedSec: encodedSec}
		}
	}
	var r *encodeSizeRefusal
	if w.policy != nil {
		r = decideEarlySizeRefusal(w.sourceBytes, w.durationSec, w.policy, w.rule, w.base, info.Size(), encodedSec)
	}
	if r == nil {
		r = decideReserveRefusal(w.sourceBytes, w.reserveBytes, w.durationSec, w.rule, w.base, info.Size(), encodedSec)
	}
	if r == nil {
		return nil
	}
	return r
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
	untrack, err := startGated(ctx, cmd)
	if err != nil {
		return err
	}
	var (
		refusal error
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
	err = cmd.Wait()
	untrack()
	close(done)
	wg.Wait()
	chargeCPU(ctx, cmd)
	if refusal != nil && err != nil && ctx.Err() == nil {
		return refusal
	}
	return err
}
