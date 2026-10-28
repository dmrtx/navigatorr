package action

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

const (
	levelSourceBytes = int64(3_000_000_000)
	levelVideoBytes  = "1700000000"
	levelTrackBytes  = "144000000" // 640 kbps over 1800 s
)

// levelReport is a 30 minute 1080p episode: eight E-AC-3 5.1 tracks at 640 kbps
// (1.15 GB), about 30 text subtitle streams and a container, around a 1.7 GB
// video stream, in a 3 GB file. The names are synthetic.
func levelReport() *mediainspect.DetailedReport {
	rep := &mediainspect.DetailedReport{DurationSec: 1800,
		Video: []mediainspect.DetailedStream{{Index: 0, Codec: "h264", BitDepth: 8, Width: 1920, Height: 1080, FPS: 23.976, Tags: map[string]string{"NUMBER_OF_BYTES": levelVideoBytes}}},
	}
	for i := 0; i < 8; i++ {
		rep.Audio = append(rep.Audio, mediainspect.DetailedStream{Index: 1 + i, Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", Language: "und", BitRate: 640_000, Tags: map[string]string{"NUMBER_OF_BYTES": levelTrackBytes}})
	}
	for i := 0; i < 30; i++ {
		rep.Subtitles = append(rep.Subtitles, mediainspect.DetailedStream{Index: 9 + i, Codec: "subrip", Language: "und", Tags: map[string]string{"NUMBER_OF_BYTES": "4000000"}})
	}
	return rep
}

func levelPolicy(t *testing.T, bytes float64) *SizeBudgetPolicy {
	t.Helper()
	p, err := modeSizeBudgetInput("size", map[string]any{"size_budget_bytes": bytes})
	if err != nil || p == nil || p.Audio == nil {
		t.Fatalf("policy %+v %v", p, err)
	}
	return p
}

// The picture a level leaves is the budget's target less everything else.
func levelVideoKbps(policy *SizeBudgetPolicy, rep *mediainspect.DetailedReport, budget int64, mode string) int {
	return planSizeBudgetEncodeAt(policy, rep, levelSourceBytes, budget, mode).X265.BitrateKbps
}

// The owner's example: eight E-AC-3 640 kbps tracks and a 1.00 GB budget. AAC-LC
// at 384 kbps (level 1) leaves the picture about 0.6 Mbps, below the good
// bitrate for 1080p; HE-AAC at 160 kbps (level 2) leaves it about 2.4 Mbps.
func TestSizeBudgetAudioLevelForTheEightTrackEpisode(t *testing.T) {
	rep := levelReport()
	policy := levelPolicy(t, 1_000_000_000)
	good := policy.Audio.goodVideoKbps(rep, levelSourceBytes)
	if good < 1980 || good > 2000 {
		t.Fatalf("good video bitrate for 1080p at 23.976 fps is %d kbps, want about 1990", good)
	}
	l1 := levelVideoKbps(policy, rep, 1_000_000_000, transcode.AudioModeSizeAACBed)
	l2 := levelVideoKbps(policy, rep, 1_000_000_000, transcode.AudioModeSizeHEAACBed)
	if l1 >= good || l2 < good || l1 < transcode.X265MinBitrateKbps {
		t.Fatalf("level 1 leaves %d kbps and level 2 %d against a good %d", l1, l2, good)
	}

	enc := planSizeBudgetEncode(policy, rep, levelSourceBytes, 1_000_000_000)
	if enc.Reason != "" || enc.AudioMode != transcode.AudioModeSizeHEAACBed || enc.X265.BitrateKbps != l2 {
		t.Fatalf("1.00 GB planned %+v", enc)
	}
	a := enc.Audio
	if a == nil || a.Mode != transcode.AudioModeSizeHEAACBed || a.Level != 2 || a.GoodVideoKbps != good || a.PreviousVideoKbps != l1 || len(a.Tracks) != 8 || a.converted() != 8 {
		t.Fatalf("audio record %+v", a)
	}
	for i, tr := range a.Tracks {
		if tr.Index != i || tr.Codec != "eac3" || tr.SourceKbps != 640 || tr.Output != "he_aac" || tr.Kbps != 160 {
			t.Fatalf("track %d: %+v", i, tr)
		}
	}
	for _, want := range []string{"Audio level 2 of 2", "8 of 8 tracks", "HE-AAC", "subtitles and attachments are untouched", "Level 1 left only"} {
		if !strings.Contains(sizeBudgetAudioSentence(a), want) {
			t.Errorf("sentence %q lacks %q", sizeBudgetAudioSentence(a), want)
		}
	}
}

// A roomy budget converts the lossy tracks to AAC-LC and keeps the picture
// good: level 1 is the minimum of a new snapshot, so level 0 is never chosen.
func TestSizeBudgetAudioLevelOneIsTheMinimum(t *testing.T) {
	rep := levelReport()
	policy := levelPolicy(t, 2_500_000_000)
	enc := planSizeBudgetEncode(policy, rep, levelSourceBytes, 2_500_000_000)
	if enc.Reason != "" || enc.AudioMode != transcode.AudioModeSizeAACBed || enc.Audio == nil || enc.Audio.Level != 1 || enc.Audio.PreviousVideoKbps != 0 {
		t.Fatalf("2.5 GB planned %+v", enc)
	}
	if enc.X265.BitrateKbps < policy.Audio.goodVideoKbps(rep, levelSourceBytes) {
		t.Fatalf("level 1 chosen with %d kbps of video", enc.X265.BitrateKbps)
	}
	for i, tr := range enc.Audio.Tracks {
		if tr.Output != "aac" || tr.Kbps != 384 {
			t.Fatalf("track %d at level 1: %+v", i, tr)
		}
	}
	// Even a budget the original audio would fit in converts: no budget picks
	// the level that only converts lossless audio.
	huge := planSizeBudgetEncode(policy, rep, levelSourceBytes, sizeBudgetCeiling(levelSourceBytes))
	if huge.AudioMode != transcode.AudioModeSizeAACBed {
		t.Fatalf("the largest budget planned %+v", huge)
	}
	if strings.Contains(sizeBudgetAudioSentence(enc.Audio), "Level 1 left") {
		t.Errorf("level 1 explains a level it did not reject: %q", sizeBudgetAudioSentence(enc.Audio))
	}
}

// Across budgets the choice is one rule: the least compressed level that leaves
// the picture good, else the most compressed level that leaves a usable one,
// else unreachable with the most compressed level's figures.
func TestSizeBudgetAudioLevelRule(t *testing.T) {
	rep := levelReport()
	var seen = map[string]int{}
	for budget := int64(300_000_000); budget <= 2_550_000_000; budget += 25_000_000 {
		policy := levelPolicy(t, float64(budget))
		good := policy.Audio.goodVideoKbps(rep, levelSourceBytes)
		l1 := planSizeBudgetEncodeAt(policy, rep, levelSourceBytes, budget, transcode.AudioModeSizeAACBed)
		l2 := planSizeBudgetEncodeAt(policy, rep, levelSourceBytes, budget, transcode.AudioModeSizeHEAACBed)
		got := planSizeBudgetEncode(policy, rep, levelSourceBytes, budget)
		// Level 2 is both the last usable level and the unreachable figures.
		want := l2
		if l1.Reason == "" && l1.X265.BitrateKbps >= good {
			want = l1
		}
		if got.AudioMode != want.AudioMode || got.Reason != want.Reason || got.X265.BitrateKbps != want.X265.BitrateKbps || got.TargetBytes != want.TargetBytes {
			t.Fatalf("budget %d: chose %s (%s) at %d kbps, rule says %s (%s) at %d kbps", budget, got.AudioMode, got.Reason, got.X265.BitrateKbps, want.AudioMode, want.Reason, want.X265.BitrateKbps)
		}
		if got.Reason == "" && got.Audio == nil || got.Reason == "" && got.Audio.Mode != got.AudioMode {
			t.Fatalf("budget %d: record %+v", budget, got.Audio)
		}
		key := got.AudioMode
		if got.Reason != "" {
			key = got.Reason
		}
		seen[key]++
	}
	if seen[transcode.AudioModeSizeAACBed] == 0 || seen[transcode.AudioModeSizeHEAACBed] == 0 || seen["size_budget_unreachable"] == 0 {
		t.Fatalf("the sweep did not reach every outcome: %+v", seen)
	}
	// Level 0 is never chosen for a new snapshot.
	if seen[transcode.AudioModeCompactLossless] != 0 {
		t.Fatalf("level 0 chosen: %+v", seen)
	}
}

// A budget too small for any level ends as unreachable, with the figures of the
// most compressed level, so "at least X" is what really would work.
func TestSizeBudgetAudioLevelUnreachable(t *testing.T) {
	rep := levelReport()
	policy := levelPolicy(t, 300_000_000)
	enc := planSizeBudgetEncode(policy, rep, levelSourceBytes, 300_000_000)
	if enc.Reason != "size_budget_unreachable" || enc.AudioMode != transcode.AudioModeSizeHEAACBed {
		t.Fatalf("300 MB planned %+v", enc)
	}
	least := planSizeBudgetEncodeAt(policy, rep, levelSourceBytes, 300_000_000, transcode.AudioModeSizeHEAACBed)
	if enc.NonVideoBytes != least.NonVideoBytes {
		t.Fatalf("unreachable figures %d are not the most compressed level's %d", enc.NonVideoBytes, least.NonVideoBytes)
	}
	minimum := sizeBudgetMinimumBudget(policy, enc.NonVideoBytes, rep.DurationSec)
	if got := planSizeBudgetEncode(policy, rep, levelSourceBytes, minimum); got.Reason != "" {
		t.Fatalf("the stated minimum %d is unreachable: %+v", minimum, got)
	}
	if got := planSizeBudgetEncode(policy, rep, levelSourceBytes, minimum-1); got.Reason != "size_budget_unreachable" {
		t.Fatalf("a budget under the stated minimum plans: %+v", got)
	}
}

// A track already at or below its level's target is copied, E-AC-3 JOC is
// copied at every level, and the track count and order never change.
func TestSizeBudgetAudioRecordCopiesWhatIsWithinTarget(t *testing.T) {
	rep := levelReport()
	rep.Audio = []mediainspect.DetailedStream{
		{Index: 1, Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 640_000},
		{Index: 2, Codec: "ac3", Channels: 6, ChannelLayout: "5.1", BitRate: 384_000},
		{Index: 3, Codec: "aac", Profile: "LC", Channels: 2, ChannelLayout: "stereo", BitRate: 128_000},
		{Index: 4, Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Channels: 6, ChannelLayout: "5.1", BitRate: 768_000},
		{Index: 5, Codec: "truehd", Channels: 8, ChannelLayout: "7.1", BitRate: 4_000_000},
		{Index: 6, Codec: "dts", Profile: "DTS", Channels: 6, ChannelLayout: "5.1(side)"},
	}
	type out struct {
		output string
		kbps   int
	}
	for mode, want := range map[string][]out{
		transcode.AudioModeSizeAAC:   {{"aac", 384}, {"copy", 0}, {"copy", 0}, {"copy", 0}, {"aac", 512}, {"copy", 0}},
		transcode.AudioModeSizeHEAAC: {{"he_aac", 160}, {"he_aac", 160}, {"he_aac", 64}, {"copy", 0}, {"aac", 512}, {"copy", 0}},
	} {
		level := 1
		if mode == transcode.AudioModeSizeHEAAC {
			level = 2
		}
		r := sizeBudgetAudioRecord(rep, mode, level, 1990, 0)
		if len(r.Tracks) != len(rep.Audio) {
			t.Fatalf("%s: %d tracks of %d", mode, len(r.Tracks), len(rep.Audio))
		}
		for i, tr := range r.Tracks {
			if tr.Index != i || tr.Output != want[i].output || tr.Kbps != want[i].kbps {
				t.Errorf("%s track %d (%s): %+v, want %+v", mode, i, tr.Codec, tr, want[i])
			}
		}
	}
	// A level that converts nothing says so.
	only := &mediainspect.DetailedReport{DurationSec: 1800, Audio: []mediainspect.DetailedStream{{Index: 1, Codec: "aac", Channels: 2, ChannelLayout: "stereo", BitRate: 128_000}}}
	if s := sizeBudgetAudioSentence(sizeBudgetAudioRecord(only, transcode.AudioModeSizeAACBed, 1, 1990, 0)); !strings.Contains(s, "all 1 tracks are copied, each already within 25% of its target") || strings.Contains(s, "Atmos") {
		t.Errorf("sentence %q", s)
	}
	// A level frozen before the bed pair still says it copies lossy Atmos.
	if s := sizeBudgetAudioSentence(sizeBudgetAudioRecord(only, transcode.AudioModeSizeAAC, 1, 1990, 0)); !strings.Contains(s, "lossy Atmos/DTS:X, which this goal copies") {
		t.Errorf("object-copy sentence %q", s)
	}
}

// The good bitrate scales with the picture and never asks for more than the
// source ever had.
func TestSizeBudgetGoodVideoBitrate(t *testing.T) {
	a := &SizeBudgetAudio{GoodBitsPerPixel: sizeBudgetGoodBitsPerPixel}
	rep := levelReport()
	if got := a.goodVideoKbps(rep, levelSourceBytes); got < 1985 || got > 1995 {
		t.Errorf("1080p at 23.976 fps: %d kbps", got)
	}
	rep.Video[0].Width, rep.Video[0].Height = 3840, 2160
	rep.Video[0].Tags = map[string]string{"NUMBER_OF_BYTES": "2900000000"}
	if got := a.goodVideoKbps(rep, levelSourceBytes); got < 7940 || got > 7960 {
		t.Errorf("2160p: %d kbps", got)
	}
	// A source whose video stream was 1 Mbps asks for no more than that.
	rep = levelReport()
	rep.Video[0].Tags = map[string]string{"NUMBER_OF_BYTES": "225000000"}
	if got := a.goodVideoKbps(rep, levelSourceBytes); got != 1000 {
		t.Errorf("capped at the source's own bitrate: %d kbps", got)
	}
	// Unknown geometry or frame rate asks for nothing, so level 1 is accepted.
	rep = levelReport()
	rep.Video[0].FPS = 0
	if got := a.goodVideoKbps(rep, levelSourceBytes); got != 0 {
		t.Errorf("unknown frame rate: %d kbps", got)
	}
	if sizeBudgetGoodBitsPerPixel != 0.04 {
		t.Errorf("the good bitrate constant changed to %v", sizeBudgetGoodBitsPerPixel)
	}
}

// Budgets frozen before the levels carry no Audio and keep level 0: only
// lossless audio converts, so the eight lossy tracks stay and the 1.00 GB
// budget is unreachable, exactly as before.
func TestSizeBudgetWithoutLevelsKeepsLevelZero(t *testing.T) {
	rep := levelReport()
	policy := levelPolicy(t, 1_000_000_000)
	policy.Audio = nil
	if got := policy.audioModes(); len(got) != 1 || got[0] != transcode.AudioModeCompactLossless {
		t.Fatalf("legacy modes %v", got)
	}
	enc := planSizeBudgetEncode(policy, rep, levelSourceBytes, 1_000_000_000)
	if enc.Reason != "size_budget_unreachable" || enc.AudioMode != transcode.AudioModeCompactLossless || enc.Audio != nil {
		t.Fatalf("legacy budget planned %+v", enc)
	}
	roomy := planSizeBudgetEncode(policy, rep, levelSourceBytes, 2_500_000_000)
	if roomy.Reason != "" || roomy.AudioMode != transcode.AudioModeCompactLossless || roomy.Audio != nil {
		t.Fatalf("legacy roomy budget planned %+v", roomy)
	}
	if sizeBudgetAudioRecordJSON(roomy.Audio) != "null" {
		t.Fatalf("legacy record carries audio: %s", sizeBudgetAudioRecordJSON(roomy.Audio))
	}
	// Audio levels are validated when the snapshot is read back.
	for name, a := range map[string]*SizeBudgetAudio{
		"other order": {Modes: []string{transcode.AudioModeSizeHEAAC, transcode.AudioModeSizeAAC}, GoodBitsPerPixel: 0.04},
		"level zero":  {Modes: []string{transcode.AudioModeCompactLossless}, GoodBitsPerPixel: 0.04},
		"no target":   {Modes: transcode.AudioLevelModes},
		"too high":    {Modes: transcode.AudioLevelModes, GoodBitsPerPixel: 4},
	} {
		if a.validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The plan step freezes the level in the plan, the record and the message, and
// the validation step rebuilds exactly that from the frozen policy.
func TestSizeBudgetPlanStepFreezesTheAudioLevel(t *testing.T) {
	for name, hardware := range map[string]bool{"x265": false, "media engine": true} {
		t.Run(name, func(t *testing.T) {
			c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_000_000_000.0, "allow_hardware": hardware}, nil)
			c.ec.State["source_report"], c.ec.State["original_size"] = levelReport(), levelSourceBytes
			planRecipe(t, c)
			plan := getPlan(c.ec.State["plan"])
			rec := sizeBudgetOf(c.ec.State)
			if rec == nil || plan == nil || rec.Outcome == SizeBudgetExceeded || getBool(c.ec.State, "skip_transcode") {
				t.Fatalf("not planned: %+v plan %+v state %+v", rec, plan, c.ec.State)
			}
			if plan.ModePolicyVersion != transcode.ModePolicyV5 || plan.AudioMode != transcode.AudioModeSizeHEAACBed || rec.Audio == nil || rec.Audio.Mode != plan.AudioMode || rec.Audio.Level != 2 || rec.Audio.converted() != 8 {
				t.Fatalf("plan %s %s record %+v", plan.ModePolicyVersion, plan.AudioMode, rec.Audio)
			}
			if !strings.Contains(rec.Message, "Audio level 2 of 2") || !strings.Contains(rec.Message, "8 of 8 tracks") {
				t.Fatalf("message %q", rec.Message)
			}
			if got := plan.PlanDigest; got == "" || c.ec.State["plan_digest"] != got {
				t.Fatalf("digest %q", got)
			}
			if !sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, rec) {
				t.Fatal("the plan does not rebuild from the frozen policy")
			}
			// A plan with another level, or a record that disagrees with the
			// plan's source, is not the frozen plan.
			other, err := planWithAudioMode(plan, transcode.AudioModeSizeAACBed)
			if err != nil || sizeBudgetPlanMatches(c.snapshot, c.ec.State, other, rec) {
				t.Fatalf("a plan with level 1 matched: %v", err)
			}
			tampered := *rec
			a := *rec.Audio
			a.Tracks = append([]SizeBudgetAudioTrack(nil), a.Tracks...)
			a.Tracks[0].Output, a.Tracks[0].Kbps = "copy", 0
			tampered.Audio = &a
			if sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, &tampered) {
				t.Fatal("a record with a different track plan matched")
			}
			// The encoder is part of the frozen record: a record that names
			// another encoder for a track is not the plan's.
			for _, other := range []string{"aac", "", "opus"} {
				if a.Tracks[0].Encoder == other {
					continue
				}
				b := *rec.Audio
				b.Tracks = append([]SizeBudgetAudioTrack(nil), b.Tracks...)
				b.Tracks[1].Encoder = other
				tampered.Audio = &b
				if sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, &tampered) {
					t.Fatalf("a record naming encoder %q for a track matched", other)
				}
			}
			tampered.Audio = nil
			if sizeBudgetPlanMatches(c.snapshot, c.ec.State, plan, &tampered) {
				t.Fatal("a record without the level matched")
			}
		})
	}
}

// The submit step and the final validation request check the capability for
// every level plan, so a plan frozen before a worker downgrade is refused here
// and never reaches a worker that would reject it.
func TestAudioLevelPlansNeedAWorkerWithAudioLevels(t *testing.T) {
	// A worker with the levels but not the bed pair runs the levels that copy
	// lossy Atmos, which queued goals froze, and refuses the bed pair.
	workers := map[string]transcode.WorkerCapabilities{
		"old":    {ProtocolVersion: transcode.WorkerProtocolVersion},
		"levels": {ProtocolVersion: transcode.WorkerProtocolVersion, SizeAudioLevels: true},
		"bed":    {ProtocolVersion: transcode.WorkerProtocolVersion, SizeAudioLevels: true, SizeAudioObjectBed: true},
	}
	for _, mode := range append(slices.Clone(transcode.ObjectCopyAudioLevelModes), transcode.AudioLevelModes...) {
		bed := transcode.IsObjectBedAudioLevelMode(mode)
		t.Run(mode, func(t *testing.T) {
			for name, worker := range workers {
				want := ""
				switch {
				case name == "old":
					want = "audio levels"
				case name == "levels" && bed:
					want = errAudioObjectBedWorker
				}
				mock := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
					return worker, nil
				}}
				e := NewEngine(EngineDeps{Transcode: mock})
				ec := testReconcileEC("job-levels")
				plan := testReconcilePlan()
				plan.Mode, plan.ModePolicyVersion, plan.AudioMode = "size", transcode.ModePolicyV5, mode
				ec.State["plan"] = plan
				res, err := e.stepTranscodeSubmit(context.Background(), ec)
				if err != nil {
					t.Fatal(err)
				}
				refused := res.Status == StepFailed && want != "" && strings.Contains(res.Error, want)
				if refused != (want != "") || want == "" && res.Status == StepFailed {
					t.Fatalf("%s worker: status %s error %q, want refusal %q", name, res.Status, res.Error, want)
				}
			}
			ec := &ExecutionContext{InstanceID: "levels", Inputs: map[string]any{"mode": "size"}, State: map[string]any{"original_sha256": strings.Repeat("a", 64)}}
			policy, err := freezeModePolicy(ec)
			if err != nil {
				t.Fatal(err)
			}
			ec.State["plan"] = &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 23, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, Mode: "size", AudioMode: mode, ModePolicyVersion: transcode.ModePolicyV5}
			rep := &mediainspect.DetailedReport{DurationSec: 600, Video: []mediainspect.DetailedStream{{BitDepth: 8, FPS: 24, BitRate: 5000000}}}
			caps := modeTestCapabilities()
			caps.SizeAudioLevels = false
			if _, err := finalRequestForPlan(ec, "/media/source.mkv", rep, policy.Profile.Optimization, caps); err == nil || !strings.Contains(err.Error(), "audio levels") {
				t.Fatalf("an old worker was given the final validation: %v", err)
			}
			caps.SizeAudioLevels, caps.SizeAudioObjectBed = true, false
			_, err = finalRequestForPlan(ec, "/media/source.mkv", rep, policy.Profile.Optimization, caps)
			if refused := err != nil && strings.Contains(err.Error(), errAudioObjectBedWorker); refused != bed {
				t.Fatalf("a worker without the bed pair, final validation: %v", err)
			}
		})
	}
}

// A worker with the levels but not the bed pair would refuse a bed-pair plan:
// the coordinator declines the budget before submitting.
func TestSizeBudgetNeedsAWorkerWithTheObjectBed(t *testing.T) {
	c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_000_000_000.0}, func(caps *transcode.WorkerCapabilities) { caps.SizeAudioObjectBed = false })
	c.ec.State["source_report"], c.ec.State["original_size"] = levelReport(), levelSourceBytes
	res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
	if err != nil || res.Status != StepCompleted || !getBool(res.Outputs, "skipped") && !getBool(c.ec.State, "skip_transcode") {
		t.Fatalf("a worker without the bed pair was given the plan: %+v %v", res, err)
	}
	if reason := getString(c.ec.State, "reason_code"); reason != "size_budget_worker_unsupported" {
		t.Fatalf("reason %q", reason)
	}
	if c.executor.submitCalls != 0 {
		t.Fatalf("submitted %d jobs", c.executor.submitCalls)
	}
}

// A worker without the levels would not know the new audio modes and would
// refuse the plan: the coordinator declines the budget before submitting.
func TestSizeBudgetNeedsAWorkerWithAudioLevels(t *testing.T) {
	for name, hardware := range map[string]bool{"x265": false, "media engine": true} {
		t.Run(name, func(t *testing.T) {
			c := newRecipeCase(t, map[string]any{"size_budget_bytes": 1_000_000_000.0, "allow_hardware": hardware}, func(caps *transcode.WorkerCapabilities) { caps.SizeAudioLevels = false })
			c.ec.State["source_report"], c.ec.State["original_size"] = levelReport(), levelSourceBytes
			res, err := c.engine.stepPlanEncode(context.Background(), c.ec)
			if err != nil || res.Status != StepCompleted || !getBool(res.Outputs, "skipped") && !getBool(c.ec.State, "skip_transcode") {
				t.Fatalf("an old worker was given the plan: %+v %v", res, err)
			}
			if reason := getString(c.ec.State, "reason_code"); reason != "size_budget_worker_unsupported" {
				t.Fatalf("reason %q", reason)
			}
			if c.executor.submitCalls != 0 {
				t.Fatalf("submitted %d jobs", c.executor.submitCalls)
			}
		})
	}
}

// A track is re-encoded only when it is at least 1.25 times its level's target:
// AC-3 5.1 at 448 kbps stays at level 1 and is HE-AAC at level 2, E-AC-3 5.1 at
// 640 kbps is converted at both, and a FLAC track that states a low bitrate
// (a quiet or mono one) is copied while one that states none is converted.
func TestSizeBudgetAudioRecordMinimumGain(t *testing.T) {
	rep := levelReport()
	rep.Audio = []mediainspect.DetailedStream{
		{Index: 1, Codec: "ac3", Channels: 6, ChannelLayout: "5.1", BitRate: 448_000},
		{Index: 2, Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", BitRate: 640_000},
		{Index: 3, Codec: "aac", Profile: "LC", Channels: 6, ChannelLayout: "5.1", BitRate: 384_000},
		{Index: 4, Codec: "flac", Channels: 2, ChannelLayout: "stereo", BitRate: 133_000},
		{Index: 5, Codec: "flac", Channels: 2, ChannelLayout: "stereo"},
	}
	type out struct {
		output string
		kbps   int
	}
	for mode, want := range map[string][]out{
		transcode.AudioModeSizeAAC:   {{"copy", 0}, {"aac", 384}, {"copy", 0}, {"copy", 0}, {"aac", 192}},
		transcode.AudioModeSizeHEAAC: {{"he_aac", 160}, {"he_aac", 160}, {"he_aac", 160}, {"he_aac", 64}, {"he_aac", 64}},
	} {
		r := sizeBudgetAudioRecord(rep, mode, 1, 1990, 0)
		for i, tr := range r.Tracks {
			if tr.Output != want[i].output || tr.Kbps != want[i].kbps {
				t.Errorf("%s track %d (%s %d kbps): %+v, want %+v", mode, i, tr.Codec, tr.SourceKbps, tr, want[i])
			}
		}
	}
}

// The budget sweep for the eight-track episode: the least compressed level that
// leaves the picture its good bitrate (about 1990 kbps) wins, else level 2.
func TestSizeBudgetAudioLevelSweep(t *testing.T) {
	rep := levelReport()
	for _, tc := range []struct {
		budget    int64
		mode      string
		videoKbps int
	}{
		{600_000_000, transcode.AudioModeSizeHEAACBed, 648},
		{800_000_000, transcode.AudioModeSizeHEAACBed, 1511},
		{1_000_000_000, transcode.AudioModeSizeHEAACBed, 2373},
		{1_200_000_000, transcode.AudioModeSizeHEAACBed, 3235},
		{1_500_000_000, transcode.AudioModeSizeAACBed, 2736},
		{2_000_000_000, transcode.AudioModeSizeAACBed, 4892},
	} {
		enc := planSizeBudgetEncode(levelPolicy(t, float64(tc.budget)), rep, levelSourceBytes, tc.budget)
		if enc.Reason != "" || enc.AudioMode != tc.mode || enc.X265.BitrateKbps != tc.videoKbps {
			t.Errorf("%d bytes: %q at %d kbps (%q), want %q at %d kbps", tc.budget, enc.AudioMode, enc.X265.BitrateKbps, enc.Reason, tc.mode, tc.videoKbps)
		}
	}
}

// The decision to convert a track and the estimate of what it saves read the
// same bitrate: bit_rate (or BPS) first, then NUMBER_OF_BYTES over the duration.
func TestAudioTrackBytesReadsTheBitrateTheDecisionReads(t *testing.T) {
	const file = int64(3_000_000_000)
	both := mediainspect.DetailedStream{BitRate: 640_000, Tags: map[string]string{"NUMBER_OF_BYTES": "100000000"}}
	if got := audioTrackBytes(both, 1800, file); got != 144_000_000 {
		t.Errorf("bit_rate and NUMBER_OF_BYTES: %d, want the bit_rate's 144000000", got)
	}
	if got := transcode.SourceAudioKbps(both.BitRate, both.Tags, 1800); got != 640 {
		t.Errorf("the decision reads %d kbps", got)
	}
	onlyBytes := mediainspect.DetailedStream{Tags: map[string]string{"NUMBER_OF_BYTES": "144000000"}}
	if got := audioTrackBytes(onlyBytes, 1800, file); got != 144_000_000 {
		t.Errorf("NUMBER_OF_BYTES alone: %d", got)
	}
	if got := audioTrackBytes(mediainspect.DetailedStream{BitRate: 640_000}, 1800, 100); got != 0 {
		t.Errorf("a size larger than the file: %d, want unknown", got)
	}
}
