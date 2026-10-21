package action

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestJoyFinalValidationRequiresUpdatedWorker(t *testing.T) {
	b, err := os.ReadFile("../docs/transcode/examples/transcode-profile-joy.json")
	if err != nil {
		t.Fatal(err)
	}
	var profile recipe.Profile
	if err = json.Unmarshal(b, &profile); err != nil {
		t.Fatal(err)
	}
	profile, _, err = recipe.NormalizeAndDigestProfile("joy-1080p-experimental", profile)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := recipe.Parse(recipe.EmbeddedBytes())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := recipe.Resolve(snapshot, "joy-1080p-experimental", map[string]recipe.Profile{"joy-1080p-experimental": profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := &mediainspect.DetailedReport{DurationSec: 100, Video: []mediainspect.DetailedStream{{BitDepth: 8, BitRate: 5000000, FPS: 25}}}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, ExplicitMain10Benchmarks: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"libvmaf": true}, Quality: &transcode.QualityCapabilities{Native10Bit: true, CAMBIFullRef: true, Models: map[string]transcode.QualityModelCapability{"v1_1080p_3h": {Available: true}}}}
	ec := &ExecutionContext{InstanceID: "joy-test", Inputs: map[string]any{"preserve_source_bit_depth": false}, State: map[string]any{"plan": plan, "preserve_source_bit_depth": false}}
	_, err = finalRequestForPlan(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err == nil || !strings.Contains(err.Error(), "two-pass x265") {
		t.Fatalf("old worker accepted new experiment: %v", err)
	}
	caps.X265TwoPass = true
	_, err = finalRequestForPlan(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err == nil || !strings.Contains(err.Error(), "HE-AAC") {
		t.Fatalf("old audio capability accepted: %v", err)
	}
	caps.HEAACAudio = true
	req, err := finalRequestForPlan(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	if req.Metric != "vmaf" || len(req.Samples) == 0 {
		t.Fatalf("unexpected request: %+v", req)
	}
	// The plan, not the request, carries the experiment's encoder settings.
	if plan.X265 == nil || plan.X265.BitrateKbps != 1200 || plan.AudioMode != "he_aac" {
		t.Fatalf("recipe changed: %+v", plan)
	}
}
