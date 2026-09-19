package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

type SubtitleAction struct {
	SourceStreamIndex int    `json:"source_stream_index" yaml:"source_stream_index"`
	TypeIndex         int    `json:"type_index" yaml:"type_index"`
	SourceCodec       string `json:"source_codec" yaml:"source_codec"`
	Operation         string `json:"operation" yaml:"operation"`
	Codec             string `json:"codec" yaml:"codec"`
	Reason            string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type ResiliencePlan struct {
	MaxAttempts         int      `json:"max_attempts" yaml:"max_attempts"`
	TransientRetries    int      `json:"transient_retries" yaml:"transient_retries"`
	RetryBackoffSeconds []int    `json:"retry_backoff_seconds,omitempty" yaml:"retry_backoff_seconds,omitempty"`
	MaxFallbacks        int      `json:"max_fallbacks" yaml:"max_fallbacks"`
	RetryOn             []string `json:"retry_on,omitempty" yaml:"retry_on,omitempty"`
}

type Plan struct {
	Container  string `json:"container" yaml:"container"`
	VideoCodec string `json:"video_codec" yaml:"video_codec"`
	// Quality is the rate-control knob. For hevc_videotoolbox it maps to -q:v
	// (higher = higher quality). For libx265 it maps to -crf (LOWER = higher
	// quality, valid range 1..51). See VideoProfile.Preset for x265 preset.
	Quality      int    `json:"quality" yaml:"quality"`
	VideoProfile string `json:"video_profile,omitempty" yaml:"video_profile,omitempty"`
	PixelFormat  string `json:"pixel_format,omitempty" yaml:"pixel_format,omitempty"`
	// Preset is the libx265 speed/efficiency preset. It must be empty for
	// hevc_videotoolbox and is validated against a fixed safe enum for libx265.
	Preset          string `json:"preset,omitempty" yaml:"preset,omitempty"`
	PrioritizeSpeed *bool  `json:"prioritize_speed,omitempty" yaml:"prioritize_speed,omitempty"`
	SpatialAQ       *bool  `json:"spatial_aq,omitempty" yaml:"spatial_aq,omitempty"`
	Realtime        *bool  `json:"realtime,omitempty" yaml:"realtime,omitempty"`
	// Bounded typed hevc_videotoolbox rate-control/offline knobs. All must be
	// unset for libx265; quality-vs-bitrate exclusivity is enforced at every
	// validation layer. See VideoProfile for field semantics.
	AverageBitrateKbps           int              `json:"average_bitrate_kbps,omitempty" yaml:"average_bitrate_kbps,omitempty"`
	MaxBitrateKbps               int              `json:"max_bitrate_kbps,omitempty" yaml:"max_bitrate_kbps,omitempty"`
	ConstantBitrate              *bool            `json:"constant_bitrate,omitempty" yaml:"constant_bitrate,omitempty"`
	QMin                         *int             `json:"qmin,omitempty" yaml:"qmin,omitempty"`
	QMax                         *int             `json:"qmax,omitempty" yaml:"qmax,omitempty"`
	GOPSize                      *int             `json:"gop_size,omitempty" yaml:"gop_size,omitempty"`
	BFrames                      *int             `json:"b_frames,omitempty" yaml:"b_frames,omitempty"`
	ClosedGOP                    *bool            `json:"closed_gop,omitempty" yaml:"closed_gop,omitempty"`
	PowerEfficient               *bool            `json:"power_efficient,omitempty" yaml:"power_efficient,omitempty"`
	MaxRefFrames                 *int             `json:"max_ref_frames,omitempty" yaml:"max_ref_frames,omitempty"`
	ExpectedBitDepth             int              `json:"expected_bit_depth,omitempty" yaml:"expected_bit_depth,omitempty"`
	AudioMode                    string           `json:"audio_mode" yaml:"audio_mode"`
	SubtitleMode                 string           `json:"subtitle_mode" yaml:"subtitle_mode"`
	ConvertIncompatibleSubtitles bool             `json:"convert_incompatible_subtitles" yaml:"convert_incompatible_subtitles"`
	PreserveMetadata             bool             `json:"preserve_metadata" yaml:"preserve_metadata"`
	PreserveChapters             bool             `json:"preserve_chapters" yaml:"preserve_chapters"`
	PreserveAttachments          bool             `json:"preserve_attachments" yaml:"preserve_attachments"`
	SubtitleActions              []SubtitleAction `json:"subtitle_actions,omitempty" yaml:"subtitle_actions,omitempty"`
	RecipeVersion                string           `json:"recipe_version,omitempty" yaml:"recipe_version,omitempty"`
	RecipeDigest                 string           `json:"recipe_digest,omitempty" yaml:"recipe_digest,omitempty"`
	PlanDigest                   string           `json:"plan_digest,omitempty" yaml:"plan_digest,omitempty"`
	Resilience                   ResiliencePlan   `json:"resilience,omitempty" yaml:"resilience,omitempty"`
	AppliedFallbacks             []string         `json:"applied_fallbacks,omitempty" yaml:"applied_fallbacks,omitempty"`
}

// Video encoder identifiers accepted by the worker and recipe engine.
const (
	VideoCodecHEVCVideoToolbox = "hevc_videotoolbox"
	VideoCodecLibX265          = "libx265"
)

// libx265 rate-control bounds. For libx265, Quality is interpreted as CRF,
// where a LOWER value yields HIGHER quality (inverse of VideoToolbox -q:v).
const (
	LibX265CRFMin = 1
	LibX265CRFMax = 51
)

// Bounds for typed hevc_videotoolbox rate-control/offline knobs shared by the
// coordinator and worker without a transcode->recipe import cycle.
// MaxVideoBitrateKbps mirrors the recipe MaxBitrateKbps limit (1 Gbps).
const MaxVideoBitrateKbps = 1_000_000

// MaxQPBound bounds explicit qmin/qmax quantizer values (FFmpeg scale;
// nil means "emit nothing", never the FFmpeg "auto" sentinel).
const MaxQPBound = 69

// MaxBenchmarkGOPSize bounds explicit gop_size keyframe intervals.
const MaxBenchmarkGOPSize = 100000

// MaxBenchmarkBFrames bounds explicit b_frames to the only truthful values:
// 0 disables frame reordering/B-frames (-bf 0) and 1 enables it (-bf 1).
// Upstream FFmpeg derives a boolean (avctx->max_b_frames > 0, reported as
// depth 2 for HEVC) and never uses the requested value as a tunable depth,
// so anything above 1 fails closed instead of implying fake granularity.
const MaxBenchmarkBFrames = 1

// MaxBenchmarkRefFrames bounds explicit max_ref_frames.
const MaxBenchmarkRefFrames = 16

// libX265Presets is the fixed, safe libx265 -preset enum. Arbitrary values are
// rejected everywhere to preserve the fail-closed, no-raw-args boundary.
var libX265Presets = map[string]bool{
	"ultrafast": true, "superfast": true, "veryfast": true, "faster": true,
	"fast": true, "medium": true, "slow": true, "slower": true,
	"veryslow": true, "placebo": true,
}

// NormalizeVideoCodec lowercases and trims a codec identifier.
func NormalizeVideoCodec(codec string) string {
	return strings.ToLower(strings.TrimSpace(codec))
}

// IsValidLibX265Preset reports whether preset belongs to the libx265 enum.
func IsValidLibX265Preset(preset string) bool {
	return libX265Presets[strings.ToLower(strings.TrimSpace(preset))]
}

// ValidLibX265Presets returns the allowed libx265 presets in a stable order.
func ValidLibX265Presets() []string {
	return []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow", "placebo"}
}

func DigestPlan(p *Plan) (string, error) {
	if p == nil {
		return "", fmt.Errorf("plan is nil")
	}
	cp := *p
	cp.PlanDigest = ""
	b, err := json.Marshal(cp)
	if err != nil {
		return "", fmt.Errorf("serializing plan digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

type ConversionRecord struct {
	StreamType  string `json:"stream_type"`
	StreamIndex int    `json:"stream_index"`
	FromCodec   string `json:"from_codec"`
	ToCodec     string `json:"to_codec"`
	Reason      string `json:"reason"`
}

func ContainerExtension(container string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(container)) {
	case "mkv", "matroska":
		return ".mkv", nil
	default:
		return "", fmt.Errorf("unsupported container %q (fail closed)", container)
	}
}

type Request struct {
	ID                  string `json:"id"`
	SourcePath          string `json:"source_path"`
	CandidatePath       string `json:"candidate_path"`
	Profile             string `json:"profile"`
	Plan                *Plan  `json:"plan,omitempty"`
	IdempotencyKey      string `json:"idempotency_key,omitempty"`
	ExecutionSpecDigest string `json:"execution_spec_digest,omitempty"`
}
type Job struct {
	ID string `json:"id"`
}

// ProgressSnapshot is the last observed encoder update. A nil snapshot means
// no measurement has arrived; zero-valued rates are not evidence of a stall.
type ProgressSnapshot struct {
	Progress  float64   `json:"progress"`
	FPS       float64   `json:"fps"`
	Speed     float64   `json:"speed"`
	UpdatedAt time.Time `json:"updated_at"`
}

// JobTelemetry is additive to the stable queued/running/terminal status model.
// Phase describes actual work; runner slots include preparation and publication
// as well as encoding. Durations are omitted when older workers lack evidence.
type JobTelemetry struct {
	Phase                  string            `json:"phase,omitempty"`
	CreatedAt              time.Time         `json:"created_at,omitzero"`
	StartedAt              time.Time         `json:"started_at,omitzero"`
	FinishedAt             time.Time         `json:"finished_at,omitzero"`
	EncodeStartedAt        time.Time         `json:"encode_started_at,omitzero"`
	EncodeFinishedAt       time.Time         `json:"encode_finished_at,omitzero"`
	ValidationStartedAt    time.Time         `json:"validation_started_at,omitzero"`
	ValidationFinishedAt   time.Time         `json:"validation_finished_at,omitzero"`
	QueueDurationMs        *int64            `json:"queue_duration_ms,omitempty"`
	EncodeDurationMs       *int64            `json:"encode_duration_ms,omitempty"`
	ValidationDurationMs   *int64            `json:"validation_duration_ms,omitempty"`
	WallDurationMs         *int64            `json:"wall_duration_ms,omitempty"`
	LastProgressAt         time.Time         `json:"last_progress_at,omitzero"`
	WorkerHeartbeatAt      time.Time         `json:"worker_heartbeat_at,omitzero"`
	ProgressIsStale        bool              `json:"progress_is_stale"`
	LastKnownProgress      *ProgressSnapshot `json:"last_known_progress,omitempty"`
	WorkerSlotsTotal       int               `json:"worker_slots_total,omitempty"`
	WorkerSlotsUsed        int               `json:"worker_slots_used"`
	QueuePosition          int               `json:"queue_position,omitempty"`
	FinalizationRetryCount int               `json:"finalization_retry_count,omitempty"`
	NextFinalizationAt     time.Time         `json:"next_finalization_at,omitzero"`
	RecoveryRequired       bool              `json:"recovery_required,omitempty"`
	ErrorClass             string            `json:"error_class,omitempty"`
	StorageBackend         string            `json:"storage_backend,omitempty"`
	NavigatorrPath         string            `json:"navigatorr_path,omitempty"`
	WorkerResolvedPath     string            `json:"worker_resolved_path,omitempty"`
	SMBShare               string            `json:"smb_share,omitempty"`
	SMBRelativePath        string            `json:"smb_relative_path,omitempty"`
}

type JobStatus struct {
	JobTelemetry
	ID                    string             `json:"id"`
	Status                string             `json:"status"`
	Progress              float64            `json:"progress"`
	FPS                   float64            `json:"fps"`
	Speed                 float64            `json:"speed"`
	CandidatePath         string             `json:"candidate_path"`
	Error                 string             `json:"error,omitempty"`
	Profile               string             `json:"profile,omitempty"`
	RecipeVersion         string             `json:"recipe_version,omitempty"`
	RecipeDigest          string             `json:"recipe_digest,omitempty"`
	PlanDigest            string             `json:"plan_digest,omitempty"`
	Container             string             `json:"container,omitempty"`
	VideoCodec            string             `json:"video_codec,omitempty"`
	Quality               int                `json:"quality,omitempty"`
	VideoProfile          string             `json:"video_profile,omitempty"`
	PixelFormat           string             `json:"pixel_format,omitempty"`
	PrioritizeSpeed       *bool              `json:"prioritize_speed,omitempty"`
	SpatialAQ             *bool              `json:"spatial_aq,omitempty"`
	Realtime              *bool              `json:"realtime,omitempty"`
	AverageBitrateKbps    int                `json:"average_bitrate_kbps,omitempty"`
	MaxBitrateKbps        int                `json:"max_bitrate_kbps,omitempty"`
	ConstantBitrate       *bool              `json:"constant_bitrate,omitempty"`
	QMin                  *int               `json:"qmin,omitempty"`
	QMax                  *int               `json:"qmax,omitempty"`
	GOPSize               *int               `json:"gop_size,omitempty"`
	BFrames               *int               `json:"b_frames,omitempty"`
	ClosedGOP             *bool              `json:"closed_gop,omitempty"`
	PowerEfficient        *bool              `json:"power_efficient,omitempty"`
	MaxRefFrames          *int               `json:"max_ref_frames,omitempty"`
	ExpectedBitDepth      int                `json:"expected_bit_depth,omitempty"`
	Attempt               int                `json:"attempt,omitempty"`
	RetryCount            int                `json:"retry_count,omitempty"`
	FallbackCount         int                `json:"fallback_count,omitempty"`
	AppliedFallbacks      []string           `json:"applied_fallbacks,omitempty"`
	FailureClassification string             `json:"failure_classification,omitempty"`
	Conversions           []ConversionRecord `json:"conversions,omitempty"`
}

type PathMapping struct {
	Local  string `json:"local" yaml:"local"`
	Remote string `json:"remote" yaml:"remote"`
}
type SSHConfig struct {
	Host           string        `json:"host" yaml:"host"`
	Port           int           `json:"port,omitempty" yaml:"port,omitempty"`
	User           string        `json:"user" yaml:"user"`
	Command        string        `json:"command" yaml:"command"`
	IdentityFile   string        `json:"identity_file" yaml:"identity_file"`
	KnownHostsFile string        `json:"known_hosts_file,omitempty" yaml:"known_hosts_file,omitempty"`
	ConnectTimeout time.Duration `json:"connect_timeout" yaml:"connect_timeout"`
	CommandTimeout time.Duration `json:"command_timeout,omitempty" yaml:"command_timeout,omitempty"`
	PathMappings   []PathMapping `json:"path_mappings" yaml:"path_mappings"`
}
