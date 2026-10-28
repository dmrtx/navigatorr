package transcodeworker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

func TestDecideEarlySizeRefusal(t *testing.T) {
	const source = 10_000_000_000 // 10 GB; a 15% saving accepts at most 8.5 GB
	saving := &transcode.SizePolicy{MinSavingsPercent: 15}
	for _, tc := range []struct {
		name       string
		policy     *transcode.SizePolicy
		duration   float64
		written    int64
		encodedSec float64
		reason     string // "" lets the encode continue
		candidate  int64
		message    string
	}{
		{"certain overflow", saving, 7200, 8_600_000_000, 3600, "insufficient_savings", 17_200_000_000, "8.60 GB already written, over the 8.50 GB"},
		{"certain overflow before any progress", saving, 7200, 8_600_000_000, 0, "insufficient_savings", 8_600_000_000, "8.60 GB already written"},
		{"certain overflow of a size budget", &transcode.SizePolicy{MinSavingsPercent: 15, MaxCandidateBytes: 5_000_000_000}, 7200, 5_100_000_000, 6000, "size_budget_exceeded", 6_120_000_000, "already written, over the 5.00 GB"},
		{"exactly the accepted maximum", saving, 7200, 8_500_000_000, 7199, "", 0, ""},
		{"projection over the margin", saving, 7200, 3_500_000_000, 2232, "insufficient_savings", 11_290_322_580, "projected 11.29 GB after 31% of the encode (3.50 GB written), more than 25% over the 8.50 GB"},
		{"projection over the limit but under the margin", saving, 7200, 3_000_000_000, 2232, "", 0, ""},
		{"projection too early in the encode", saving, 7200, 2_400_000_000, 1440, "", 0, ""},
		{"no policy", nil, 7200, 9_000_000_000, 3600, "", 0, ""},
		{"zero duration cannot project", saving, 0, 3_500_000_000, 2232, "", 0, ""},
		{"nothing written yet", saving, 7200, 0, 3600, "", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideEarlySizeRefusal(source, tc.duration, tc.policy, tc.written, tc.encodedSec)
			if tc.reason == "" {
				if got != nil {
					t.Fatalf("the encode must continue, got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("the encode must stop")
			}
			var rejected *transcode.PolicyRejectionError
			if !errors.As(got, &rejected) || rejected.ReasonCode != tc.reason {
				t.Fatalf("refusal = %v, want reason %s", got, tc.reason)
			}
			if got.CandidateBytes != tc.candidate {
				t.Fatalf("reported candidate = %d, want %d", got.CandidateBytes, tc.candidate)
			}
			if !strings.HasPrefix(got.Error(), tc.reason+": encode stopped early: ") || !strings.Contains(got.Error(), tc.message) {
				t.Fatalf("message = %q, want it to contain %q", got.Error(), tc.message)
			}
		})
	}
}

func TestEncodeSizeWatchIsArmedOnlyForJudgeablePlans(t *testing.T) {
	job := func(mutate func(*JobRecord)) *JobRecord {
		j := &JobRecord{SourceSizeBytes: 10_000, DurationSec: 100, Plan: &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 15}}}
		if mutate != nil {
			mutate(j)
		}
		return j
	}
	if w := encodeSizeWatchFrom(withEncodeSizeWatch(context.Background(), job(nil), "/out", "/progress")); w == nil || w.sourceBytes != 10_000 || w.durationSec != 100 || w.outputPath != "/out" || w.progressPath != "/progress" {
		t.Fatalf("watch = %+v, want it armed with the job's source size and duration", w)
	}
	for name, mutate := range map[string]func(*JobRecord){
		"no size policy":    func(j *JobRecord) { j.Plan.SizePolicy = nil },
		"no source size":    func(j *JobRecord) { j.SourceSizeBytes = 0 },
		"no duration":       func(j *JobRecord) { j.DurationSec = 0 },
		"copied video":      func(j *JobRecord) { j.Plan.VideoCodec = transcode.VideoCodecCopy },
		"an invalid policy": func(j *JobRecord) { j.Plan.SizePolicy.MinSavingsPercent = 150 },
	} {
		if w := encodeSizeWatchFrom(withEncodeSizeWatch(context.Background(), job(mutate), "/out", "/progress")); w != nil {
			t.Fatalf("%s: the encode must run unwatched", name)
		}
	}
}

// fakeEncode is a process that writes an output of written bytes and an ffmpeg
// progress record at encodedSec, then runs on until killed or for runFor.
func fakeEncode(ctx context.Context, output, progress string, written int, encodedSec int, runFor string) *exec.Cmd {
	script := `head -c "$3" /dev/zero > "$1" && printf 'out_time_us=%s000000\nprogress=continue\n' "$4" > "$2" && exec sleep "$5"`
	return exec.CommandContext(ctx, "/bin/sh", "-c", script, "sh", output, progress, strconv.Itoa(written), strconv.Itoa(encodedSec), runFor)
}

func fastSizeWatch(t *testing.T) {
	t.Helper()
	was := sizeWatchInterval
	sizeWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { sizeWatchInterval = was })
}

func TestRunWatchedEncodeKillsAnEncodeThePolicyWillRefuse(t *testing.T) {
	fastSizeWatch(t)
	dir := t.TempDir()
	output, progress := filepath.Join(dir, "candidate.mkv"), filepath.Join(dir, "progress.txt")
	job := &JobRecord{SourceSizeBytes: 10_000, DurationSec: 100, Plan: &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 15}}}
	ctx := withEncodeSizeWatch(context.Background(), job, output, progress)

	// 5000 bytes at 40%: 12500 projected, more than 25% over the 8500 accepted.
	start := time.Now()
	err := runWatchedEncode(ctx, fakeEncode(context.Background(), output, progress, 5_000, 40, "30"))
	var refused *encodeSizeRefusal
	if !errors.As(err, &refused) || refused.Rejection.ReasonCode != "insufficient_savings" || refused.CandidateBytes != 12_500 {
		t.Fatalf("err = %v, want an insufficient_savings refusal projecting 12500 bytes", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the encode was not killed: it ran %s", elapsed)
	}

	// 3000 bytes at 40%: 7500 projected, within the policy; the encode finishes.
	if err := runWatchedEncode(ctx, fakeEncode(context.Background(), output, progress, 3_000, 40, "0.3")); err != nil {
		t.Fatalf("an encode on course to pass must run to the end: %v", err)
	}
	// Unwatched, the same oversized encode runs to the end.
	if err := runWatchedEncode(context.Background(), fakeEncode(context.Background(), output, progress, 9_000, 40, "0.3")); err != nil {
		t.Fatalf("an unwatched encode must run to the end: %v", err)
	}
}

// A refusal during the encode ends the job exactly as the post-encode size
// check does: the reason code, policy_rejected, the reported size and the
// partial candidate removed. It never starts validating, which is what tells
// the coordinator the size was projected.
func TestEarlySizeRefusalEndsTheJobAsAPostEncodeRefusal(t *testing.T) {
	fastSizeWatch(t)
	dir := t.TempDir()
	source := ioOptWriteFile(t, filepath.Join(dir, "source.mkv"), string(make([]byte, 10_000)))
	candidate := filepath.Join(dir, "candidate.mkv")
	cfg := pr6b2Config(dir, nil)
	w := NewWorker(cfg)
	newStubSpawn().install(w)
	rec := &pr6b2Recorder{probeDur: 100}
	rec.install(w)
	watched := false
	w.SetRunFFmpeg(func(ctx context.Context, plan *ExecutionPlan, job *JobRecord, input, output, progress, log string) error {
		watched = encodeSizeWatchFrom(ctx) != nil
		return runWatchedEncode(ctx, fakeEncode(ctx, output, progress, 5_000, 40, "30"))
	})
	plan := ioOptPlan(t)
	plan.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: 15}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	if _, err := w.Submit(context.Background(), SubmitRequest{ID: "early", SourcePath: source, CandidatePath: candidate, Plan: plan}, "unused", ""); err != nil {
		t.Fatal(err)
	}
	err := pr6b2Run(t, w, "early")
	job := pr6b1LoadJob(t, cfg.StateDir, "early")
	if err == nil || !watched {
		t.Fatalf("the watched encode must be refused: err=%v watched=%v", err, watched)
	}
	if job.Status != "failed" || job.ReasonCode != "insufficient_savings" || job.FailureClassification != "policy_rejected" || job.CandidateSizeBytes != 12_500 {
		t.Fatalf("job = %+v, want a policy_rejected insufficient_savings refusal reporting the projected size", job)
	}
	if !strings.Contains(job.Error, "projected") || !strings.Contains(job.Error, "after 40% of the encode") {
		t.Fatalf("error = %q, want it to say the size was projected", job.Error)
	}
	if job.EncodeFinishedAt.IsZero() || !job.ValidationStartedAt.IsZero() {
		t.Fatalf("an early refusal finishes the encode without validating: %+v", job.JobTelemetry)
	}
	if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
		t.Fatalf("the partial candidate must be removed: %v", err)
	}
	if _, err := os.Lstat(source); err != nil {
		t.Fatalf("the source must be kept: %v", err)
	}
}
