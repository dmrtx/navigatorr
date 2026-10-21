package acceptance

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// kase is one acceptance case: a subtest that records its row for the report
// and keeps the first assertion failure as the row's reason.
type kase struct {
	*testing.T
	s   *suite
	row *caseReport
}

// Fatalf records the failure for the report, then fails the subtest.
func (k *kase) Fatalf(format string, args ...any) {
	k.T.Helper()
	if k.row.Failure == "" {
		k.row.Failure = fmt.Sprintf(format, args...)
	}
	k.T.Fatalf(format, args...)
}

func (k *kase) require(ok bool, format string, args ...any) {
	k.T.Helper()
	if !ok {
		k.Fatalf(format, args...)
	}
}

// needCaps fails (or skips on a development machine) when the worker lacks
// something this case drives.
func (k *kase) needCaps(needs ...string) {
	k.T.Helper()
	caps := k.s.caps
	for _, need := range needs {
		ok := false
		switch need {
		case "libx265":
			ok = caps.Encoders["libx265"]
		case "libx264":
			ok = caps.Encoders["libx264"]
		case "aac":
			ok = caps.Encoders["aac"]
		case "flac":
			ok = caps.Encoders["flac"]
		case "ac3":
			ok = caps.Encoders["ac3"]
		case "hevc_videotoolbox":
			ok = caps.Encoders["hevc_videotoolbox"] && caps.HardwareSearch
		case "vmaf":
			ok = caps.Quality != nil && caps.Quality.ProbeError == "" && caps.Quality.Models["v1_1080p_3h"].Available
		case "lut3d":
			ok = caps.CompactTranscode && caps.Filters["lut3d"]
		default:
			k.T.Fatalf("unknown capability %q", need)
		}
		if !ok {
			k.row.Skipped = os.Getenv(envAllowMissing) == "1"
			k.row.Failure = "worker lacks " + need
			missing(k.T, "worker lacks %s", need)
		}
	}
}

// goalVerdict is what the UI's source profile says about a goal for path.
func (k *kase) goalVerdict(path, goal string) (status, reason string) {
	k.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := mediainspect.InspectDetailed(ctx, k.s.ffprobe, path)
	k.require(err == nil && rep.Probed, "inspecting %s: %v", filepath.Base(path), err)
	goals := k.s.engine.SourceGoals(ctx, path, &rep, nil)
	v, ok := goals.Goals[goal]
	k.require(ok, "source profile has no verdict for goal %s", goal)
	return v.Status, v.ReasonCode
}

// promisedOK is the UI contract for a candidate case: the source profile must
// offer the goal the job is about to complete.
func (k *kase) promisedOK(path, goal string) {
	k.T.Helper()
	status, reason := k.goalVerdict(path, goal)
	k.require(status == "ok", "the source profile does not promise goal %s for a source the job converts: %s %s", goal, status, reason)
}

// candidateWant is what a goal promises about its candidate.
type candidateWant struct {
	// minSaved is the least percent of the source the candidate must save.
	minSaved float64
	// maxBytes caps the candidate (a size budget); zero for none.
	maxBytes int64
	// audio is the codec every audio stream must have; empty skips the check.
	audio string
	// sdr requires BT.709 SDR output no taller than maxHeight.
	sdr       bool
	maxHeight int
	// verdict is the final quality verdict required; "pass" by default.
	verdict string
}

// expectCandidate asserts what the user gets from a finished goal: a valid,
// playable HEVC candidate beside an untouched original, with the savings,
// audio, colour and quality the goal promises. It fills the report row.
func (k *kase) expectCandidate(src string, srcSHA string, r *result, want candidateWant) {
	k.T.Helper()
	k.fill(src, r)
	k.require(r.res.Status == "completed", "action did not complete: %s", r.describe())
	k.require(r.outcome() == "candidate_ready", "no candidate was produced: %s", r.describe())

	cand := r.candidate()
	k.require(cand != "", "completed without a candidate path: %s", r.describe())
	// Candidates sit in the source folder's own candidate directory.
	k.require(filepath.Dir(cand) == filepath.Join(filepath.Dir(src), k.s.candidatesDirName()),
		"candidate %s is not in the source folder's %s", filepath.Base(cand), k.s.candidatesDirName())
	candSize := fileSize(k.T, cand)
	srcSize := fileSize(k.T, src)
	k.row.CandidateB = candSize
	k.row.SavedPercent = 100 * float64(srcSize-candSize) / float64(srcSize)

	// The original is never touched, however the candidate came out.
	k.require(sha256File(k.T, src) == srcSHA, "the original changed during the run")
	k.require(getStr(r.res.Outputs, "original_sha256") == srcSHA, "the action's recorded original hash differs from the file's")

	info, orig := k.s.probe(k.T, cand), k.s.probe(k.T, src)
	video, ok := info.first("video")
	k.require(ok && video.CodecName == "hevc", "candidate video is %q, want hevc", video.CodecName)
	for _, st := range info.Streams {
		if st.CodecType == "audio" && want.audio != "" {
			k.require(st.CodecName == want.audio, "candidate audio is %s, want %s", st.CodecName, want.audio)
		}
	}
	k.require(countKind(info, "audio") == countKind(orig, "audio"), "candidate has %d audio streams, source %d", countKind(info, "audio"), countKind(orig, "audio"))
	k.require(math.Abs(info.Format.Duration-orig.Format.Duration) <= math.Max(1, orig.Format.Duration*0.01),
		"candidate lasts %.2fs, source %.2fs", info.Format.Duration, orig.Format.Duration)
	k.s.decodes(k.T, cand)

	k.require(k.row.SavedPercent >= want.minSaved, "candidate saves %.1f%% (%s from %s), want at least %.0f%%", k.row.SavedPercent, megabytes(candSize), megabytes(srcSize), want.minSaved)
	if want.maxBytes > 0 {
		k.require(candSize <= want.maxBytes, "candidate is %s, over the %s budget", megabytes(candSize), megabytes(want.maxBytes))
	}
	if want.sdr {
		k.require(video.Width <= want.maxHeight*16/9 && video.Height <= want.maxHeight, "candidate is %dx%d, want at most %dp", video.Width, video.Height, want.maxHeight)
		k.require(video.ColorPrimaries == "bt709" && video.ColorTransfer == "bt709" && video.ColorSpace == "bt709",
			"candidate colour is %s/%s/%s, want bt709 SDR", video.ColorPrimaries, video.ColorTransfer, video.ColorSpace)
		k.require(video.PixFmt == "yuv420p" || video.PixFmt == "yuv420p10le", "candidate pixel format is %s", video.PixFmt)
	}

	verdict := want.verdict
	if verdict == "" {
		verdict = "pass"
	}
	k.require(r.evidence != nil, "no final quality evidence was recorded")
	k.require(r.evidence.Verdict == verdict, "final quality verdict is %q (%v), want %q", r.evidence.Verdict, r.evidence.ReasonCodes, verdict)
	k.require(r.evidence.VMAF != nil, "the final quality evidence has no VMAF scores")
	k.require(r.evidence.VMAF.Mean > 20, "VMAF mean is %.1f: the candidate is misaligned or damaged", r.evidence.VMAF.Mean)
}

// fill records what the table shows for any case.
func (k *kase) fill(src string, r *result) {
	k.row.Outcome = r.outcome()
	k.row.ReasonCode = r.reason()
	k.row.WallSeconds = r.wall.Seconds()
	if src != "" {
		k.row.SourceBytes = fileSize(k.T, src)
	}
	if r.goal != nil {
		k.row.Encoder = r.goal.Encoder
	}
	if r.budget != nil {
		k.row.Rung = r.budget.Rung
		if r.budget.Choice != nil && k.row.Encoder == "" {
			k.row.Encoder = r.budget.Choice.VideoCodec
		}
	}
	if r.evidence != nil && r.evidence.VMAF != nil {
		k.row.VMAFMean, k.row.VMAFP5, k.row.VMAFWorst = r.evidence.VMAF.Mean, r.evidence.VMAF.P5, r.evidence.VMAF.WorstWindowMean
	}
}

func countKind(p probeInfo, kind string) int {
	n := 0
	for _, st := range p.Streams {
		if st.CodecType == kind {
			n++
		}
	}
	return n
}
