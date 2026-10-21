package action

import (
	"context"
	"errors"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

func sdrClassifier(_ context.Context, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
	return mediainspect.ClassifyDynamicRange(rep, nil), nil
}

// A preview must never disagree with the job: every non-compact verdict is
// exactly what modeSourceReason returns for the same report.
func TestSourceGoalsMatchTheJobGates(t *testing.T) {
	e := NewEngine(EngineDeps{})
	for name, stream := range map[string]mediainspect.DetailedStream{
		"sdr 1080p":    {Codec: "h264", BitDepth: 8, FPS: 24, Width: 1920, Height: 1080},
		"hevc":         {Codec: "hevc", BitDepth: 10, FPS: 24, Width: 1920, Height: 1080},
		"hdr":          {Codec: "hevc", BitDepth: 10, FPS: 24, Width: 3840, Height: 2160, ColorTransfer: "smpte2084"},
		"12 bit":       {Codec: "h264", BitDepth: 12, FPS: 24, Width: 1920, Height: 1080},
		"interlaced":   {Codec: "h264", BitDepth: 8, FPS: 50, FieldOrder: "tt", Width: 1920, Height: 1080},
		"4k sdr":       {Codec: "hevc", BitDepth: 10, FPS: 24, Width: 3840, Height: 2160},
		"no framerate": {Codec: "h264", BitDepth: 8, Width: 1920, Height: 1080},
	} {
		rep := &mediainspect.DetailedReport{Probed: true, Video: []mediainspect.DetailedStream{stream}}
		got := e.SourceGoals(context.Background(), "/media/movie.mkv", rep, sdrClassifier)
		for _, mode := range []string{"size", "quality", "x265_preserve"} {
			want := modeSourceReason(mode, rep, false)
			v := got.Goals[mode]
			if (want == "") != (v.Status == GoalOK) || v.ReasonCode != want {
				t.Fatalf("%s/%s: job says %q, preview %+v", name, mode, want, v)
			}
		}
	}
}

func TestSourceGoalsRecommendation(t *testing.T) {
	e := NewEngine(EngineDeps{})
	sdr1080 := &mediainspect.DetailedReport{Probed: true, Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: 24, Width: 1920, Height: 1080}}}
	if g := e.SourceGoals(context.Background(), "/media/movie.mkv", sdr1080, sdrClassifier); g.Recommended != "size" || g.Goals["compact"].ReasonCode != "compact_not_needed" {
		t.Fatalf("%+v", g)
	}
	sdr4k := &mediainspect.DetailedReport{Probed: true, Video: []mediainspect.DetailedStream{{Codec: "hevc", BitDepth: 10, FPS: 24, Width: 3840, Height: 2160}}}
	if g := e.SourceGoals(context.Background(), "/media/movie.mkv", sdr4k, sdrClassifier); g.Recommended != "compact" || g.Goals["compact"].Status != GoalOK {
		t.Fatalf("%+v", g)
	}
	// Nothing recommended when the only converting goal could not be judged.
	hdr := &mediainspect.DetailedReport{Probed: true, Video: []mediainspect.DetailedStream{{Codec: "hevc", BitDepth: 10, FPS: 24, Width: 3840, Height: 2160, ColorTransfer: "smpte2084"}}}
	failing := func(context.Context, *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
		return mediainspect.DynamicRange{}, errors.New("no frames")
	}
	g := e.SourceGoals(context.Background(), "/media/movie.mkv", hdr, failing)
	if g.Goals["compact"].Status != GoalUnknown || g.Goals["size"].ReasonCode != "hdr_requires_compact" || g.Recommended != "" || g.Dynamic != nil {
		t.Fatalf("%+v", g)
	}
}

func TestSourceGoalsLeaveHFRUnknownWhenTheWorkerCannotBeAsked(t *testing.T) {
	hfr := &mediainspect.DetailedReport{Probed: true, Video: []mediainspect.DetailedStream{{Codec: "h264", BitDepth: 8, FPS: 59.94, Width: 1920, Height: 1080}}}
	down := &mockTranscodeExecutor{capabilitiesFunc: func(context.Context) (transcode.WorkerCapabilities, error) {
		return transcode.WorkerCapabilities{}, errors.New("worker unreachable")
	}}
	g := NewEngine(EngineDeps{Transcode: down}).SourceGoals(context.Background(), "/media/movie.mkv", hfr, sdrClassifier)
	for _, mode := range []string{"size", "quality", "x265_preserve"} {
		if v := g.Goals[mode]; v.Status != GoalUnknown {
			t.Fatalf("%s: %+v", mode, v)
		}
	}
	// Without an executor the job itself would skip, so the preview says so.
	g = NewEngine(EngineDeps{}).SourceGoals(context.Background(), "/media/movie.mkv", hfr, sdrClassifier)
	if v := g.Goals["size"]; v.Status != GoalSkip || v.ReasonCode != "hfr_quality_unavailable" {
		t.Fatalf("%+v", v)
	}
}
