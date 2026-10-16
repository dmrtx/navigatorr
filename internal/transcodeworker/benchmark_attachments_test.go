package transcodeworker

import (
	"context"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func TestBenchmarkSelectionIncludesPreservedAttachmentPayload(t *testing.T) {
	for _, tc := range []struct {
		name        string
		supplied    int64
		attachments []mediainspect.DetailedStream
		want        int64
		wantErr     bool
	}{
		{"coordinator supplies size to existing worker", 5046808, nil, 5046808, false},
		{"native inspection fallback", 0, []mediainspect.DetailedStream{{ExtraDataSize: 5046808}}, 5046808, false},
		{"legacy tag fallback", 0, []mediainspect.DetailedStream{{Tags: map[string]string{"NUMBER_OF_BYTES": "5046808"}}}, 5046808, false},
		{"count supplied payload only once", 5046808, []mediainspect.DetailedStream{{ExtraDataSize: 5046808}}, 5046808, false},
		{"absent", 0, nil, 0, false},
		{"present with unknown size", 0, []mediainspect.DetailedStream{{Index: 12}}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := transcode.BenchmarkCandidate{ID: "q20", VideoCodec: "libx265", Quality: 20}
			record := &BenchmarkRecord{Metric: "vmaf", SourceDuration: 1440, AttachmentBytes: tc.supplied, Candidates: []transcode.BenchmarkCandidate{candidate}, Samples: []transcode.BenchmarkSampleWindow{{Index: 0, DurationSeconds: 1440}}}
			evidence := &BenchmarkExecutionEvidence{SourceBitDepth: 8, CandidateSamples: []BenchmarkCandidateSampleResult{{CandidateID: "q20", SampleIndex: 0, SizeBytes: 190342032}}, CandidateMetrics: []BenchmarkCandidateMetricAggregate{{CandidateID: "q20", MetricType: optimization.MetricTypeVMAF, Aggregate: optimization.MetricAggregate{MetricType: optimization.MetricTypeVMAF, Valid: true, MeanScore: 97, SampleScores: []optimization.SampleScore{{SampleIndex: 0, Score: 97, Valid: true}}}}}}
			rep := mediainspect.DetailedReport{SizeBytes: 1477215721, DurationSec: 1440, Attachments: tc.attachments, Audio: []mediainspect.DetailedStream{{Index: 1, Codec: "aac", Channels: 2, BitRate: 189375}}, Subtitles: []mediainspect.DetailedStream{{Index: 2, Codec: "ass", Tags: map[string]string{"NUMBER_OF_BYTES": "223902"}}}}
			err := (&ProductionBenchmarkRunner{}).runSelection(context.Background(), record, evidence, []validatedCandidate{{candidate: candidate, bitDepth: 8, profile: "main", pixelFormat: "yuv420p"}}, rep, rep.SizeBytes)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "attachment payload size") {
					t.Fatalf("unknown bytes accepted: %v", err)
				}
				return
			}
			if err != nil || evidence.Decision == nil || evidence.Decision.Winner == nil {
				t.Fatalf("selection failed: %v, %+v", err, evidence.Decision)
			}
			winner := evidence.Decision.Winner
			wantTotal := int64(225776701)
			if tc.want > 0 {
				wantTotal = 230848743
			}
			if winner.EstimatedAttachmentBytes != tc.want || winner.EstimatedTotalBytes != wantTotal {
				t.Fatalf("estimate dropped or duplicated attachment payload: %+v; want attachments=%d total=%d", winner, tc.want, wantTotal)
			}
		})
	}
}
