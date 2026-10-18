package transcode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

func TestAttachVMAFEvidenceKeepsMeasuredDistributionPerEvaluation(t *testing.T) {
	decision := &BenchmarkDecision{Evaluations: []BenchmarkCandidateEvaluation{{CandidateID: "crf16"}, {CandidateID: "crf20"}, {CandidateID: "skipped"}}}
	evidence := []BenchmarkCandidateQualityEvidence{
		{CandidateID: "crf16", VMAF: &quality.VMAFStats{ModelID: "vmaf_v1", Mean: 94.87, P5: 91.5, Min: 90.3, WorstWindowMean: 92.8, WorstWindowSeconds: 5, FramesTotal: 716, TemporalDecimation: 1}},
		{CandidateID: "crf20", VMAF: &quality.VMAFStats{}},
	}
	AttachVMAFEvidence(decision, evidence)
	got := decision.Evaluations[0].VMAF
	if got == nil || got.Mean != 94.87 || got.P5 != 91.5 || got.Min != 90.3 || got.WorstWindowMean != 92.8 || got.FramesTotal != 716 {
		t.Fatalf("measured distribution not kept: %+v", got)
	}
	if decision.Evaluations[1].VMAF != nil || decision.Evaluations[2].VMAF != nil {
		t.Fatal("summary invented for a candidate without measured frames")
	}
	raw, _ := json.Marshal(decision.Evaluations[0])
	if !strings.Contains(string(raw), `"vmaf":{"model":"vmaf_v1","mean":94.87,"p5":91.5,"min":90.3,"worst_window_mean":92.8`) {
		t.Fatalf("unexpected persisted shape: %s", raw)
	}
	// An already recorded summary is not replaced by later evidence.
	evidence[0].VMAF.Mean = 10
	AttachVMAFEvidence(decision, evidence)
	if decision.Evaluations[0].VMAF.Mean != 94.87 {
		t.Fatal("recorded summary overwritten")
	}
}
