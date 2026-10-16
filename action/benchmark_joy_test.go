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

func TestJoyBenchmarkRequiresUpdatedWorkerAndPreservesABR(t *testing.T) {
	b, err := os.ReadFile("../docs/examples/transcode-profile-joy.json")
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
	_, err = buildBenchmarkRequest(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err == nil || !strings.Contains(err.Error(), "two-pass x265") {
		t.Fatalf("old worker accepted new experiment: %v", err)
	}
	caps.X265TwoPass = true
	_, err = buildBenchmarkRequest(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err == nil || !strings.Contains(err.Error(), "HE-AAC") {
		t.Fatalf("old audio capability accepted: %v", err)
	}
	caps.HEAACAudio = true
	req, err := buildBenchmarkRequest(ec, "/media/source.mkv", rep, profile.Optimization, caps)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Candidates) != 1 || req.Candidates[0].X265 == nil || req.Candidates[0].X265.BitrateKbps != 1200 || req.Candidates[0].Quality != 0 || req.AudioMode != "he_aac" || !req.Allow8BitTo10Bit {
		t.Fatalf("recipe changed: %+v", req)
	}
	digest, _ := transcode.DigestBenchmarkRequest(req)
	req.Candidates[0].X265.BitrateKbps = 1300
	if plan.X265.BitrateKbps != 1200 {
		t.Fatal("candidate mutated immutable base plan")
	}
	if changed, _ := transcode.DigestBenchmarkRequest(req); changed == digest {
		t.Fatal("request identity ignores ABR settings")
	}
}
