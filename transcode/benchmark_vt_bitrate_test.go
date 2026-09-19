package transcode

import (
	"strings"
	"testing"
)

func vtBitrateCandidates() []BenchmarkCandidate {
	t := true
	return []BenchmarkCandidate{
		{ID: "cand_br3200k", VideoCodec: VideoCodecHEVCVideoToolbox, AverageBitrateKbps: 3200, VideoProfile: "main", PixelFormat: "yuv420p", PrioritizeSpeed: &t, SpatialAQ: &t},
		{ID: "cand_br3500k", VideoCodec: VideoCodecHEVCVideoToolbox, AverageBitrateKbps: 3500, VideoProfile: "main", PixelFormat: "yuv420p", PrioritizeSpeed: &t, SpatialAQ: &t},
		{ID: "cand_br3800k", VideoCodec: VideoCodecHEVCVideoToolbox, AverageBitrateKbps: 3800, VideoProfile: "main", PixelFormat: "yuv420p", PrioritizeSpeed: &t, SpatialAQ: &t},
	}
}

func TestValidateBenchmarkRequest_VTBitrateMode(t *testing.T) {
	req := validTestBenchmarkRequest()
	req.Candidates = vtBitrateCandidates()
	if err := ValidateBenchmarkRequest(&req); err != nil {
		t.Fatalf("expected valid VT bitrate request, got error: %v", err)
	}
}

func TestValidateBenchmarkRequest_VTRateModeFailClosed(t *testing.T) {
	newReq := func(cands []BenchmarkCandidate) BenchmarkRequest {
		req := validTestBenchmarkRequest()
		req.Candidates = cands
		return req
	}
	cases := []struct {
		name  string
		cands []BenchmarkCandidate
		want  string
	}{
		{
			name:  "quality and bitrate together",
			cands: []BenchmarkCandidate{{ID: "c1", Quality: 65, AverageBitrateKbps: 3500}},
			want:  "mutually exclusive",
		},
		{
			name:  "neither quality nor bitrate",
			cands: []BenchmarkCandidate{{ID: "c1", VideoProfile: "main"}},
			want:  "must specify either quality",
		},
		{
			name: "mixed quality and bitrate candidates",
			cands: []BenchmarkCandidate{
				{ID: "c1", Quality: 65},
				{ID: "c2", AverageBitrateKbps: 3500},
			},
			want: "mixed rate-control",
		},
		{
			name:  "duplicate bitrates",
			cands: []BenchmarkCandidate{{ID: "c1", AverageBitrateKbps: 3500}, {ID: "c2", AverageBitrateKbps: 3500}},
			want:  "duplicate candidate quality 0 (bitrate 3500)",
		},
		{
			name:  "constant bitrate without average",
			cands: []BenchmarkCandidate{{ID: "c1", Quality: 65, ConstantBitrate: boolPtr(true)}},
			want:  "constant_bitrate without average_bitrate_kbps",
		},
		{
			name:  "maxrate without average",
			cands: []BenchmarkCandidate{{ID: "c1", Quality: 65, MaxBitrateKbps: 5000}},
			want:  "max_bitrate_kbps without average_bitrate_kbps",
		},
		{
			name:  "maxrate below average",
			cands: []BenchmarkCandidate{{ID: "c1", AverageBitrateKbps: 3500, MaxBitrateKbps: 3000}},
			want:  "must be >= average_bitrate_kbps",
		},
		{
			name:  "qmin above qmax",
			cands: []BenchmarkCandidate{{ID: "c1", Quality: 65, QMin: intPtr(40), QMax: intPtr(30)}},
			want:  "qmin (40) must be <= qmax (30)",
		},
		{
			name:  "preset for videotoolbox",
			cands: []BenchmarkCandidate{{ID: "c1", Quality: 65, Preset: "slow"}},
			want:  "only supported for libx265",
		},
		{
			name:  "bitrate for libx265",
			cands: []BenchmarkCandidate{{ID: "c1", VideoCodec: VideoCodecLibX265, Quality: 24, Preset: "slow", AverageBitrateKbps: 3500}},
			want:  "only supported for hevc_videotoolbox, not libx265",
		},
		{
			name:  "qmin for libx265",
			cands: []BenchmarkCandidate{{ID: "c1", VideoCodec: VideoCodecLibX265, Quality: 24, QMin: intPtr(0)}},
			want:  "only supported for hevc_videotoolbox, not libx265",
		},
		{
			name:  "spatial aq for libx265",
			cands: []BenchmarkCandidate{{ID: "c1", VideoCodec: VideoCodecLibX265, Quality: 24, SpatialAQ: boolPtr(true)}},
			want:  "not supported for libx265",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newReq(tc.cands)
			if err := ValidateBenchmarkRequest(&req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

func intPtr(v int) *int { return &v }
