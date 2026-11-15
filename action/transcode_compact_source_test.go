package action

import (
	"context"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// compactReport is audioOnlyReport with its 1080p HEVC video at 1.5 Mbps,
// about 0.03 bits per pixel: already as small as a size goal would make it.
func compactReport() *mediainspect.DetailedReport {
	rep := audioOnlyReport()
	rep.SizeBytes = 10_700_000_000
	rep.Video[0].BitRate = 1_500_000
	return rep
}

func TestCompactVideoSource(t *testing.T) {
	if !compactVideoSource(compactReport()) {
		t.Fatal("1.5 Mbps 1080p HEVC is compact")
	}
	for name, mutate := range map[string]func(*mediainspect.DetailedReport){
		"H.264":              func(r *mediainspect.DetailedReport) { r.Video[0].Codec = "h264" },
		"6 Mbps HEVC":        func(r *mediainspect.DetailedReport) { r.Video[0].BitRate = 6_000_000 },
		"unknown frame rate": func(r *mediainspect.DetailedReport) { r.Video[0].FPS = 0 },
		"unknown size":       func(r *mediainspect.DetailedReport) { r.SizeBytes, r.Video[0].BitRate = 0, 0 },
	} {
		rep := compactReport()
		mutate(rep)
		if compactVideoSource(rep) {
			t.Fatalf("%s counted as compact", name)
		}
	}
	if compactVideoSource(nil) {
		t.Fatal("nil report counted as compact")
	}
}

// A size goal on a compact source copies the video and converts the audio
// instead of running the x265 ladder.
func TestSizeGoalCopiesACompactVideo(t *testing.T) {
	c := newAudioOnlyCase(t, audioOnlyCaps(), nil)
	c.ec.State["source_report"] = compactReport()
	result, handled := c.engine.startCompactSourceAudioOnly(context.Background(), c.ec)
	if !handled || result.Status != StepCompleted || getBool(c.ec.State, "skip_transcode") || QualityLadderOf(c.ec.State) != nil {
		t.Fatalf("result %+v handled=%v state %+v", result, handled, c.ec.State)
	}
	if plan := getPlan(c.ec.State["plan"]); !transcode.IsSizeAudioOnlyPlan(plan) || !getBool(c.ec.State, compactVideoSourceKey) || result.Outputs["plan_note"] == nil {
		t.Fatalf("plan %+v outputs %+v", plan, result.Outputs)
	}

	// The audio alone would not save enough: the job ends without an encode.
	c = newAudioOnlyCase(t, audioOnlyCaps(), nil)
	rep := compactReport()
	rep.Audio = rep.Audio[2:]
	c.ec.State["source_report"] = rep
	result, handled = c.engine.startCompactSourceAudioOnly(context.Background(), c.ec)
	if !handled || result.Status != StepCompleted || !getBool(c.ec.State, "skip_transcode") || getString(c.ec.State, "reason_code") != reasonVideoAlreadyCompact {
		t.Fatalf("lossy-only result %+v state %+v", result, c.ec.State)
	}
	c = newAudioOnlyCase(t, audioOnlyCaps(), nil)
	c.ec.State["source_report"] = compactReport()
	c.ec.State["original_size"] = int64(60_000_000_000)
	if _, handled = c.engine.startCompactSourceAudioOnly(context.Background(), c.ec); !handled || getString(c.ec.State, "reason_code") != reasonVideoAlreadyCompact {
		t.Fatalf("small saving state %+v", c.ec.State)
	}

	// A video that can still shrink goes to the ladder.
	c = newAudioOnlyCase(t, audioOnlyCaps(), nil)
	if _, handled = c.engine.startCompactSourceAudioOnly(context.Background(), c.ec); handled {
		t.Fatal("a 6 Mbps HEVC source skipped the ladder")
	}
}

func TestEstimateCopiesACompactVideoForASizeGoal(t *testing.T) {
	est := EstimateConversion(compactReport(), EstimateSettings{Mode: "size"})
	if est.VideoBasis != "copied" || est.Streams.Video.Fate != "copied" || est.Streams.Video.AfterBytes != est.Streams.Video.BeforeBytes {
		t.Fatalf("estimate %+v video %+v", est, est.Streams.Video)
	}
	if est := EstimateConversion(compactReport(), EstimateSettings{Mode: "size", BudgetPercent: 50}); est.VideoBasis != "size_budget" {
		t.Fatalf("a budget still encodes the video: %+v", est)
	}
}
