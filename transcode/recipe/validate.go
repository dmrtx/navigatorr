package recipe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"gopkg.in/yaml.v3"
)

var safeToken = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

var allowedCopySubtitleCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true, "hdmv_pgs_subtitle": true, "pgs": true,
	"dvd_subtitle": true, "vobsub": true, "dvb_subtitle": true, "dvb_teletext": true, "webvtt": true, "text": true,
}

var allowedFailureClasses = map[string]bool{
	"worker_busy": true, "ssh_transient": true, "worker_unreachable": true, "encoder_temporarily_unavailable": true,
	"container_subtitle_incompatible": true, "container_audio_incompatible": true, "container_attachment_incompatible": true,
	"ffmpeg_input_corrupt": true, "ffmpeg_unknown": true, "validation_duration_mismatch": true,
	"validation_stream_loss": true, "validation_codec_mismatch": true, "source_changed": true,
}

var allowedFallbackActions = map[string]bool{"retry": true, "apply_container_conversion": true}

func Parse(data []byte) (*Snapshot, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("parsing recipe bundle: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("recipe bundle must contain exactly one YAML/JSON document")
	}
	for _, p := range b.Profiles {
		if p.Optimization != nil {
			NormalizeOptimizationPolicy(p.Optimization)
		}
	}
	if err := Validate(&b); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &Snapshot{Bundle: b, Identity: Identity{Version: b.BundleVersion, Digest: "sha256:" + hex.EncodeToString(sum[:])}, Raw: append([]byte(nil), data...)}, nil
}

func Validate(b *Bundle) error {
	if b == nil {
		return fmt.Errorf("recipe bundle is nil")
	}
	if b.SchemaVersion < MinSchemaVersion || b.SchemaVersion > LatestSchemaVersion {
		return fmt.Errorf("unsupported recipe schema_version %d (supported: %d-%d)", b.SchemaVersion, MinSchemaVersion, LatestSchemaVersion)
	}
	if strings.TrimSpace(b.BundleVersion) == "" || !safeToken.MatchString(b.BundleVersion) {
		return fmt.Errorf("invalid bundle_version %q", b.BundleVersion)
	}
	if len(b.Containers) == 0 {
		return fmt.Errorf("recipe bundle has no containers")
	}
	if len(b.Profiles) == 0 {
		return fmt.Errorf("recipe bundle has no profiles")
	}

	for rawName, c := range b.Containers {
		name := normalizeContainer(rawName)
		if name != "mkv" {
			return fmt.Errorf("unsupported recipe container %q", rawName)
		}
		seen := map[string]bool{}
		for _, rawCodec := range c.SubtitleCopy {
			codec := normalizeCodec(rawCodec)
			if !safeToken.MatchString(codec) || !allowedCopySubtitleCodecs[codec] {
				return fmt.Errorf("container %s: unsafe or unsupported subtitle copy codec %q", rawName, rawCodec)
			}
			if seen[codec] {
				return fmt.Errorf("container %s: duplicate subtitle copy codec %q", rawName, rawCodec)
			}
			seen[codec] = true
		}
		for rawFrom, conv := range c.SubtitleConversions {
			from := normalizeCodec(rawFrom)
			target := normalizeCodec(conv.TargetCodec)
			if !safeToken.MatchString(from) {
				return fmt.Errorf("container %s: invalid source subtitle codec %q", rawName, rawFrom)
			}
			if target != "subrip" {
				return fmt.Errorf("container %s: unsupported subtitle conversion target %q", rawName, conv.TargetCodec)
			}
			if !safeToken.MatchString(conv.Reason) {
				return fmt.Errorf("container %s: invalid conversion reason %q", rawName, conv.Reason)
			}
			if seen[from] {
				return fmt.Errorf("container %s: codec %q is both copy-compatible and configured for conversion", rawName, rawFrom)
			}
		}
	}

	for name, p := range b.Profiles {
		if !safeToken.MatchString(name) {
			return fmt.Errorf("invalid profile name %q", name)
		}
		if p.Optimization != nil && b.SchemaVersion < SupportedSchemaVersionV2 {
			return fmt.Errorf("profile %q: optimization policy requires schema_version %d", name, SupportedSchemaVersionV2)
		}
		if err := ValidateProfile(name, p); err != nil {
			return err
		}
		if _, ok := b.Containers[normalizeContainer(p.Container)]; !ok {
			// permit matroska alias to resolve the canonical mkv rule
			if normalizeContainer(p.Container) != "mkv" {
				return fmt.Errorf("profile %q references undefined container %q", name, p.Container)
			}
			if _, ok = b.Containers["mkv"]; !ok {
				return fmt.Errorf("profile %q references undefined container %q", name, p.Container)
			}
		}
	}
	return nil
}

func ValidateProfile(name string, p Profile) error {
	if normalizeContainer(p.Container) != "mkv" {
		return fmt.Errorf("profile %q: unsupported container %q", name, p.Container)
	}
	if normalizeCodec(p.Video.Codec) != "hevc_videotoolbox" {
		return fmt.Errorf("profile %q: unsupported video codec %q", name, p.Video.Codec)
	}
	if p.Video.Quality < 1 || p.Video.Quality > 100 {
		return fmt.Errorf("profile %q: video quality %d out of range 1-100", name, p.Video.Quality)
	}
	videoProfile := normalizeCodec(p.Video.Profile)
	pixelFormat := normalizeCodec(p.Video.PixelFormat)
	if videoProfile != "" && videoProfile != "main" && videoProfile != "main10" {
		return fmt.Errorf("profile %q: unsupported HEVC profile %q (allowed: main, main10)", name, p.Video.Profile)
	}
	if pixelFormat != "" && pixelFormat != "yuv420p" && pixelFormat != "p010le" {
		return fmt.Errorf("profile %q: unsupported pixel_format %q (allowed: yuv420p, p010le)", name, p.Video.PixelFormat)
	}
	if videoProfile == "main10" && pixelFormat != "p010le" {
		return fmt.Errorf("profile %q: HEVC main10 requires pixel_format p010le to guarantee 10-bit output", name)
	}
	if pixelFormat == "p010le" && videoProfile != "main10" {
		return fmt.Errorf("profile %q: pixel_format p010le requires HEVC profile main10", name)
	}
	if videoProfile == "main" && pixelFormat == "p010le" {
		return fmt.Errorf("profile %q: HEVC main is incompatible with pixel_format p010le", name)
	}
	if strings.ToLower(strings.TrimSpace(p.Audio.Mode)) != "copy" {
		return fmt.Errorf("profile %q: unsupported audio mode %q", name, p.Audio.Mode)
	}
	if strings.ToLower(strings.TrimSpace(p.Subtitles.Mode)) != "preserve" {
		return fmt.Errorf("profile %q: unsupported subtitles mode %q", name, p.Subtitles.Mode)
	}
	if !p.Preserve.Metadata || !p.Preserve.Chapters || !p.Preserve.Attachments {
		return fmt.Errorf("profile %q: metadata, chapters, and attachments must all be preserved by the current engine capability boundary", name)
	}
	r := p.Resilience
	if r.MaxAttempts <= 0 || r.MaxAttempts > 10 {
		return fmt.Errorf("profile %q: max_attempts must be 1-10", name)
	}
	if r.TransientRetries < 0 || r.TransientRetries >= r.MaxAttempts {
		return fmt.Errorf("profile %q: transient_retries must be >=0 and < max_attempts", name)
	}
	if len(r.RetryBackoffSeconds) < r.TransientRetries {
		return fmt.Errorf("profile %q: retry_backoff_seconds must cover transient_retries", name)
	}
	for _, s := range r.RetryBackoffSeconds {
		if s < 0 || s > 3600 {
			return fmt.Errorf("profile %q: retry backoff %d out of range", name, s)
		}
	}
	if r.MaxFallbacks < 0 || r.MaxFallbacks > 4 {
		return fmt.Errorf("profile %q: max_fallbacks out of range 0-4", name)
	}
	seen := map[string]bool{}
	for _, f := range r.Fallbacks {
		if !allowedFailureClasses[f.When] {
			return fmt.Errorf("profile %q: unknown fallback condition %q", name, f.When)
		}
		if !allowedFallbackActions[f.Action] {
			return fmt.Errorf("profile %q: unknown fallback action %q", name, f.Action)
		}
		key := f.When + ":" + f.Action
		if seen[key] {
			return fmt.Errorf("profile %q: duplicate fallback %q", name, key)
		}
		seen[key] = true
		if f.Action == "apply_container_conversion" && f.When != "container_subtitle_incompatible" {
			return fmt.Errorf("profile %q: container conversion fallback only valid for container_subtitle_incompatible", name)
		}
	}
	if p.Optimization != nil {
		if err := ValidateOptimizationPolicy(name, p.Optimization); err != nil {
			return err
		}
	}
	return nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// NormalizeOptimizationPolicy populates documented defaults for enabled optimization policies
// where fields were omitted, replacing truly omitted/zero values with concrete defaults without
// overwriting invalid negative numbers.
func NormalizeOptimizationPolicy(opt *OptimizationPolicy) {
	if opt == nil || !opt.Enabled {
		return
	}

	// 1. Sampling normalization
	if opt.Sampling == nil {
		opt.Sampling = &SamplingPolicy{
			Strategy:      DefaultSamplingStrategy,
			SampleCount:   DefaultSampleCount,
			SampleSeconds: DefaultSampleSeconds,
			Positions:     append([]float64(nil), DefaultSamplingPositions...),
		}
	} else {
		opt.Sampling.Strategy = strings.ToLower(strings.TrimSpace(opt.Sampling.Strategy))
		if opt.Sampling.Strategy == "" {
			opt.Sampling.Strategy = DefaultSamplingStrategy
		}
		// Only default if EXACTLY zero. Do not overwrite negative numbers!
		if opt.Sampling.SampleSeconds == 0 {
			opt.Sampling.SampleSeconds = DefaultSampleSeconds
		}
		if opt.Sampling.SampleCount == 0 {
			if len(opt.Sampling.Positions) > 0 {
				opt.Sampling.SampleCount = len(opt.Sampling.Positions)
			} else {
				opt.Sampling.SampleCount = DefaultSampleCount
			}
		}
		if len(opt.Sampling.Positions) == 0 && opt.Sampling.SampleCount > 0 {
			if opt.Sampling.SampleCount == 3 {
				opt.Sampling.Positions = append([]float64(nil), DefaultSamplingPositions...)
			} else {
				opt.Sampling.Positions = make([]float64, opt.Sampling.SampleCount)
				for i := 0; i < opt.Sampling.SampleCount; i++ {
					opt.Sampling.Positions[i] = float64(i+1) / float64(opt.Sampling.SampleCount+1)
				}
			}
		}
	}

	// 2. Quality normalization
	defaultVMAFTol := DefaultVMAFMarginalTolerance
	defaultSSIMTol := DefaultSSIMMarginalTolerance
	if opt.Quality == nil {
		opt.Quality = &QualityPolicy{
			PreferredMetric: DefaultPreferredMetric,
			VMAF:            &MetricTarget{Target: DefaultVMAFTarget, Minimum: DefaultVMAFMinimum, MarginalTolerance: &defaultVMAFTol},
			SSIM:            &MetricTarget{Target: DefaultSSIMTarget, Minimum: DefaultSSIMMinimum, MarginalTolerance: &defaultSSIMTol},
		}
	} else {
		opt.Quality.PreferredMetric = strings.ToLower(strings.TrimSpace(opt.Quality.PreferredMetric))
		if opt.Quality.PreferredMetric == "" {
			opt.Quality.PreferredMetric = DefaultPreferredMetric
		}
		if opt.Quality.VMAF != nil {
			if opt.Quality.VMAF.Target == 0 {
				opt.Quality.VMAF.Target = DefaultVMAFTarget
			}
			if opt.Quality.VMAF.Minimum == 0 {
				opt.Quality.VMAF.Minimum = DefaultVMAFMinimum
			}
			if opt.Quality.VMAF.MarginalTolerance == nil {
				v := DefaultVMAFMarginalTolerance
				opt.Quality.VMAF.MarginalTolerance = &v
			}
		} else if opt.Quality.PreferredMetric == "vmaf" {
			v := DefaultVMAFMarginalTolerance
			opt.Quality.VMAF = &MetricTarget{Target: DefaultVMAFTarget, Minimum: DefaultVMAFMinimum, MarginalTolerance: &v}
		}

		if opt.Quality.SSIM != nil {
			if opt.Quality.SSIM.Target == 0 {
				opt.Quality.SSIM.Target = DefaultSSIMTarget
			}
			if opt.Quality.SSIM.Minimum == 0 {
				opt.Quality.SSIM.Minimum = DefaultSSIMMinimum
			}
			if opt.Quality.SSIM.MarginalTolerance == nil {
				v := DefaultSSIMMarginalTolerance
				opt.Quality.SSIM.MarginalTolerance = &v
			}
		} else if opt.Quality.PreferredMetric == "ssim" {
			v := DefaultSSIMMarginalTolerance
			opt.Quality.SSIM = &MetricTarget{Target: DefaultSSIMTarget, Minimum: DefaultSSIMMinimum, MarginalTolerance: &v}
		}
	}

	// 3. Search normalization
	if opt.Search == nil {
		opt.Search = &SearchPolicy{
			MaxCandidates: DefaultMaxCandidates,
			QualityValues: append([]int(nil), DefaultQualityValues...),
		}
	} else {
		// Only default if EXACTLY zero. Do not overwrite negative numbers!
		if opt.Search.MaxCandidates == 0 {
			if len(opt.Search.QualityValues) > 0 {
				opt.Search.MaxCandidates = len(opt.Search.QualityValues)
			} else {
				opt.Search.MaxCandidates = DefaultMaxCandidates
			}
		}
		if len(opt.Search.QualityValues) == 0 {
			opt.Search.QualityValues = append([]int(nil), DefaultQualityValues...)
		}
		// Adaptive defaults: empty mode stays empty (treated as exhaustive downstream).
		// Normalize explicit mode spelling; leave AdaptiveInitialQuality 0 as default 65.
		if opt.Search.AdaptiveMode != "" {
			opt.Search.AdaptiveMode = strings.ToLower(strings.TrimSpace(opt.Search.AdaptiveMode))
		}
	}
}

// ValidateOptimizationPolicy verifies that typed optimization models contain safe, bounded values.
func ValidateOptimizationPolicy(name string, opt *OptimizationPolicy) error {
	if opt == nil || !opt.Enabled {
		return nil
	}

	// 1. Sampling validation
	if opt.Sampling == nil {
		return fmt.Errorf("profile %q: sampling policy is required when optimization is enabled", name)
	}
	s := opt.Sampling
	strat := strings.ToLower(strings.TrimSpace(s.Strategy))
	if strat != "distributed" && strat != "uniform" && strat != "relative_positions" {
		return fmt.Errorf("profile %q: unsupported sampling strategy %q (allowed: distributed, uniform, relative_positions)", name, s.Strategy)
	}
	if s.SampleCount < 1 || s.SampleCount > 20 {
		return fmt.Errorf("profile %q: sample_count %d out of range 1-20", name, s.SampleCount)
	}
	if !isFinite(s.SampleSeconds) || s.SampleSeconds <= 0 || s.SampleSeconds > 120.0 {
		return fmt.Errorf("profile %q: sample_seconds %v out of range (0.0, 120.0]", name, s.SampleSeconds)
	}
	if len(s.Positions) != s.SampleCount {
		return fmt.Errorf("profile %q: sampling positions length (%d) must match sample_count (%d)", name, len(s.Positions), s.SampleCount)
	}
	for i, pos := range s.Positions {
		if !isFinite(pos) || pos <= 0.0 || pos >= 1.0 {
			return fmt.Errorf("profile %q: sampling position %v at index %d out of range (0.0, 1.0)", name, pos, i)
		}
		if i > 0 && pos <= s.Positions[i-1] {
			return fmt.Errorf("profile %q: sampling positions must be strictly increasing (%v <= %v)", name, pos, s.Positions[i-1])
		}
	}

	// 2. Quality validation
	if opt.Quality == nil {
		return fmt.Errorf("profile %q: quality policy is required when optimization is enabled", name)
	}
	q := opt.Quality
	metric := strings.ToLower(strings.TrimSpace(q.PreferredMetric))
	if metric != "vmaf" && metric != "ssim" {
		return fmt.Errorf("profile %q: unsupported preferred_metric %q (allowed: vmaf, ssim)", name, q.PreferredMetric)
	}
	if metric == "vmaf" && q.VMAF == nil {
		return fmt.Errorf("profile %q: vmaf metric targets are required when preferred_metric is vmaf", name)
	}
	if metric == "ssim" && q.SSIM == nil {
		return fmt.Errorf("profile %q: ssim metric targets are required when preferred_metric is ssim", name)
	}

	validateMetricTarget := func(metricName string, m *MetricTarget, maxVal float64) error {
		if m == nil {
			return nil
		}
		if !isFinite(m.Target) || m.Target <= 0 || m.Target > maxVal {
			return fmt.Errorf("profile %q: %s target %v out of range (0.0-%v]", name, metricName, m.Target, maxVal)
		}
		if !isFinite(m.Minimum) || m.Minimum <= 0 || m.Minimum > maxVal {
			return fmt.Errorf("profile %q: %s minimum %v out of range (0.0-%v]", name, metricName, m.Minimum, maxVal)
		}
		if m.Target < m.Minimum {
			return fmt.Errorf("profile %q: %s target (%v) must be >= minimum (%v)", name, metricName, m.Target, m.Minimum)
		}
		if m.MarginalTolerance != nil {
			tol := *m.MarginalTolerance
			if !isFinite(tol) || tol < 0 || tol > maxVal {
				return fmt.Errorf("profile %q: %s marginal_tolerance %v out of range [0.0, %v]", name, metricName, tol, maxVal)
			}
		}
		return nil
	}

	if err := validateMetricTarget("vmaf", q.VMAF, 100.0); err != nil {
		return err
	}
	if err := validateMetricTarget("ssim", q.SSIM, 1.0); err != nil {
		return err
	}

	// 3. Search validation
	if opt.Search == nil {
		return fmt.Errorf("profile %q: search policy is required when optimization is enabled", name)
	}
	srch := opt.Search
	if srch.MaxCandidates < 1 || srch.MaxCandidates > 20 {
		return fmt.Errorf("profile %q: max_candidates %d out of range 1-20", name, srch.MaxCandidates)
	}
	if len(srch.QualityValues) == 0 {
		return fmt.Errorf("profile %q: search quality_values cannot be empty", name)
	}
	if len(srch.QualityValues) > srch.MaxCandidates {
		return fmt.Errorf("profile %q: number of quality_values (%d) exceeds max_candidates (%d)", name, len(srch.QualityValues), srch.MaxCandidates)
	}
	for i, val := range srch.QualityValues {
		if val < 1 || val > 100 {
			return fmt.Errorf("profile %q: quality value %d out of range 1-100", name, val)
		}
		if i > 0 && val <= srch.QualityValues[i-1] {
			return fmt.Errorf("profile %q: quality_values must be strictly ordered without duplicates (found %d after %d)", name, val, srch.QualityValues[i-1])
		}
	}
	if srch.AdaptiveMode != "" {
		mode := strings.ToLower(strings.TrimSpace(srch.AdaptiveMode))
		if mode != "exhaustive" && mode != "adaptive" {
			return fmt.Errorf("profile %q: adaptive_mode %q must be 'exhaustive' or 'adaptive'", name, srch.AdaptiveMode)
		}
	}
	if srch.AdaptiveInitialQuality != 0 && (srch.AdaptiveInitialQuality < 1 || srch.AdaptiveInitialQuality > 100) {
		return fmt.Errorf("profile %q: adaptive_initial_quality %d out of range 1-100", name, srch.AdaptiveInitialQuality)
	}
	if srch.EncodeConcurrency < 0 || srch.EncodeConcurrency > transcode.MaxBenchmarkConcurrency {
		return fmt.Errorf("profile %q: encode_concurrency %d out of range 0-%d (0 selects default)", name, srch.EncodeConcurrency, transcode.MaxBenchmarkConcurrency)
	}
	if srch.MetricConcurrency < 0 || srch.MetricConcurrency > transcode.MaxBenchmarkConcurrency {
		return fmt.Errorf("profile %q: metric_concurrency %d out of range 0-%d (0 selects default)", name, srch.MetricConcurrency, transcode.MaxBenchmarkConcurrency)
	}

	// 4. Size validation
	if opt.Size != nil {
		sz := opt.Size
		if sz.PreferredTotalBitrateKbps != nil {
			pb := sz.PreferredTotalBitrateKbps
			if pb.Min <= 0 || pb.Max <= 0 {
				return fmt.Errorf("profile %q: preferred_total_bitrate_kbps values must be positive", name)
			}
			if pb.Min > MaxBitrateKbps || pb.Max > MaxBitrateKbps {
				return fmt.Errorf("profile %q: preferred_total_bitrate_kbps values exceed upper limit (%d kbps)", name, MaxBitrateKbps)
			}
			if pb.Max < pb.Min {
				return fmt.Errorf("profile %q: preferred_total_bitrate_kbps max (%d) must be >= min (%d)", name, pb.Max, pb.Min)
			}
		}
		if sz.SoftMaxTotalBitrateKbps < 0 {
			return fmt.Errorf("profile %q: soft_max_total_bitrate_kbps must be >= 0", name)
		}
		if sz.SoftMaxTotalBitrateKbps > MaxBitrateKbps {
			return fmt.Errorf("profile %q: soft_max_total_bitrate_kbps (%d) exceeds upper limit (%d kbps)", name, sz.SoftMaxTotalBitrateKbps, MaxBitrateKbps)
		}
		if sz.SoftMaxTotalBitrateKbps > 0 && sz.PreferredTotalBitrateKbps != nil {
			if sz.SoftMaxTotalBitrateKbps < sz.PreferredTotalBitrateKbps.Max {
				return fmt.Errorf("profile %q: soft_max_total_bitrate_kbps (%d) must be >= preferred_total_bitrate_kbps max (%d)", name, sz.SoftMaxTotalBitrateKbps, sz.PreferredTotalBitrateKbps.Max)
			}
		}
	}

	return nil
}

func normalizeCodec(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func normalizeContainer(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "matroska" {
		return "mkv"
	}
	return s
}
