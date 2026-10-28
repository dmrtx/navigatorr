package transcode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

// WorkerProtocolVersion is the current version of the worker capability
// protocol.
//
// Version 2 is a breaking wire-schema change: normal transcode submit and
// BenchmarkRequest payloads now carry `source_sha256`. Worker HTTP decoders use
// DisallowUnknownFields, so an older (v1) worker rejects those payloads. New
// coordinators therefore reject a stale v1 worker during the capability
// handshake (before any submit) instead of deferring the failure. Server and
// worker must be upgraded together.
const WorkerProtocolVersion = 2

// EncoderCapabilities describes detailed supported profiles, pixel formats, and options for a specific encoder.
type EncoderCapabilities struct {
	Encoder      string   `json:"encoder"`
	Available    bool     `json:"available"`
	Profiles     []string `json:"profiles,omitempty"`
	PixelFormats []string `json:"pixel_formats,omitempty"`
	Options      []string `json:"options,omitempty"`
}

// VideoToolboxCapabilities is an alias to EncoderCapabilities for convenience and compatibility.
type VideoToolboxCapabilities = EncoderCapabilities

// ProbeError records non-fatal probe warnings or errors encountered during capability discovery,
// allowing callers to differentiate between confirmed absence and probe failure.
type ProbeError struct {
	Component string `json:"component"` // e.g. "encoders", "filters", "videotoolbox"
	Message   string `json:"message"`   // bounded error description
}

// QualityModelCapability is the result of executing an actual model and JSON
// measurement path, not merely finding the libvmaf filter in -filters output.
type QualityModelCapability struct {
	Available           bool    `json:"available"`
	MeasurementBitDepth int     `json:"measurement_bit_depth"`
	ScoreMin            float64 `json:"score_min"`
	ScoreMax            float64 `json:"score_max"`
	Reason              string  `json:"reason,omitempty"`
}

type QualityCapabilities struct {
	Native10Bit    bool                              `json:"native_10_bit,omitempty"`
	LibvmafVersion string                            `json:"libvmaf_version,omitempty"`
	Models         map[string]QualityModelCapability `json:"models,omitempty"`
	CAMBIFullRef   bool                              `json:"cambi_full_ref"`
	CAMBIOutput    string                            `json:"cambi_output,omitempty"`
	// HFRTemporalDecimation is the VMAF decimation factor the worker executed
	// on a synthetic high-frame-rate pair. Zero means HFR sources are
	// unsupported (older workers), so coordinators must not dispatch them.
	HFRTemporalDecimation int `json:"hfr_temporal_decimation,omitempty"`
	// HFRProbeReason says why HFRTemporalDecimation is zero on a worker that
	// ran the probe. It never grants support.
	HFRProbeReason string `json:"hfr_probe_reason,omitempty"`
	ProbeError     string `json:"probe_error,omitempty"`
}

// WorkerCapabilities represents the versioned capability report of a transcode worker node.
type WorkerCapabilities struct {
	X265TwoPass              bool                 `json:"x265_two_pass,omitempty"`
	HEAACAudio               bool                 `json:"he_aac_audio,omitempty"`
	AutomaticModesV2         bool                 `json:"automatic_modes_v2,omitempty"`
	AutomaticQualityPruning  bool                 `json:"automatic_quality_pruning,omitempty"`
	SavingsPrecheck          bool                 `json:"savings_precheck,omitempty"`
	Podcast                  *PodcastCapabilities `json:"podcast,omitempty"`
	ExplicitMain10Benchmarks bool                 `json:"explicit_main10_benchmarks,omitempty"`
	CompactAudio             bool                 `json:"compact_audio,omitempty"`
	CoverArt                 bool                 `json:"cover_art,omitempty"`
	// DeferredPublication reports a worker that accepts publish_policy
	// on_approval: it keeps a validated candidate on its local disk and
	// publishes it to the share only when asked (POST /v1/jobs/{id}/publish).
	// It is advertised only when the worker's configuration enables retention.
	DeferredPublication bool `json:"deferred_publication,omitempty"`
	// StartRetentionAdmission reports a worker that applies its retention
	// limits when an on_approval job's slot comes up rather than at submit,
	// and accepts retention_wait on a submit.
	StartRetentionAdmission bool `json:"start_retention_admission,omitempty"`
	// SizeModeV3 reports a worker that executes size policy v3: lossless
	// audio compaction, 8-bit to Main10 size encodes and duration-scaled
	// final validation windows.
	SizeModeV3 bool `json:"size_mode_v3,omitempty"`
	// SizeAudioOnly reports a worker that runs size v3 audio-only plans:
	// the video stream is copied and proven unchanged packet by packet
	// from ffprobe's demuxed payload hashes, and lossless audio is
	// compacted to AAC.
	SizeAudioOnly bool `json:"size_audio_only,omitempty"`
	// SizeAudioLevels reports a worker that runs size policy v5 plans: the
	// size goal's audio levels (AAC-LC and HE-AAC per track, from the source
	// bitrate). Older workers refuse the audio modes, so the coordinator checks
	// this before it submits a v5 plan.
	SizeAudioLevels bool `json:"size_audio_levels,omitempty"`
	// TrackSelection reports a worker that runs plans of mode policy v6: it maps
	// exactly the audio and subtitle streams the plan keeps and validates that
	// the output has those tracks and no others. Older workers refuse the plan,
	// so the coordinator checks this before it submits one. Like the flags
	// before it, it is not part of the pinned capability fingerprint (the
	// fingerprint of an unchanged machine must not move).
	TrackSelection bool `json:"track_selection,omitempty"`
	// LearnSourceDigest reports a worker that takes a submit without a source
	// digest (with the size the coordinator probed), hashes the original while
	// it copies it, and reports the digest in the job status. The coordinator
	// then reads a source over the network once instead of hashing it before the
	// worker starts. Older workers get the coordinator's own hash. Like
	// TrackSelection, it is not part of the pinned capability fingerprint.
	LearnSourceDigest bool `json:"learn_source_digest,omitempty"`
	// SizeAudioObjectBed reports a worker that runs the size goal's bed audio
	// levels (AudioModeSizeAACBed, AudioModeSizeHEAACBed), which also convert
	// lossy Atmos and DTS:X tracks to their channel bed. Older workers refuse
	// those modes, so the coordinator checks this before it submits one. Like
	// TrackSelection, it is not part of the pinned capability fingerprint.
	SizeAudioObjectBed bool `json:"size_audio_object_bed,omitempty"`
	// SearchBudgetExtension reports a worker that accepts search_extension on
	// mode benchmarks: the search budget stretches while work keeps finishing,
	// up to a hard cap. Older workers reject the field.
	SearchBudgetExtension bool `json:"search_budget_extension,omitempty"`
	// AutomaticSearchRules reports a worker that accepts automatic_search on
	// size-mode benchmarks: a selection margin over the VMAF p5 and
	// worst-window minimums, quality-first verification after a holdout
	// fails on quality, and a quality fallback tried preset by preset.
	// Older workers reject the field.
	AutomaticSearchRules bool `json:"automatic_search_rules,omitempty"`
	// AutomaticGridVerification reports a worker that verifies a size search's
	// candidates on every window of the final validation grid (five to twelve
	// sample windows, holdout_windows "final_grid"), keeps verifying until one
	// passes or none is eligible (verify_until_exhausted), and measures the
	// fresh final validation windows apart from the verdict. Older workers
	// reject those fields, so the server sends them only to workers that
	// advertise this.
	AutomaticGridVerification bool `json:"automatic_grid_verification,omitempty"`
	// AutomaticQualityWalk reports a worker that accepts quality_walk and
	// refinement_quality_values in automatic_search: the quality fallback is
	// measured one rung at a time, most compressed first, per encoder, and each
	// encoder stops at its first rung that passes; a rung between a passing and a
	// failing ladder rung is measured to tighten the result; and up to
	// MaxAutomaticWalkCandidates candidates (MaxAutomaticWalkHardwareCandidates
	// beside the hardware ladder) are accepted. Older workers reject those fields
	// and the longer candidate list, so the server sends them only to workers that
	// advertise this.
	AutomaticQualityWalk bool `json:"automatic_quality_walk,omitempty"`
	// HardwareSearch reports a worker that accepts hardware_search on size
	// benchmarks: VideoToolbox candidates that win within a frozen size margin
	// of the x265 winner. Older workers reject the field.
	HardwareSearch bool `json:"hardware_search,omitempty"`
	// HardwareFallbackSearch reports a worker that accepts fallback_quality_values
	// in hardware_search and measures those higher-quality VideoToolbox
	// candidates when the whole ladder fails quality. Older workers reject the
	// field and the larger candidate list.
	HardwareFallbackSearch bool `json:"hardware_fallback_search,omitempty"`
	// HardwareSelectionMargin reports a worker that accepts
	// extra_selection_margin_vmaf in hardware_search and holds VideoToolbox
	// candidates to that larger VMAF margin. Older workers reject the field.
	HardwareSelectionMargin bool `json:"hardware_selection_margin,omitempty"`
	// SearchContinuation reports a worker that accepts continues_benchmark on
	// a size search that follows a failed VideoToolbox result: it reuses the
	// earlier search's measured x265 samples instead of encoding them again.
	// Older workers reject the field.
	SearchContinuation bool `json:"search_continuation,omitempty"`
	// CompactTranscode reports an executable proof of the compact SDR chain
	// (setparams, swscale, lut3d and sidedata delete) in the worker FFmpeg.
	CompactTranscode      bool                           `json:"compact_transcode,omitempty"`
	ProtocolVersion       int                            `json:"protocol_version"`
	WorkerVersion         string                         `json:"worker_version,omitempty"`
	BuildGitCommit        string                         `json:"build_git_commit,omitempty"`
	FFmpegVersion         string                         `json:"ffmpeg_version"`
	FFmpegPath            string                         `json:"ffmpeg_path,omitempty"`
	Encoders              map[string]bool                `json:"encoders"`
	EncoderDetails        map[string]EncoderCapabilities `json:"encoder_details,omitempty"`
	Filters               map[string]bool                `json:"filters"`
	Quality               *QualityCapabilities           `json:"quality,omitempty"`
	ProbeErrors           []ProbeError                   `json:"probe_errors,omitempty"`
	CapabilityFingerprint string                         `json:"capability_fingerprint,omitempty"`
}

// Fingerprint returns the canonical capability fingerprint.
func (c WorkerCapabilities) Fingerprint() string {
	return c.CapabilityFingerprint
}

// HasProbeErrors returns true if any non-fatal probe errors were reported during capability discovery.
func (c WorkerCapabilities) HasProbeErrors() bool {
	return len(c.ProbeErrors) > 0
}

// HasComponentError returns true if a probe error exists for the given component.
func (c WorkerCapabilities) HasComponentError(component string) bool {
	for _, pe := range c.ProbeErrors {
		if strings.EqualFold(pe.Component, component) {
			return true
		}
	}
	return false
}

// fingerprintPayload defines the stable subset of capabilities used to compute
// the cache identity / capability fingerprint.
// Machine-specific paths like FFmpegPath are excluded to ensure capability equivalence
// across different worker nodes with identical capability sets.
type fingerprintPayload struct {
	X265TwoPass               bool                           `json:"x265_two_pass,omitempty"`
	HEAACAudio                bool                           `json:"he_aac_audio,omitempty"`
	AutomaticModesV2          bool                           `json:"automatic_modes_v2,omitempty"`
	AutomaticQualityPruning   bool                           `json:"automatic_quality_pruning,omitempty"`
	SavingsPrecheck           bool                           `json:"savings_precheck,omitempty"`
	Podcast                   *PodcastCapabilities           `json:"podcast,omitempty"`
	ExplicitMain10Benchmarks  bool                           `json:"explicit_main10_benchmarks,omitempty"`
	CompactAudio              bool                           `json:"compact_audio,omitempty"`
	CoverArt                  bool                           `json:"cover_art,omitempty"`
	DeferredPublication       bool                           `json:"deferred_publication,omitempty"`
	StartRetentionAdmission   bool                           `json:"start_retention_admission,omitempty"`
	SizeModeV3                bool                           `json:"size_mode_v3,omitempty"`
	SizeAudioOnly             bool                           `json:"size_audio_only,omitempty"`
	SizeAudioLevels           bool                           `json:"size_audio_levels,omitempty"`
	SearchBudgetExtension     bool                           `json:"search_budget_extension,omitempty"`
	AutomaticSearchRules      bool                           `json:"automatic_search_rules,omitempty"`
	AutomaticGridVerification bool                           `json:"automatic_grid_verification,omitempty"`
	AutomaticQualityWalk      bool                           `json:"automatic_quality_walk,omitempty"`
	HardwareSearch            bool                           `json:"hardware_search,omitempty"`
	HardwareFallbackSearch    bool                           `json:"hardware_fallback_search,omitempty"`
	HardwareSelectionMargin   bool                           `json:"hardware_selection_margin,omitempty"`
	SearchContinuation        bool                           `json:"search_continuation,omitempty"`
	CompactTranscode          bool                           `json:"compact_transcode,omitempty"`
	ProtocolVersion           int                            `json:"protocol_version"`
	WorkerVersion             string                         `json:"worker_version,omitempty"`
	BuildGitCommit            string                         `json:"build_git_commit,omitempty"`
	FFmpegVersion             string                         `json:"ffmpeg_version"`
	Encoders                  map[string]bool                `json:"encoders"`
	EncoderDetails            map[string]EncoderCapabilities `json:"encoder_details,omitempty"`
	Filters                   map[string]bool                `json:"filters"`
	Quality                   *QualityCapabilities           `json:"quality,omitempty"`
	ProbeErrors               []ProbeError                   `json:"probe_errors,omitempty"`
}

// ComputeCapabilityFingerprint returns a deterministic sha256 digest of WorkerCapabilities
// representing its capability equivalence for caching and validation.
// Note: This is a cache identity fingerprint, NOT a cryptographic authentication proof.
func ComputeCapabilityFingerprint(caps WorkerCapabilities) (string, error) {
	// Deep-clone and sort encoder details
	detCopy := make(map[string]EncoderCapabilities, len(caps.EncoderDetails))
	for k, v := range caps.EncoderDetails {
		ec := v
		if len(ec.Profiles) > 1 {
			ec.Profiles = append([]string(nil), ec.Profiles...)
			sort.Strings(ec.Profiles)
		}
		if len(ec.PixelFormats) > 1 {
			ec.PixelFormats = append([]string(nil), ec.PixelFormats...)
			sort.Strings(ec.PixelFormats)
		}
		if len(ec.Options) > 1 {
			ec.Options = append([]string(nil), ec.Options...)
			sort.Strings(ec.Options)
		}
		detCopy[k] = ec
	}

	payload := fingerprintPayload{
		X265TwoPass: caps.X265TwoPass, HEAACAudio: caps.HEAACAudio,
		AutomaticModesV2:          caps.AutomaticModesV2,
		AutomaticQualityPruning:   caps.AutomaticQualityPruning,
		SavingsPrecheck:           caps.SavingsPrecheck,
		Podcast:                   caps.Podcast,
		ExplicitMain10Benchmarks:  caps.ExplicitMain10Benchmarks,
		CompactAudio:              caps.CompactAudio,
		CoverArt:                  caps.CoverArt,
		DeferredPublication:       caps.DeferredPublication,
		StartRetentionAdmission:   caps.StartRetentionAdmission,
		SizeModeV3:                caps.SizeModeV3,
		SizeAudioOnly:             caps.SizeAudioOnly,
		SizeAudioLevels:           caps.SizeAudioLevels,
		SearchBudgetExtension:     caps.SearchBudgetExtension,
		AutomaticSearchRules:      caps.AutomaticSearchRules,
		AutomaticGridVerification: caps.AutomaticGridVerification,
		AutomaticQualityWalk:      caps.AutomaticQualityWalk,
		HardwareSearch:            caps.HardwareSearch,
		HardwareFallbackSearch:    caps.HardwareFallbackSearch,
		HardwareSelectionMargin:   caps.HardwareSelectionMargin,
		SearchContinuation:        caps.SearchContinuation,
		CompactTranscode:          caps.CompactTranscode,
		ProtocolVersion:           caps.ProtocolVersion,
		WorkerVersion:             caps.WorkerVersion,
		BuildGitCommit:            caps.BuildGitCommit,
		FFmpegVersion:             caps.FFmpegVersion,
		Encoders:                  caps.Encoders,
		EncoderDetails:            detCopy,
		Filters:                   caps.Filters,
		Quality:                   caps.Quality,
		ProbeErrors:               caps.ProbeErrors,
	}

	if len(payload.ProbeErrors) > 1 {
		payload.ProbeErrors = append([]ProbeError(nil), payload.ProbeErrors...)
		sort.Slice(payload.ProbeErrors, func(i, j int) bool {
			if payload.ProbeErrors[i].Component != payload.ProbeErrors[j].Component {
				return payload.ProbeErrors[i].Component < payload.ProbeErrors[j].Component
			}
			return payload.ProbeErrors[i].Message < payload.ProbeErrors[j].Message
		})
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("serializing worker capabilities for fingerprint: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyCapabilityFingerprint checks that the fingerprint on WorkerCapabilities matches the computed digest.
func VerifyCapabilityFingerprint(caps WorkerCapabilities) error {
	if caps.CapabilityFingerprint == "" {
		return fmt.Errorf("worker capabilities missing capability fingerprint (fail closed)")
	}
	expected, err := ComputeCapabilityFingerprint(caps)
	if err != nil {
		return fmt.Errorf("failed computing capability fingerprint: %w", err)
	}
	if caps.CapabilityFingerprint != expected {
		return fmt.Errorf("worker capability fingerprint mismatch: got %s, want %s (fail closed)", caps.CapabilityFingerprint, expected)
	}
	return nil
}

// SupportsHFRQuality reports a proven decimated VMAF measurement path for
// progressive sources between quality.HFRMinFPS and quality.HFRMaxFPS. The
// proof was run with quality.ProbeModelID.
func (c WorkerCapabilities) SupportsHFRQuality() bool {
	return c.Quality != nil && c.Quality.ProbeError == "" && c.Quality.Models[quality.ProbeModelID].Available && c.Quality.HFRTemporalDecimation == quality.HFRDecimationFactor
}

// SupportsQualityFPS is the single coordinator/worker frame-rate gate for
// modeled VMAF measurement of one video stream: native rates below
// quality.HFRMinFPS always, the progressive HFR class only on a worker that
// proved decimation, nothing else. fieldOrder is the stream's ffprobe
// field_order; an interlaced stream in the HFR class may report its field
// rate and is never admitted.
func (c WorkerCapabilities) SupportsQualityFPS(fps float64, fieldOrder string) bool {
	factor, err := quality.TemporalDecimationForStream(fps, fieldOrder)
	return err == nil && (factor == 1 || c.SupportsHFRQuality())
}

// SupportsCompactTranscode reports a worker that can run compact mode: the
// proven SDR chain plus compact audio.
func (c WorkerCapabilities) SupportsCompactTranscode() bool {
	return c.CompactTranscode && c.CompactAudio
}

// Both metrics must have executed on native 10-bit synthetic inputs without
// automatic pixel conversion. Older workers cannot claim this capability.
func (c WorkerCapabilities) SupportsNative10BitQuality() bool {
	return c.Quality != nil && c.Quality.Native10Bit && c.Quality.ProbeError == "" && c.Quality.Models[quality.ProbeModelID].Available && c.Quality.CAMBIFullRef
}
