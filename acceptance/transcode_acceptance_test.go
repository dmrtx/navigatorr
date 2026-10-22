package acceptance

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/action"
)

// Sources are synthetic: a moving, textured test pattern with a little
// temporal noise, long enough for the sampling the frozen policies use.
var (
	sdrSource  = srcSpec{name: "sdr-1080p", w: 1920, h: 1080, secs: 20, noise: 1}
	hdrSource  = srcSpec{name: "hdr10-2160p", w: 3840, h: 2160, secs: 12, noise: 1, hdr: true}
	hevcSource = srcSpec{name: "hevc-1080p", w: 1920, h: 1080, secs: 12, hevc: true}
)

// caseTimeout bounds one action; a healthy case finishes in minutes.
const caseTimeout = 40 * time.Minute

// TestTranscodeAcceptance drives the real engine and the real worker binary
// through every goal on synthetic files. Run it on the transcode worker host
// before a deploy:
//
//	NAVIGATORR_ACCEPTANCE=1 go test ./acceptance -v -timeout 60m
func TestTranscodeAcceptance(t *testing.T) {
	gate(t)
	s := getSuite(t)
	rep := &reporter{}
	t.Cleanup(func() { rep.finish(t) })

	cases := []struct {
		name string
		run  func(k *kase)
	}{
		{"1_size_hardware", caseSizeHardware},
		{"1b_size_hardware_no_budget", caseSizeHardwareNoBudget},
		{"2_size_x265", caseSizeX265},
		{"3_compact_hdr", caseCompact},
		{"4_quality", caseQuality},
		{"5_x265_preserve", caseX265Preserve},
		{"6a_skip_hdr_size", caseSkipHDRSize},
		{"6b_skip_sdr_compact", caseSkipSDRCompact},
		{"6c_skip_hevc_preserve", caseSkipHEVCPreserve},
		{"7_timestamp_glitch", caseTimestampGlitch},
		{"8_no_size_tags", caseNoSizeTags},
		{"8b_no_size_tags_flac", caseNoSizeTagsFLAC},
		{"8c_size_lossy_audio", caseSizeLossyAudio},
		{"9_promotion", casePromotion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := &caseReport{Case: c.name}
			rep.add(row)
			k := &kase{T: t, s: s, row: row}
			defer func() { row.Passed = !t.Failed() && !t.Skipped() }()
			c.run(k)
		})
	}
}

// sha is the original's hash before any action touches it.
func (k *kase) sha(path string) string { return sha256File(k.T, path) }

// budgetPercent is the size budget of the size cases: a share of the source
// the VideoToolbox ladder reaches on this content with room to spare.
const budgetPercent = 60

func budgetBytes(src int64) int64 { return src * budgetPercent / 100 }

func caseSizeHardware(k *kase) {
	k.needCaps("libx264", "flac", "aac", "libx265", "hevc_videotoolbox", "vmaf")
	k.row.Goal = "size+hardware"
	src := k.s.source(k.T, sdrSource)
	sha := k.sha(src)
	k.promisedOK(src, "size")
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": true, "size_budget_percent": budgetPercent}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, maxBytes: budgetBytes(fileSize(k.T, src)), audio: "aac"})
	k.expectLadder(r)
}

// caseSizeHardwareNoBudget is the size goal with the media engine and no
// budget: the VideoToolbox quality ladder, planned without a sample test, whose
// rung passes the goal's quality gate on the whole file.
func caseSizeHardwareNoBudget(k *kase) {
	k.needCaps("libx264", "flac", "aac", "hevc_videotoolbox", "vmaf")
	k.row.Goal = "size+hardware, no budget"
	src := k.s.source(k.T, sdrSource)
	sha := k.sha(src)
	k.promisedOK(src, "size")
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": true}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, audio: "aac"})
	k.require(r.budget == nil, "a goal without a budget recorded one: %s", r.describe())
	k.require(r.ladder != nil, "the result has no quality ladder record: %s", r.describe())
	k.require(r.ladder.Outcome == action.QualityLadderPassed, "the quality ladder ended %q: %s", r.ladder.Outcome, r.ladder.Message)
	k.require(r.ladder.Choice == nil, "the ladder fell back: %s", r.ladder.Message)
	k.require(r.goal.Hardware && r.ladder.VideoCodec == "hevc_videotoolbox" && r.goal.Encoder == "hevc_videotoolbox",
		"the quality ladder encoded with %q/%q, want hevc_videotoolbox", r.ladder.VideoCodec, r.goal.Encoder)
	k.require(pickStr(r.res, "plan_source") == "quality_ladder", "the encode plan came from %q, want the quality ladder", pickStr(r.res, "plan_source"))
	k.expectNoSampleTest(r)
}

// expectLadder asserts a hardware size goal was met on the VideoToolbox ladder,
// not the x265 encode a worker without the media engine falls back to. Which
// rung fits depends on the node's media engine: the first (best picture) rung
// fits on some nodes and comes out over the budget on others, so any rung of
// the ladder passes.
func (k *kase) expectLadder(r *result) {
	k.T.Helper()
	k.require(r.skipReason() == "", "the size budget was not used: %s (%s)", r.skipReason(), pickStr(r.res, "size_budget_skip_message"))
	k.require(r.budget != nil, "the result has no size budget record: %s", r.describe())
	k.require(r.budget.Trigger == "budget_first", "the budget was planned by %q, want budget_first", r.budget.Trigger)
	k.require(r.budget.Choice != nil && r.budget.Choice.VideoCodec == "hevc_videotoolbox" && r.budget.Choice.Reason == action.SizeBudgetChoiceLadder,
		"the budget encoder choice is %+v, want the VideoToolbox ladder", r.budget.Choice)
	k.require(r.goal.Hardware && r.goal.Encoder == "hevc_videotoolbox", "the budget encoded with %q, want hevc_videotoolbox", r.goal.Encoder)
	k.expectNoSampleTest(r)
}

// expectNoSampleTest asserts the goal ran its frozen recipe: no sample test
// was submitted to plan it.
func (k *kase) expectNoSampleTest(r *result) {
	k.T.Helper()
	k.require(pickStr(r.res, "benchmark_job_id") == "", "a sample test ran: %s", r.describe())
}

// expectBudgetX265 asserts a size goal with the media engine off was met by
// its one two-pass x265 encode at the budget.
func (k *kase) expectBudgetX265(r *result) {
	k.T.Helper()
	k.require(r.skipReason() == "", "the size budget was not used: %s (%s)", r.skipReason(), pickStr(r.res, "size_budget_skip_message"))
	k.require(r.budget != nil, "the result has no size budget record: %s", r.describe())
	k.require(r.budget.Trigger == "budget_first", "the budget was planned by %q, want budget_first", r.budget.Trigger)
	k.require(r.budget.Choice != nil && r.budget.Choice.VideoCodec == "libx265" && r.budget.Choice.Reason == action.SizeBudgetChoiceHardwareOff,
		"the budget encoder choice is %+v, want libx265 because the media engine is off", r.budget.Choice)
	k.require(r.goal.Encoder == "libx265", "an x265 budget encoded with %q", r.goal.Encoder)
	k.expectNoSampleTest(r)
}

// expectQualityLadder asserts a quality goal was met on its x265 CRF ladder:
// a rung passed the goal's limits on the whole file.
func (k *kase) expectQualityLadder(r *result) {
	k.T.Helper()
	k.require(r.ladder != nil, "the result has no quality ladder record: %s", r.describe())
	k.require(r.ladder.Outcome == action.QualityLadderPassed, "the quality ladder ended %q: %s", r.ladder.Outcome, r.ladder.Message)
	k.require(r.ladder.VideoCodec == "libx265" && r.goal.Encoder == "libx265", "the quality ladder encoded with %q/%q, want libx265", r.ladder.VideoCodec, r.goal.Encoder)
	k.require(pickStr(r.res, "plan_source") == "quality_ladder", "the encode plan came from %q, want the quality ladder", pickStr(r.res, "plan_source"))
	k.expectNoSampleTest(r)
}

func caseSizeX265(k *kase) {
	k.needCaps("libx264", "flac", "aac", "libx265", "vmaf")
	k.row.Goal = "size+x265"
	src := k.s.source(k.T, sdrSource)
	sha := k.sha(src)
	k.promisedOK(src, "size")
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": false, "size_budget_percent": budgetPercent}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, audio: "aac", maxBytes: budgetBytes(fileSize(k.T, src))})
	k.require(!r.goal.Hardware && r.goal.Encoder == "libx265", "allow_hardware=false encoded with %q, want libx265", r.goal.Encoder)
	k.expectBudgetX265(r)
}

func caseCompact(k *kase) {
	k.needCaps("libx265", "aac", "lut3d", "vmaf")
	k.row.Goal = "compact"
	src := k.s.source(k.T, hdrSource)
	sha := k.sha(src)
	k.promisedOK(src, "compact")
	r := k.s.transcode(k.T, src, map[string]any{"mode": "compact"}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 30, audio: "aac", sdr: true, maxHeight: 1080})
	k.expectQualityLadder(r)
}

func caseQuality(k *kase) {
	k.needCaps("libx264", "flac", "libx265", "vmaf")
	k.row.Goal = "quality"
	src := k.s.source(k.T, sdrSource)
	sha := k.sha(src)
	k.promisedOK(src, "quality")
	r := k.s.transcode(k.T, src, map[string]any{"mode": "quality"}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{})
	k.expectQualityLadder(r)
}

func caseX265Preserve(k *kase) {
	k.needCaps("libx264", "flac", "libx265", "vmaf")
	k.row.Goal = "x265_preserve"
	src := k.s.source(k.T, sdrSource)
	sha := k.sha(src)
	k.promisedOK(src, "x265_preserve")
	r := k.s.transcode(k.T, src, map[string]any{"mode": "x265_preserve"}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{})
	k.expectQualityLadder(r)
}

// expectSkip runs a goal that must end without a candidate and checks the
// source profile gave the same verdict for the same file.
func (k *kase) expectSkip(spec srcSpec, goal, reason string) {
	k.T.Helper()
	k.row.Goal = goal
	src := k.s.source(k.T, spec)
	sha := k.sha(src)

	status, code := k.goalVerdict(src, goal)
	k.require(status == "skip" && code == reason, "the source profile says %s/%s for goal %s, want skip/%s", status, code, goal, reason)

	r := k.s.transcode(k.T, src, map[string]any{"mode": goal}, caseTimeout)
	k.fill(src, r)
	k.require(r.res.Status == "completed", "action did not complete: %s", r.describe())
	k.require(r.reason() == reason, "skipped with reason %q, want %q: %s", r.reason(), reason, r.describe())
	k.require(r.candidate() == "", "a skipped goal produced a candidate %s", r.candidate())
	k.require(r.outcome() == "no_changes", "outcome is %q, want no_changes: %s", r.outcome(), r.describe())
	k.require(k.sha(src) == sha, "the original changed during a skipped run")
}

// A skip is decided from the probe alone, so the skip cases need no encoder or
// metric.
func caseSkipHDRSize(k *kase) {
	k.expectSkip(hdrSource, "size", "hdr_requires_compact")
}

func caseSkipSDRCompact(k *kase) {
	k.expectSkip(sdrSource, "compact", "compact_not_needed")
}

func caseSkipHEVCPreserve(k *kase) {
	k.expectSkip(hevcSource, "x265_preserve", "already_target_codec")
}

// vfrSource has a stretch of frames closer together than one frame period, the
// shape that made an encode drop frames and the quality gate compare shifted
// pictures.
var vfrSource = srcSpec{name: "sdr-vfr-1080p", w: 1920, h: 1080, secs: 28, noise: 1, vfr: true}

func caseTimestampGlitch(k *kase) {
	k.needCaps("libx264", "flac", "aac", "libx265", "hevc_videotoolbox", "vmaf")
	k.row.Goal = "size+hardware"
	src := k.s.source(k.T, vfrSource)
	sha := k.sha(src)
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": true, "size_budget_percent": budgetPercent}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, audio: "aac"})
	k.expectLadder(r)
	want, got := k.s.videoFrames(k.T, src), k.s.videoFrames(k.T, r.candidate())
	k.require(want == got, "the candidate has %d frames, the source %d: frames were dropped or merged", got, want)
}

// untaggedSource carries no per-stream Matroska statistics, as most remuxed
// files do; its AC-3 track still states its bitrate.
var untaggedSource = srcSpec{name: "sdr-untagged-1080p", w: 1920, h: 1080, secs: 20, noise: 1, untagged: true, audio: "ac3"}

func caseNoSizeTags(k *kase) {
	k.needCaps("libx264", "ac3", "libx265", "hevc_videotoolbox", "vmaf")
	k.row.Goal = "size+hardware"
	src := k.s.source(k.T, untaggedSource)
	sha := k.sha(src)
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": true, "size_budget_percent": budgetPercent}, caseTimeout)
	k.require(r.reason() != "size_budget_estimate_unavailable", "the size budget could not be planned for a source without stream size tags: %s", r.describe())
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, maxBytes: budgetBytes(fileSize(k.T, src))})
	k.expectLadder(r)
}

// untaggedFLACSource is the same without a stated audio bitrate either: a
// lossless track in a remux states neither a size nor a bitrate.
var untaggedFLACSource = srcSpec{name: "sdr-untagged-flac-1080p", w: 1920, h: 1080, secs: 20, noise: 1, untagged: true}

func caseNoSizeTagsFLAC(k *kase) {
	k.needCaps("libx264", "flac", "aac", "libx265", "hevc_videotoolbox", "vmaf")
	k.row.Goal = "size+hardware"
	src := k.s.source(k.T, untaggedFLACSource)
	sha := k.sha(src)
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": true, "size_budget_percent": budgetPercent}, caseTimeout)
	k.require(r.reason() != "size_budget_estimate_unavailable", "the size budget could not be planned for a source whose streams state no size and whose audio states no bitrate: %s", r.describe())
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, maxBytes: budgetBytes(fileSize(k.T, src)), audio: "aac"})
	k.expectLadder(r)
}

// lossyAudioSource carries a tagged AC-3 stereo track at 384 kbps, twice the
// AAC-LC stereo target: the size goal's audio level 1 converts it.
var lossyAudioSource = srcSpec{name: "sdr-ac3-1080p", w: 1920, h: 1080, secs: 20, noise: 1, audio: "ac3"}

func caseSizeLossyAudio(k *kase) {
	k.needCaps("libx264", "ac3", "aac", "libx265", "vmaf")
	k.row.Goal = "size+x265"
	src := k.s.source(k.T, lossyAudioSource)
	sha := k.sha(src)
	r := k.s.transcode(k.T, src, map[string]any{"mode": "size", "allow_hardware": false, "size_budget_percent": budgetPercent}, caseTimeout)
	k.expectCandidate(src, sha, r, candidateWant{minSaved: 15, audio: "aac", maxBytes: budgetBytes(fileSize(k.T, src))})
	k.expectBudgetX265(r)
	k.require(r.budget.Audio != nil && r.budget.Audio.Level >= 1 && len(r.budget.Audio.Tracks) == 1,
		"the budget recorded no audio level for a lossy track above the AAC-LC target: %+v", r.budget.Audio)
	k.require(r.budget.Audio.Tracks[0].Output != "copy", "the lossy track was %q, want it converted", r.budget.Audio.Tracks[0].Output)
}

// promoSource is a short file of its own: promotion replaces it in the
// library, so no other case may share it.
var promoSource = srcSpec{name: "promote-1080p", w: 1920, h: 1080, secs: 12, noise: 1}

func casePromotion(k *kase) {
	k.needCaps("libx264", "flac", "libx265", "vmaf")
	k.row.Goal = "promotion"
	src := k.s.source(k.T, promoSource)
	origSHA := k.sha(src)
	r := k.s.transcode(k.T, src, map[string]any{"mode": "x265_preserve"}, caseTimeout)
	k.expectCandidate(src, origSHA, r, candidateWant{})
	cand := r.candidate()
	candSHA := k.sha(cand)
	candInfo := k.s.probe(k.T, cand)

	start := time.Now()
	defer func() { k.row.WallSeconds += time.Since(start).Seconds() }()
	ctx := context.Background()
	p, err := k.s.engine.Run(ctx, "promote_transcode_candidate", map[string]any{"transcode_action_id": r.res.ID, "service": "filesystem"})
	k.require(err == nil, "starting the promotion: %v", err)
	review := k.s.await(k.T, p.ID, 5*time.Minute)
	k.require(review.Status == action.StatusWaitingDecision, "promotion did not wait for approval: status=%s error=%q waiting=%q", review.Status, review.Error, review.WaitingReason)
	digest := action.PromotionPlanDigest(pick(review, "promotion"))
	k.require(digest != "", "the promotion plan has no digest to approve")
	k.require(k.sha(src) == origSHA, "planning the promotion changed the original")

	_, err = k.s.engine.ResumeReviewedPromotion(ctx, p.ID, digest)
	k.require(err == nil, "approving the promotion: %v", err)
	done := k.s.await(k.T, p.ID, 10*time.Minute)
	k.require(done.Status == action.StatusCompleted, "promotion did not complete: status=%s error=%q waiting=%q", done.Status, done.Error, done.WaitingReason)

	var st struct {
		NewPath  string `json:"new_path"`
		Recovery string `json:"recovery_path"`
	}
	k.require(decode(pick(done, "promotion"), &st), "the finished promotion has no record")
	want := strings.TrimSuffix(src, filepath.Ext(src)) + filepath.Ext(cand)
	k.require(st.NewPath == want, "the promoted file is at a different path than the original name with the candidate extension")

	k.require(k.sha(want) == candSHA, "the library file is not the candidate that was reviewed")
	info := k.s.probe(k.T, want)
	video, ok := info.first("video")
	k.require(ok && video.CodecName == "hevc", "the library file is %q video, want hevc", video.CodecName)
	k.require(math.Abs(info.Format.Duration-candInfo.Format.Duration) < 0.1, "the library file lasts %.2fs, the candidate %.2fs", info.Format.Duration, candInfo.Format.Duration)
	k.s.decodes(k.T, want)
	if want != src {
		_, serr := os.Stat(src)
		k.require(os.IsNotExist(serr), "the old library file is still present")
	}
	_, cerr := os.Stat(cand)
	k.require(os.IsNotExist(cerr), "the candidate file is still in the candidate directory")
	if st.Recovery != "" {
		_, rerr := os.Stat(st.Recovery)
		k.require(os.IsNotExist(rerr), "the recovery copy of the original was not cleaned up")
	}
	k.row.Outcome = "promoted"
}
