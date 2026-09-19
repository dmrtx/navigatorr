package recipe

import "time"

const (
	MinSchemaVersion         = 1
	LatestSchemaVersion      = 2
	SupportedSchemaVersionV1 = 1
	SupportedSchemaVersionV2 = 2
	// Deprecated: use MinSchemaVersion or LatestSchemaVersion instead.
	SupportedSchemaVersion = LatestSchemaVersion
)

const (
	DefaultSamplingStrategy      = "distributed"
	DefaultSampleCount           = 3
	DefaultSampleSeconds         = 20.0
	DefaultPreferredMetric       = "vmaf"
	DefaultVMAFTarget            = 96.0
	DefaultVMAFMinimum           = 95.0
	DefaultVMAFMarginalTolerance = 0.5
	DefaultSSIMTarget            = 0.99
	DefaultSSIMMinimum           = 0.98
	DefaultSSIMMarginalTolerance = 0.005
	DefaultMaxCandidates         = 5
	// MaxBitrateKbps is the conservative upper limit (1,000,000 kbps = 1 Gbps) for recipe bitrate guidance
	// to prevent overflow and absurd values during future arithmetic and optimization.
	MaxBitrateKbps = 1_000_000
)

var (
	DefaultSamplingPositions = []float64{0.2, 0.5, 0.8}
	DefaultQualityValues     = []int{55, 60, 65, 70, 75}
)

type Bundle struct {
	SchemaVersion int                      `json:"schema_version" yaml:"schema_version"`
	BundleVersion string                   `json:"bundle_version" yaml:"bundle_version"`
	Containers    map[string]ContainerRule `json:"containers" yaml:"containers"`
	Profiles      map[string]Profile       `json:"profiles" yaml:"profiles"`
}

type ContainerRule struct {
	SubtitleCopy        []string                  `json:"subtitle_copy" yaml:"subtitle_copy"`
	SubtitleConversions map[string]ConversionRule `json:"subtitle_conversions" yaml:"subtitle_conversions"`
}

type ConversionRule struct {
	TargetCodec string `json:"target_codec" yaml:"target_codec"`
	Reason      string `json:"reason" yaml:"reason"`
}

// OptimizationPolicy defines the tuning policy for automated quality and bitrate optimization.
type OptimizationPolicy struct {
	Enabled  bool            `json:"enabled" yaml:"enabled"`
	Sampling *SamplingPolicy `json:"sampling,omitempty" yaml:"sampling,omitempty"`
	Quality  *QualityPolicy  `json:"quality,omitempty" yaml:"quality,omitempty"`
	Search   *SearchPolicy   `json:"search,omitempty" yaml:"search,omitempty"`
	Size     *SizePolicy     `json:"size,omitempty" yaml:"size,omitempty"`
}

// Clone creates a deep copy of OptimizationPolicy without aliasing pointers or slices.
func (opt *OptimizationPolicy) Clone() *OptimizationPolicy {
	if opt == nil {
		return nil
	}
	return &OptimizationPolicy{
		Enabled:  opt.Enabled,
		Sampling: opt.Sampling.Clone(),
		Quality:  opt.Quality.Clone(),
		Search:   opt.Search.Clone(),
		Size:     opt.Size.Clone(),
	}
}

// SamplingPolicy controls where and how probe samples are extracted from the source video.
type SamplingPolicy struct {
	Strategy      string    `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	SampleCount   int       `json:"sample_count,omitempty" yaml:"sample_count,omitempty"`
	SampleSeconds float64   `json:"sample_seconds,omitempty" yaml:"sample_seconds,omitempty"`
	Positions     []float64 `json:"positions,omitempty" yaml:"positions,omitempty"`
}

// Clone creates a deep copy of SamplingPolicy.
func (s *SamplingPolicy) Clone() *SamplingPolicy {
	if s == nil {
		return nil
	}
	var pos []float64
	if s.Positions != nil {
		pos = append([]float64(nil), s.Positions...)
	}
	return &SamplingPolicy{
		Strategy:      s.Strategy,
		SampleCount:   s.SampleCount,
		SampleSeconds: s.SampleSeconds,
		Positions:     pos,
	}
}

// QualityPolicy configures target objective quality metrics and acceptability thresholds.
type QualityPolicy struct {
	PreferredMetric string        `json:"preferred_metric,omitempty" yaml:"preferred_metric,omitempty"`
	VMAF            *MetricTarget `json:"vmaf,omitempty" yaml:"vmaf,omitempty"`
	SSIM            *MetricTarget `json:"ssim,omitempty" yaml:"ssim,omitempty"`
}

// Clone creates a deep copy of QualityPolicy.
func (q *QualityPolicy) Clone() *QualityPolicy {
	if q == nil {
		return nil
	}
	return &QualityPolicy{
		PreferredMetric: q.PreferredMetric,
		VMAF:            q.VMAF.Clone(),
		SSIM:            q.SSIM.Clone(),
	}
}

// MetricTarget specifies target, minimum acceptable scores, and metric-specific marginal tolerance.
type MetricTarget struct {
	Target            float64  `json:"target" yaml:"target"`
	Minimum           float64  `json:"minimum" yaml:"minimum"`
	MarginalTolerance *float64 `json:"marginal_tolerance,omitempty" yaml:"marginal_tolerance,omitempty"`
}

// Clone creates a deep copy of MetricTarget.
func (m *MetricTarget) Clone() *MetricTarget {
	if m == nil {
		return nil
	}
	var tol *float64
	if m.MarginalTolerance != nil {
		v := *m.MarginalTolerance
		tol = &v
	}
	return &MetricTarget{
		Target:            m.Target,
		Minimum:           m.Minimum,
		MarginalTolerance: tol,
	}
}

// SearchPolicy defines parameter space and candidate selection bounds.
type SearchPolicy struct {
	MaxCandidates int   `json:"max_candidates,omitempty" yaml:"max_candidates,omitempty"`
	QualityValues []int `json:"quality_values,omitempty" yaml:"quality_values,omitempty"`
	// AdaptiveMode selects exhaustive (default) or adaptive candidate evaluation.
	AdaptiveMode string `json:"adaptive_mode,omitempty" yaml:"adaptive_mode,omitempty"`
	// AdaptiveInitialQuality overrides the adaptive starting quality (default 65).
	AdaptiveInitialQuality int `json:"adaptive_initial_quality,omitempty" yaml:"adaptive_initial_quality,omitempty"`
	// EncodeConcurrency bounds simultaneous candidate sample encodes (0 selects default 2).
	EncodeConcurrency int `json:"encode_concurrency,omitempty" yaml:"encode_concurrency,omitempty"`
	// MetricConcurrency bounds simultaneous metric evaluations (0 selects default 2).
	MetricConcurrency int `json:"metric_concurrency,omitempty" yaml:"metric_concurrency,omitempty"`
}

// Clone creates a deep copy of SearchPolicy.
func (srch *SearchPolicy) Clone() *SearchPolicy {
	if srch == nil {
		return nil
	}
	var qv []int
	if srch.QualityValues != nil {
		qv = append([]int(nil), srch.QualityValues...)
	}
	return &SearchPolicy{
		MaxCandidates:          srch.MaxCandidates,
		QualityValues:          qv,
		AdaptiveMode:           srch.AdaptiveMode,
		AdaptiveInitialQuality: srch.AdaptiveInitialQuality,
		EncodeConcurrency:      srch.EncodeConcurrency,
		MetricConcurrency:      srch.MetricConcurrency,
	}
}

// SizePolicy specifies bitrate ranges and constraints in kilobits per second.
type SizePolicy struct {
	PreferredTotalBitrateKbps *BitrateRange `json:"preferred_total_bitrate_kbps,omitempty" yaml:"preferred_total_bitrate_kbps,omitempty"`
	SoftMaxTotalBitrateKbps   int           `json:"soft_max_total_bitrate_kbps,omitempty" yaml:"soft_max_total_bitrate_kbps,omitempty"`
}

// Clone creates a deep copy of SizePolicy.
func (sz *SizePolicy) Clone() *SizePolicy {
	if sz == nil {
		return nil
	}
	return &SizePolicy{
		PreferredTotalBitrateKbps: sz.PreferredTotalBitrateKbps.Clone(),
		SoftMaxTotalBitrateKbps:   sz.SoftMaxTotalBitrateKbps,
	}
}

// BitrateRange defines minimum and maximum acceptable total bitrate in kbps.
type BitrateRange struct {
	Min int `json:"min" yaml:"min"`
	Max int `json:"max" yaml:"max"`
}

// Clone creates a deep copy of BitrateRange.
func (b *BitrateRange) Clone() *BitrateRange {
	if b == nil {
		return nil
	}
	return &BitrateRange{
		Min: b.Min,
		Max: b.Max,
	}
}

type Profile struct {
	Container    string              `json:"container" yaml:"container"`
	Video        VideoProfile        `json:"video" yaml:"video"`
	Audio        AudioProfile        `json:"audio" yaml:"audio"`
	Subtitles    SubtitleProfile     `json:"subtitles" yaml:"subtitles"`
	Preserve     PreserveProfile     `json:"preserve" yaml:"preserve"`
	Resilience   ResilienceProfile   `json:"resilience" yaml:"resilience"`
	Optimization *OptimizationPolicy `json:"optimization,omitempty" yaml:"optimization,omitempty"`
}

type VideoProfile struct {
	Codec string `json:"codec" yaml:"codec"`
	// Quality is -q:v for hevc_videotoolbox (higher = higher quality) and CRF
	// for libx265 (LOWER = higher quality, 1..51).
	Quality int `json:"quality" yaml:"quality"`
	// Preset is the libx265 preset (empty for hevc_videotoolbox).
	Preset          string `json:"preset,omitempty" yaml:"preset,omitempty"`
	Profile         string `json:"profile,omitempty" yaml:"profile,omitempty"`
	PixelFormat     string `json:"pixel_format,omitempty" yaml:"pixel_format,omitempty"`
	PrioritizeSpeed *bool  `json:"prioritize_speed,omitempty" yaml:"prioritize_speed,omitempty"`
	SpatialAQ       *bool  `json:"spatial_aq,omitempty" yaml:"spatial_aq,omitempty"`
	Realtime        *bool  `json:"realtime,omitempty" yaml:"realtime,omitempty"`
}

type AudioProfile struct {
	Mode string `json:"mode" yaml:"mode"`
}
type SubtitleProfile struct {
	Mode                string `json:"mode" yaml:"mode"`
	ConvertIncompatible bool   `json:"convert_incompatible" yaml:"convert_incompatible"`
}
type PreserveProfile struct {
	Metadata    bool `json:"metadata" yaml:"metadata"`
	Chapters    bool `json:"chapters" yaml:"chapters"`
	Attachments bool `json:"attachments" yaml:"attachments"`
}

type ResilienceProfile struct {
	MaxAttempts         int            `json:"max_attempts" yaml:"max_attempts"`
	TransientRetries    int            `json:"transient_retries" yaml:"transient_retries"`
	RetryBackoffSeconds []int          `json:"retry_backoff_seconds" yaml:"retry_backoff_seconds"`
	MaxFallbacks        int            `json:"max_fallbacks" yaml:"max_fallbacks"`
	Fallbacks           []FallbackRule `json:"fallbacks" yaml:"fallbacks"`
}

type FallbackRule struct {
	When   string `json:"when" yaml:"when"`
	Action string `json:"action" yaml:"action"`
}

type SourceSubtitle struct {
	SourceStreamIndex int
	TypeIndex         int
	Codec             string
}

type Identity struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type Snapshot struct {
	Bundle   Bundle   `json:"-"`
	Identity Identity `json:"identity"`
	Raw      []byte   `json:"-"`
}

type Status struct {
	Source          string    `json:"source"`
	Channel         string    `json:"channel,omitempty"`
	Revision        string    `json:"revision,omitempty"`
	ActiveVersion   string    `json:"active_version"`
	ActiveDigest    string    `json:"active_digest"`
	LastKnownGood   string    `json:"last_known_good"`
	LastCheckedAt   time.Time `json:"last_checked_at,omitempty"`
	LastUpdateError string    `json:"last_update_error,omitempty"`
}

type Manifest struct {
	SchemaVersion            int    `json:"schema_version"`
	BundleVersion            string `json:"bundle_version"`
	BundleSHA256             string `json:"bundle_sha256"`
	DownloadURL              string `json:"download_url"`
	MinimumNavigatorrVersion string `json:"minimum_navigatorr_version,omitempty"`
	PublishedAt              string `json:"published_at,omitempty"`
}
