package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func TestBuildBenchmarkRequestPreservesObservedAttachmentPayload(t *testing.T) {
	dir := t.TempDir()
	probe, err := os.ReadFile("../mediainspect/testdata/font-attachments-18.json")
	if err != nil {
		t.Fatal(err)
	}
	ffprobe, media := filepath.Join(dir, "ffprobe"), filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(ffprobe, append([]byte("#!/bin/sh\ncat <<'JSON'\n"), append(probe, []byte("\nJSON\n")...)...), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	rep, err := mediainspect.InspectDetailed(context.Background(), ffprobe, media)
	if err != nil {
		t.Fatal(err)
	}
	ec := &ExecutionContext{InstanceID: "attachment-size", Inputs: map[string]any{}, State: map[string]any{"plan": &transcode.Plan{VideoCodec: "libx265", Quality: 20, PreserveAttachments: true}}}
	opt := &recipe.OptimizationPolicy{Sampling: &recipe.SamplingPolicy{SampleCount: 1, SampleSeconds: 5, Positions: []float64{0.5}}, Search: &recipe.SearchPolicy{MaxCandidates: 1, QualityValues: []int{20}}, Quality: &recipe.QualityPolicy{PreferredMetric: "ssim"}}
	caps := transcode.WorkerCapabilities{ProtocolVersion: transcode.WorkerProtocolVersion, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"ssim": true}}
	req, err := buildBenchmarkRequest(ec, media, &rep, opt, caps)
	if err != nil {
		t.Fatal(err)
	}
	if req.AttachmentBytes != 5046808 {
		t.Fatalf("lost 18 font payloads: %d", req.AttachmentBytes)
	}
	withAttachments, err := transcode.DigestBenchmarkRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.AttachmentBytes = 0
	withoutAttachments, err := transcode.DigestBenchmarkRequest(req)
	if err != nil || withAttachments == withoutAttachments {
		t.Fatalf("request identity lost attachment bytes: %v", err)
	}
	rep.Attachments[0].ExtraDataSize = 0
	if _, err := buildBenchmarkRequest(ec, media, &rep, opt, caps); err == nil || !strings.Contains(err.Error(), "attachment payload size") {
		t.Fatalf("unknown payload silently counted as zero: %v", err)
	}
}
