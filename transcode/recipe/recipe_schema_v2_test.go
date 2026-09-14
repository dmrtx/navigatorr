package recipe

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

const sampleValidV1YAML = `
schema_version: 1
bundle_version: "v1.0"
containers:
  mkv:
    subtitle_copy: [subrip, ass]
profiles:
  anime-hevc-quality:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
      profile: main10
      pixel_format: p010le
    audio:
      mode: copy
    subtitles:
      mode: preserve
      convert_incompatible: true
    preserve:
      metadata: true
      chapters: true
      attachments: true
    resilience:
      max_attempts: 3
      transient_retries: 2
      retry_backoff_seconds: [1, 2]
      max_fallbacks: 1
      fallbacks:
        - when: container_subtitle_incompatible
          action: apply_container_conversion
`

const sampleValidV2YAML = `
schema_version: 2
bundle_version: "v2.0"
containers:
  mkv:
    subtitle_copy: [subrip, ass]
profiles:
  anime-hevc-quality:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
      profile: main10
      pixel_format: p010le
    audio:
      mode: copy
    subtitles:
      mode: preserve
      convert_incompatible: true
    preserve:
      metadata: true
      chapters: true
      attachments: true
    resilience:
      max_attempts: 3
      transient_retries: 2
      retry_backoff_seconds: [1, 2]
      max_fallbacks: 1
      fallbacks:
        - when: container_subtitle_incompatible
          action: apply_container_conversion
    optimization:
      enabled: true
      sampling:
        strategy: uniform
        sample_count: 3
        sample_seconds: 10.0
        positions: [0.2, 0.5, 0.8]
      quality:
        preferred_metric: vmaf
        vmaf:
          target: 95.0
          minimum: 93.0
          marginal_tolerance: 0.5
        ssim:
          target: 0.98
          minimum: 0.96
          marginal_tolerance: 0.005
      search:
        max_candidates: 5
        quality_values: [55, 60, 65, 70, 75]
      size:
        preferred_total_bitrate_kbps:
          min: 2000
          max: 6000
        soft_max_total_bitrate_kbps: 8000
`

func TestRecipeSchemaConstants(t *testing.T) {
	if MinSchemaVersion != 1 {
		t.Errorf("expected MinSchemaVersion=1, got %d", MinSchemaVersion)
	}
	if LatestSchemaVersion != 2 {
		t.Errorf("expected LatestSchemaVersion=2, got %d", LatestSchemaVersion)
	}
	if SupportedSchemaVersionV1 != 1 {
		t.Errorf("expected SupportedSchemaVersionV1=1, got %d", SupportedSchemaVersionV1)
	}
	if SupportedSchemaVersionV2 != 2 {
		t.Errorf("expected SupportedSchemaVersionV2=2, got %d", SupportedSchemaVersionV2)
	}
	if SupportedSchemaVersion != LatestSchemaVersion {
		t.Errorf("expected SupportedSchemaVersion to alias LatestSchemaVersion (%d), got %d", LatestSchemaVersion, SupportedSchemaVersion)
	}
}

func TestRecipeSchemaV1_FullCompatibility(t *testing.T) {
	snap, err := Parse([]byte(sampleValidV1YAML))
	if err != nil {
		t.Fatalf("Parse of v1 schema failed: %v", err)
	}
	if snap.Bundle.SchemaVersion != SupportedSchemaVersionV1 {
		t.Errorf("expected schema_version %d, got %d", SupportedSchemaVersionV1, snap.Bundle.SchemaVersion)
	}
	p := snap.Bundle.Profiles["anime-hevc-quality"]
	if p.Optimization != nil {
		t.Errorf("expected optimization to be nil in v1 recipe")
	}
}

func TestRecipeSchemaV1_RejectsOptimizationPolicy(t *testing.T) {
	yamlWithOpt := strings.Replace(sampleValidV1YAML, "resilience:", `optimization:
      enabled: true
    resilience:`, 1)

	_, err := Parse([]byte(yamlWithOpt))
	if err == nil || !strings.Contains(err.Error(), "optimization policy requires schema_version 2") {
		t.Fatalf("expected schema_version 2 requirement error, got: %v", err)
	}
}

func TestRecipeSchemaV2_FullValidationAndNormalization(t *testing.T) {
	snap, err := Parse([]byte(sampleValidV2YAML))
	if err != nil {
		t.Fatalf("Parse of v2 schema failed: %v", err)
	}
	if snap.Bundle.SchemaVersion != SupportedSchemaVersionV2 {
		t.Errorf("expected schema_version %d, got %d", SupportedSchemaVersionV2, snap.Bundle.SchemaVersion)
	}
	p := snap.Bundle.Profiles["anime-hevc-quality"]
	if p.Optimization == nil {
		t.Fatalf("expected optimization to be populated")
	}
	opt := p.Optimization
	if !opt.Enabled {
		t.Errorf("expected optimization.enabled=true")
	}
	if opt.Sampling.Strategy != "uniform" || opt.Sampling.SampleCount != 3 || opt.Sampling.SampleSeconds != 10.0 {
		t.Errorf("unexpected sampling config: %+v", opt.Sampling)
	}
	if opt.Quality.PreferredMetric != "vmaf" || opt.Quality.VMAF.Target != 95.0 || opt.Quality.VMAF.MarginalTolerance == nil || *opt.Quality.VMAF.MarginalTolerance != 0.5 {
		t.Errorf("unexpected quality config: %+v", opt.Quality)
	}
	if opt.Quality.SSIM.Target != 0.98 || opt.Quality.SSIM.MarginalTolerance == nil || *opt.Quality.SSIM.MarginalTolerance != 0.005 {
		t.Errorf("unexpected ssim config: %+v", opt.Quality.SSIM)
	}
	if opt.Search.MaxCandidates != 5 || len(opt.Search.QualityValues) != 5 {
		t.Errorf("unexpected search config: %+v", opt.Search)
	}
	if opt.Size.PreferredTotalBitrateKbps.Min != 2000 || opt.Size.PreferredTotalBitrateKbps.Max != 6000 || opt.Size.SoftMaxTotalBitrateKbps != 8000 {
		t.Errorf("unexpected size config: %+v", opt.Size)
	}
}

func TestRecipeSchemaV2_DocumentedDefaultsThroughNormalization(t *testing.T) {
	yamlMinimalOpt := `
schema_version: 2
bundle_version: "v2.0"
containers:
  mkv:
    subtitle_copy: [subrip]
profiles:
  test-profile:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
    audio: {mode: copy}
    subtitles: {mode: preserve}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
      enabled: true
`
	snap, err := Parse([]byte(yamlMinimalOpt))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	opt := snap.Bundle.Profiles["test-profile"].Optimization
	if opt == nil {
		t.Fatalf("expected optimization to be non-nil")
	}
	// Verify documented defaults were populated through normalization
	if opt.Sampling == nil || opt.Sampling.Strategy != DefaultSamplingStrategy || opt.Sampling.SampleCount != DefaultSampleCount || opt.Sampling.SampleSeconds != DefaultSampleSeconds {
		t.Errorf("sampling defaults not normalized: %+v", opt.Sampling)
	}
	if opt.Quality == nil || opt.Quality.PreferredMetric != DefaultPreferredMetric || opt.Quality.VMAF.Target != DefaultVMAFTarget || opt.Quality.VMAF.Minimum != DefaultVMAFMinimum || opt.Quality.VMAF.MarginalTolerance == nil || *opt.Quality.VMAF.MarginalTolerance != DefaultVMAFMarginalTolerance {
		t.Errorf("quality defaults not normalized: %+v", opt.Quality)
	}
	if opt.Quality.SSIM.Target != DefaultSSIMTarget || opt.Quality.SSIM.Minimum != DefaultSSIMMinimum || opt.Quality.SSIM.MarginalTolerance == nil || *opt.Quality.SSIM.MarginalTolerance != DefaultSSIMMarginalTolerance {
		t.Errorf("ssim defaults not normalized: %+v", opt.Quality.SSIM)
	}
	if opt.Search == nil || opt.Search.MaxCandidates != DefaultMaxCandidates || len(opt.Search.QualityValues) != len(DefaultQualityValues) {
		t.Errorf("search defaults not normalized: %+v", opt.Search)
	}
}

func TestRecipeSchemaV2_DisabledOptimizationAllowedWithoutConfig(t *testing.T) {
	yamlDisabledOpt := `
schema_version: 2
bundle_version: "v2.0"
containers:
  mkv:
    subtitle_copy: [subrip]
profiles:
  test-profile:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
    audio: {mode: copy}
    subtitles: {mode: preserve}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
      enabled: false
`
	snap, err := Parse([]byte(yamlDisabledOpt))
	if err != nil {
		t.Fatalf("Parse of disabled optimization should succeed: %v", err)
	}
	opt := snap.Bundle.Profiles["test-profile"].Optimization
	if opt.Enabled {
		t.Errorf("expected optimization.enabled=false")
	}
}

func TestRecipeSchemaV2_StrictKnownFieldsRejectsOldFlattenedSchema(t *testing.T) {
	oldFlattenedYAML := `
schema_version: 2
bundle_version: "v2.0"
containers:
  mkv:
    subtitle_copy: [subrip]
profiles:
  test-profile:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
    audio: {mode: copy}
    subtitles: {mode: preserve}
    preserve: {metadata: true, chapters: true, attachments: true}
    resilience: {max_attempts: 1}
    optimization:
      thresholds:
        target_vmaf: 95.0
`
	_, err := Parse([]byte(oldFlattenedYAML))
	if err == nil || (!strings.Contains(err.Error(), "thresholds") && !strings.Contains(err.Error(), "not found")) {
		t.Fatalf("expected KnownFields rejection of old flattened 'thresholds' field, got: %v", err)
	}
}

func TestRecipeOptimizationPolicy_ValidationFailures(t *testing.T) {
	cases := []struct {
		name        string
		opt         OptimizationPolicy
		errContains string
	}{
		{
			name: "non-finite float in sample_seconds",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   2,
					SampleSeconds: math.NaN(),
					Positions:     []float64{0.2, 0.8},
				},
			},
			errContains: "sample_seconds",
		},
		{
			name: "sampling positions count mismatch",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   3,
					SampleSeconds: 10.0,
					Positions:     []float64{0.2, 0.8},
				},
			},
			errContains: "positions length (2) must match sample_count (3)",
		},
		{
			name: "sampling positions not strictly increasing",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   2,
					SampleSeconds: 10.0,
					Positions:     []float64{0.8, 0.2},
				},
			},
			errContains: "must be strictly increasing",
		},
		{
			name: "sampling position out of (0, 1) range",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{1.5},
				},
			},
			errContains: "out of range (0.0, 1.0)",
		},
		{
			name: "vmaf target less than minimum",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 90.0, Minimum: 95.0},
				},
			},
			errContains: "vmaf target (90) must be >= minimum (95)",
		},
		{
			name: "ssim target less than minimum",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "ssim",
					SSIM:            &MetricTarget{Target: 0.90, Minimum: 0.95},
				},
			},
			errContains: "ssim target (0.9) must be >= minimum (0.95)",
		},
		{
			name: "duplicate quality candidates",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 95.0, Minimum: 93.0},
				},
				Search: &SearchPolicy{
					MaxCandidates: 5,
					QualityValues: []int{60, 60, 70},
				},
			},
			errContains: "quality_values must be strictly ordered without duplicates",
		},
		{
			name: "unordered quality candidates",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 95.0, Minimum: 93.0},
				},
				Search: &SearchPolicy{
					MaxCandidates: 5,
					QualityValues: []int{70, 60},
				},
			},
			errContains: "quality_values must be strictly ordered without duplicates",
		},
		{
			name: "candidate count exceeds max_candidates",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 95.0, Minimum: 93.0},
				},
				Search: &SearchPolicy{
					MaxCandidates: 2,
					QualityValues: []int{50, 60, 70},
				},
			},
			errContains: "exceeds max_candidates",
		},
		{
			name: "preferred bitrate min exceeds max",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 95.0, Minimum: 93.0},
				},
				Search: &SearchPolicy{
					MaxCandidates: 3,
					QualityValues: []int{50, 60},
				},
				Size: &SizePolicy{
					PreferredTotalBitrateKbps: &BitrateRange{Min: 5000, Max: 3000},
				},
			},
			errContains: "preferred_total_bitrate_kbps max (3000) must be >= min (5000)",
		},
		{
			name: "soft_max_total_bitrate_kbps less than preferred max",
			opt: OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "uniform",
					SampleCount:   1,
					SampleSeconds: 10.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 95.0, Minimum: 93.0},
				},
				Search: &SearchPolicy{
					MaxCandidates: 3,
					QualityValues: []int{50, 60},
				},
				Size: &SizePolicy{
					PreferredTotalBitrateKbps: &BitrateRange{Min: 3000, Max: 5000},
					SoftMaxTotalBitrateKbps:   4000,
				},
			},
			errContains: "soft_max_total_bitrate_kbps (4000) must be >= preferred_total_bitrate_kbps max (5000)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			optCopy := tc.opt
			err := ValidateOptimizationPolicy("test", &optCopy)
			if err == nil || !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("expected error containing %q, got: %v", tc.errContains, err)
			}
		})
	}
}

func TestRecipeSchemaV2_JSONRoundTrip(t *testing.T) {
	vmafTol := 0.5
	opt := &OptimizationPolicy{
		Enabled: true,
		Sampling: &SamplingPolicy{
			Strategy:      "uniform",
			SampleCount:   3,
			SampleSeconds: 15.0,
			Positions:     []float64{0.25, 0.5, 0.75},
		},
		Quality: &QualityPolicy{
			PreferredMetric: "vmaf",
			VMAF:            &MetricTarget{Target: 96.0, Minimum: 94.0, MarginalTolerance: &vmafTol},
		},
		Search: &SearchPolicy{
			MaxCandidates: 3,
			QualityValues: []int{60, 65, 70},
		},
		Size: &SizePolicy{
			PreferredTotalBitrateKbps: &BitrateRange{Min: 2000, Max: 5000},
			SoftMaxTotalBitrateKbps:   6000,
		},
	}

	data, err := json.Marshal(opt)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded OptimizationPolicy
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if decoded.Sampling.SampleSeconds != 15.0 || decoded.Quality.VMAF.Target != 96.0 || decoded.Quality.VMAF.MarginalTolerance == nil || *decoded.Quality.VMAF.MarginalTolerance != 0.5 {
		t.Errorf("roundtrip data mismatch: %+v", decoded)
	}
}

func TestRecipeOptimizationPolicy_StrictNormalizationAndZeroTolerance(t *testing.T) {
	zeroTol := 0.0
	opt := &OptimizationPolicy{
		Enabled: true,
		Sampling: &SamplingPolicy{
			Strategy:      "  DISTRIBUTED  ",
			SampleSeconds: -10.0, // Invalid negative number: normalization must NOT overwrite with default!
		},
		Quality: &QualityPolicy{
			PreferredMetric: "  VMAF  ",
			VMAF: &MetricTarget{
				Target:            96.0,
				Minimum:           95.0,
				MarginalTolerance: &zeroTol, // Explicit zero tolerance must NOT be overwritten!
			},
		},
	}
	NormalizeOptimizationPolicy(opt)

	// Verify trimming and lowercasing
	if opt.Sampling.Strategy != "distributed" {
		t.Errorf("expected sampling strategy to be lowercased 'distributed', got %q", opt.Sampling.Strategy)
	}
	if opt.Quality.PreferredMetric != "vmaf" {
		t.Errorf("expected preferred metric to be lowercased 'vmaf', got %q", opt.Quality.PreferredMetric)
	}

	// Verify explicit zero tolerance was preserved
	if opt.Quality.VMAF.MarginalTolerance == nil || *opt.Quality.VMAF.MarginalTolerance != 0.0 {
		t.Errorf("expected zero tolerance 0.0 to be preserved, got %v", opt.Quality.VMAF.MarginalTolerance)
	}

	// Verify negative sample_seconds was NOT replaced by default 20.0
	if opt.Sampling.SampleSeconds != -10.0 {
		t.Errorf("expected negative sample_seconds -10.0 to be retained, got %v", opt.Sampling.SampleSeconds)
	}

	// Validation must fail closed on negative sample_seconds
	err := ValidateOptimizationPolicy("test", opt)
	if err == nil || !strings.Contains(err.Error(), "sample_seconds") {
		t.Errorf("expected validation failure on negative sample_seconds, got: %v", err)
	}
}

func TestRecipeOptimizationPolicy_CloneIndependence(t *testing.T) {
	tol := 0.5
	orig := &OptimizationPolicy{
		Enabled: true,
		Sampling: &SamplingPolicy{
			Strategy:      "distributed",
			SampleCount:   3,
			SampleSeconds: 20.0,
			Positions:     []float64{0.2, 0.5, 0.8},
		},
		Quality: &QualityPolicy{
			PreferredMetric: "vmaf",
			VMAF:            &MetricTarget{Target: 96.0, Minimum: 95.0, MarginalTolerance: &tol},
		},
		Search: &SearchPolicy{
			MaxCandidates: 5,
			QualityValues: []int{55, 60, 65, 70, 75},
		},
	}

	cloned := orig.Clone()
	if cloned == nil {
		t.Fatalf("expected non-nil clone")
	}

	// Mutate clone
	cloned.Sampling.Positions[0] = 0.99
	*cloned.Quality.VMAF.MarginalTolerance = 1.0
	cloned.Search.QualityValues[0] = 999

	// Orig must remain untouched
	if orig.Sampling.Positions[0] == 0.99 {
		t.Errorf("mutation of cloned positions mutated orig")
	}
	if *orig.Quality.VMAF.MarginalTolerance == 1.0 {
		t.Errorf("mutation of cloned marginal tolerance mutated orig")
	}
	if orig.Search.QualityValues[0] == 999 {
		t.Errorf("mutation of cloned quality values mutated orig")
	}
}

func TestRecipeOptimizationPolicy_MetricTargetOmissionSafety(t *testing.T) {
	// 1. Empty vmaf block: vmaf: {}
	optEmptyVMAF := &OptimizationPolicy{
		Enabled: true,
		Sampling: &SamplingPolicy{
			Strategy:      "distributed",
			SampleCount:   3,
			SampleSeconds: 20.0,
			Positions:     []float64{0.2, 0.5, 0.8},
		},
		Quality: &QualityPolicy{
			PreferredMetric: "vmaf",
			VMAF:            &MetricTarget{}, // empty block
		},
		Search: &SearchPolicy{
			MaxCandidates: 3,
			QualityValues: []int{60, 65, 70},
		},
	}
	NormalizeOptimizationPolicy(optEmptyVMAF)
	if optEmptyVMAF.Quality.VMAF.Target != DefaultVMAFTarget {
		t.Errorf("expected target normalized to %v, got %v", DefaultVMAFTarget, optEmptyVMAF.Quality.VMAF.Target)
	}
	if optEmptyVMAF.Quality.VMAF.Minimum != DefaultVMAFMinimum {
		t.Errorf("expected minimum normalized to %v, got %v", DefaultVMAFMinimum, optEmptyVMAF.Quality.VMAF.Minimum)
	}
	if optEmptyVMAF.Quality.VMAF.MarginalTolerance == nil || *optEmptyVMAF.Quality.VMAF.MarginalTolerance != DefaultVMAFMarginalTolerance {
		t.Errorf("expected marginal tolerance normalized to %v", DefaultVMAFMarginalTolerance)
	}
	if err := ValidateOptimizationPolicy("test", optEmptyVMAF); err != nil {
		t.Fatalf("expected valid normalized policy, got: %v", err)
	}

	// 2. Partial vmaf block: vmaf: {target: 97}
	optPartialVMAF := &OptimizationPolicy{
		Enabled: true,
		Quality: &QualityPolicy{
			PreferredMetric: "vmaf",
			VMAF:            &MetricTarget{Target: 97.0},
		},
	}
	NormalizeOptimizationPolicy(optPartialVMAF)
	if optPartialVMAF.Quality.VMAF.Target != 97.0 {
		t.Errorf("expected target=97.0, got %v", optPartialVMAF.Quality.VMAF.Target)
	}
	if optPartialVMAF.Quality.VMAF.Minimum != DefaultVMAFMinimum {
		t.Errorf("expected minimum normalized to default %v, got %v", DefaultVMAFMinimum, optPartialVMAF.Quality.VMAF.Minimum)
	}

	// 3. Partial ssim block: ssim: {minimum: 0.985}
	optPartialSSIM := &OptimizationPolicy{
		Enabled: true,
		Quality: &QualityPolicy{
			PreferredMetric: "ssim",
			SSIM:            &MetricTarget{Minimum: 0.985},
		},
	}
	NormalizeOptimizationPolicy(optPartialSSIM)
	if optPartialSSIM.Quality.SSIM.Target != DefaultSSIMTarget {
		t.Errorf("expected target normalized to default %v, got %v", DefaultSSIMTarget, optPartialSSIM.Quality.SSIM.Target)
	}
	if optPartialSSIM.Quality.SSIM.Minimum != 0.985 {
		t.Errorf("expected minimum=0.985, got %v", optPartialSSIM.Quality.SSIM.Minimum)
	}
	if optPartialSSIM.Quality.SSIM.MarginalTolerance == nil || *optPartialSSIM.Quality.SSIM.MarginalTolerance != DefaultSSIMMarginalTolerance {
		t.Errorf("expected ssim marginal tolerance normalized to %v", DefaultSSIMMarginalTolerance)
	}

	// 4. Target zero without normalization must fail validation (cannot silently accept every candidate)
	optRawZero := &OptimizationPolicy{
		Enabled: true,
		Sampling: &SamplingPolicy{
			Strategy:      "distributed",
			SampleCount:   1,
			SampleSeconds: 20.0,
			Positions:     []float64{0.5},
		},
		Quality: &QualityPolicy{
			PreferredMetric: "vmaf",
			VMAF:            &MetricTarget{Target: 0.0, Minimum: 0.0},
		},
		Search: &SearchPolicy{
			MaxCandidates: 1,
			QualityValues: []int{65},
		},
	}
	if err := ValidateOptimizationPolicy("test", optRawZero); err == nil || !strings.Contains(err.Error(), "target 0") {
		t.Errorf("expected validation failure for unnormalized zero target, got: %v", err)
	}

	// 5. Negative and non-finite targets/minimums are rejected
	for _, tc := range []struct {
		name    string
		vmaf    *MetricTarget
		wantErr string
	}{
		{"negative target", &MetricTarget{Target: -10, Minimum: 90}, "target -10 out of range"},
		{"negative minimum", &MetricTarget{Target: 95, Minimum: -5}, "minimum -5 out of range"},
		{"NaN target", &MetricTarget{Target: math.NaN(), Minimum: 90}, "target NaN out of range"},
		{"Inf minimum", &MetricTarget{Target: 95, Minimum: math.Inf(1)}, "minimum +Inf out of range"},
	} {
		optBad := &OptimizationPolicy{
			Enabled: true,
			Sampling: &SamplingPolicy{
				Strategy:      "distributed",
				SampleCount:   1,
				SampleSeconds: 20.0,
				Positions:     []float64{0.5},
			},
			Quality: &QualityPolicy{
				PreferredMetric: "vmaf",
				VMAF:            tc.vmaf,
			},
			Search: &SearchPolicy{
				MaxCandidates: 1,
				QualityValues: []int{65},
			},
		}
		NormalizeOptimizationPolicy(optBad)
		if err := ValidateOptimizationPolicy("test", optBad); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("[%s] expected error containing %q, got: %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestRecipeOptimizationPolicy_BitrateUpperBounds(t *testing.T) {
	cases := []struct {
		name        string
		size        *SizePolicy
		errContains string
	}{
		{
			name: "preferred max exceeds MaxBitrateKbps",
			size: &SizePolicy{
				PreferredTotalBitrateKbps: &BitrateRange{Min: 5000, Max: 2_000_000},
			},
			errContains: "upper limit",
		},
		{
			name: "preferred min exceeds MaxBitrateKbps",
			size: &SizePolicy{
				PreferredTotalBitrateKbps: &BitrateRange{Min: 2_000_000, Max: 3_000_000},
			},
			errContains: "upper limit",
		},
		{
			name: "preferred math.MaxInt",
			size: &SizePolicy{
				PreferredTotalBitrateKbps: &BitrateRange{Min: 5000, Max: math.MaxInt},
			},
			errContains: "upper limit",
		},
		{
			name: "preferred negative min",
			size: &SizePolicy{
				PreferredTotalBitrateKbps: &BitrateRange{Min: -10, Max: 5000},
			},
			errContains: "must be positive",
		},
		{
			name: "softmax exceeds MaxBitrateKbps",
			size: &SizePolicy{
				SoftMaxTotalBitrateKbps: 2_000_000,
			},
			errContains: "upper limit",
		},
		{
			name: "softmax math.MaxInt",
			size: &SizePolicy{
				SoftMaxTotalBitrateKbps: math.MaxInt,
			},
			errContains: "upper limit",
		},
		{
			name: "softmax negative",
			size: &SizePolicy{
				SoftMaxTotalBitrateKbps: -50,
			},
			errContains: "must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := &OptimizationPolicy{
				Enabled: true,
				Sampling: &SamplingPolicy{
					Strategy:      "distributed",
					SampleCount:   1,
					SampleSeconds: 20.0,
					Positions:     []float64{0.5},
				},
				Quality: &QualityPolicy{
					PreferredMetric: "vmaf",
					VMAF:            &MetricTarget{Target: 96.0, Minimum: 95.0},
				},
				Search: &SearchPolicy{
					MaxCandidates: 1,
					QualityValues: []int{65},
				},
				Size: tc.size,
			}
			err := ValidateOptimizationPolicy("test", opt)
			if err == nil || !strings.Contains(err.Error(), tc.errContains) {
				t.Fatalf("expected error containing %q, got: %v", tc.errContains, err)
			}
		})
	}
}
