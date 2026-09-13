package recipe

import (
	"strings"
	"testing"
)

func TestRecipeSchemaV1_BackwardCompatibility(t *testing.T) {
	v1YAML := `
schema_version: 1
bundle_version: "2026.09.2"
containers:
  mkv:
    subtitle_copy: ["subrip", "ass"]
profiles:
  test-profile:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience:
      max_attempts: 1
`
	snap, err := Parse([]byte(v1YAML))
	if err != nil {
		t.Fatalf("Parse v1 bundle failed: %v", err)
	}
	if snap.Bundle.SchemaVersion != 1 {
		t.Errorf("expected schema_version 1, got %d", snap.Bundle.SchemaVersion)
	}
	p := snap.Bundle.Profiles["test-profile"]
	if p.Optimization != nil {
		t.Errorf("expected nil optimization policy for v1, got %+v", p.Optimization)
	}
}

func TestRecipeSchemaV1_RejectsOptimizationPolicy(t *testing.T) {
	v1WithOptYAML := `
schema_version: 1
bundle_version: "2026.09.2"
containers:
  mkv:
    subtitle_copy: ["subrip"]
profiles:
  opt-profile:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65}
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
      quality_candidates: [60, 65, 70]
`
	_, err := Parse([]byte(v1WithOptYAML))
	if err == nil || !strings.Contains(err.Error(), "optimization policy requires schema_version 2") {
		t.Fatalf("expected error mentioning optimization policy requires schema_version 2, got: %v", err)
	}
}

func TestRecipeSchemaV2_FullOptimizationPolicy(t *testing.T) {
	v2YAML := `
schema_version: 2
bundle_version: "2026.09.2-v2"
containers:
  mkv:
    subtitle_copy: ["subrip", "ass"]
profiles:
  optimized-anime:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
      profile: main10
      pixel_format: p010le
      prioritize_speed: false
      spatial_aq: true
      realtime: false
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience:
      max_attempts: 2
    optimization:
      sampling:
        segment_duration_sec: 15.0
        segment_count: 5
        min_source_duration_sec: 120.0
      thresholds:
        min_vmaf: 92.0
        target_vmaf: 95.5
        min_ssim: 0.97
        target_ssim: 0.99
      quality_candidates: [55, 60, 65, 70, 75]
      bitrate_guidance:
        preferred_bitrate: 4500000
        soft_max_bitrate: 8000000
`
	snap, err := Parse([]byte(v2YAML))
	if err != nil {
		t.Fatalf("Parse v2 bundle failed: %v", err)
	}
	if snap.Bundle.SchemaVersion != 2 {
		t.Errorf("expected schema_version 2, got %d", snap.Bundle.SchemaVersion)
	}
	p, ok := snap.Bundle.Profiles["optimized-anime"]
	if !ok {
		t.Fatalf("missing profile optimized-anime")
	}
	if p.Optimization == nil {
		t.Fatalf("expected optimization policy to be populated")
	}
	opt := p.Optimization

	// Sampling
	if opt.Sampling == nil || opt.Sampling.SegmentDurationSec != 15.0 || opt.Sampling.SegmentCount != 5 || opt.Sampling.MinSourceDurationSec != 120.0 {
		t.Errorf("unexpected sampling policy: %+v", opt.Sampling)
	}

	// Thresholds
	if opt.Thresholds == nil || opt.Thresholds.MinVMAF != 92.0 || opt.Thresholds.TargetVMAF != 95.5 || opt.Thresholds.MinSSIM != 0.97 || opt.Thresholds.TargetSSIM != 0.99 {
		t.Errorf("unexpected metric thresholds: %+v", opt.Thresholds)
	}

	// Quality Candidates
	if len(opt.QualityCandidates) != 5 || opt.QualityCandidates[0] != 55 || opt.QualityCandidates[4] != 75 {
		t.Errorf("unexpected quality candidates: %v", opt.QualityCandidates)
	}

	// Bitrate Guidance
	if opt.BitrateGuidance == nil || opt.BitrateGuidance.PreferredBitrate != 4500000 || opt.BitrateGuidance.SoftMaxBitrate != 8000000 {
		t.Errorf("unexpected bitrate guidance: %+v", opt.BitrateGuidance)
	}

	// Verify resolution separates optimization policy from Plan
	plan, err := Resolve(snap, "optimized-anime", nil, nil)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if plan.VideoProfile != "main10" || plan.PixelFormat != "p010le" || plan.ExpectedBitDepth != 10 {
		t.Errorf("unexpected plan video knobs: profile=%s pix_fmt=%s bit_depth=%d",
			plan.VideoProfile, plan.PixelFormat, plan.ExpectedBitDepth)
	}
	if plan.PlanDigest == "" {
		t.Errorf("plan digest must not be empty")
	}
}

func TestRecipeSchemaV2_StrictKnownFieldsRejectsArbitraryArgs(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "arbitrary ffmpeg_args in optimization",
			yaml: `
schema_version: 2
bundle_version: "2026.09.2"
containers: {mkv: {subtitle_copy: ["subrip"]}}
profiles:
  p:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65}
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
      ffmpeg_args: ["-b:v", "5M"]
`,
		},
		{
			name: "custom_command in sampling",
			yaml: `
schema_version: 2
bundle_version: "2026.09.2"
containers: {mkv: {subtitle_copy: ["subrip"]}}
profiles:
  p:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65}
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
      sampling:
        custom_command: "ffmpeg -i in.mkv out.mkv"
`,
		},
		{
			name: "unverified encoder arg in video block",
			yaml: `
schema_version: 2
bundle_version: "2026.09.2"
containers: {mkv: {subtitle_copy: ["subrip"]}}
profiles:
  p:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65, bitrate: "5M"}
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected strict KnownFields error for %s, got nil", tc.name)
			}
		})
	}
}

func TestRecipeSchemaV2_OptimizationValidationBounds(t *testing.T) {
	baseYAML := func(optBlock string) string {
		return `
schema_version: 2
bundle_version: "2026.09.2"
containers:
  mkv:
    subtitle_copy: ["subrip"]
profiles:
  test-opt:
    container: mkv
    video: {codec: hevc_videotoolbox, quality: 65}
    audio: {mode: copy}
    subtitles: {mode: preserve, convert_incompatible: true}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
` + optBlock
	}

	tests := []struct {
		name      string
		opt       string
		errSubstr string
	}{
		{
			name:      "vmaf out of range high",
			opt:       "      thresholds: {min_vmaf: 105.0}",
			errSubstr: "min_vmaf 105 out of range 0-100",
		},
		{
			name:      "vmaf target less than min",
			opt:       "      thresholds: {min_vmaf: 95.0, target_vmaf: 90.0}",
			errSubstr: "target_vmaf (90) must be >= min_vmaf (95)",
		},
		{
			name:      "ssim out of range high",
			opt:       "      thresholds: {min_ssim: 1.5}",
			errSubstr: "min_ssim 1.5 out of range 0.0-1.0",
		},
		{
			name:      "ssim target less than min",
			opt:       "      thresholds: {min_ssim: 0.98, target_ssim: 0.95}",
			errSubstr: "target_ssim (0.95) must be >= min_ssim (0.98)",
		},
		{
			name:      "quality candidate out of range high",
			opt:       "      quality_candidates: [50, 105]",
			errSubstr: "quality candidate 105 out of range 1-100",
		},
		{
			name:      "quality candidate out of range low",
			opt:       "      quality_candidates: [0, 50]",
			errSubstr: "quality candidate 0 out of range 1-100",
		},
		{
			name:      "duplicate quality candidate",
			opt:       "      quality_candidates: [65, 70, 65]",
			errSubstr: "duplicate quality candidate 65",
		},
		{
			name:      "too many quality candidates",
			opt:       "      quality_candidates: [10, 20, 30, 40, 50, 60, 70, 80, 90, 95, 99]",
			errSubstr: "maximum 10 quality candidates allowed",
		},
		{
			name:      "inverted bitrate guidance",
			opt:       "      bitrate_guidance: {preferred_bitrate: 8000000, soft_max_bitrate: 4000000}",
			errSubstr: "soft_max_bitrate (4000000) must be >= preferred_bitrate (8000000)",
		},
		{
			name:      "sampling duration too high",
			opt:       "      sampling: {segment_duration_sec: 500.0}",
			errSubstr: "segment_duration_sec must be between 0 and 300 seconds",
		},
		{
			name:      "sampling count too high",
			opt:       "      sampling: {segment_count: 25}",
			errSubstr: "segment_count out of range",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bundleData := baseYAML(tc.opt)
			_, err := Parse([]byte(bundleData))
			if err == nil || !strings.Contains(err.Error(), tc.errSubstr) {
				t.Fatalf("expected error containing %q, got: %v", tc.errSubstr, err)
			}
		})
	}
}

func TestBuiltinProfiles_AnimeHevcQualityAndCurrentProfilesUnchanged(t *testing.T) {
	snap, err := Parse(EmbeddedBytes())
	if err != nil {
		t.Fatalf("Parse embedded bytes failed: %v", err)
	}

	p, ok := snap.Bundle.Profiles["anime-hevc-quality"]
	if !ok {
		t.Fatalf("missing profile anime-hevc-quality")
	}

	if p.Video.Codec != "hevc_videotoolbox" {
		t.Errorf("expected codec hevc_videotoolbox, got %s", p.Video.Codec)
	}
	if p.Video.Quality != 75 {
		t.Errorf("expected quality 75, got %d", p.Video.Quality)
	}
	if p.Video.Profile != "main" {
		t.Errorf("expected profile main, got %s", p.Video.Profile)
	}
	if p.Video.PixelFormat != "yuv420p" {
		t.Errorf("expected pixel_format yuv420p, got %s", p.Video.PixelFormat)
	}
	if p.Video.PrioritizeSpeed == nil || *p.Video.PrioritizeSpeed != false {
		t.Errorf("expected prioritize_speed false, got %v", p.Video.PrioritizeSpeed)
	}
	if p.Video.SpatialAQ == nil || *p.Video.SpatialAQ != true {
		t.Errorf("expected spatial_aq true, got %v", p.Video.SpatialAQ)
	}
	if p.Video.Realtime == nil || *p.Video.Realtime != false {
		t.Errorf("expected realtime false, got %v", p.Video.Realtime)
	}
	if p.Optimization != nil {
		t.Errorf("expected anime-hevc-quality optimization policy to remain nil, got %+v", p.Optimization)
	}

	plan, err := Resolve(snap, "anime-hevc-quality", nil, nil)
	if err != nil {
		t.Fatalf("Resolve anime-hevc-quality failed: %v", err)
	}
	if plan.Quality != 75 || plan.VideoProfile != "main" || plan.PixelFormat != "yuv420p" || plan.ExpectedBitDepth != 8 {
		t.Errorf("unexpected resolved plan for anime-hevc-quality: %+v", plan)
	}
}
