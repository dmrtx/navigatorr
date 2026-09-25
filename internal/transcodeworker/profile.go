package transcodeworker

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

type WorkerCapabilities struct {
	Containers           []string `json:"containers"`
	VideoCodecs          []string `json:"video_codecs"`
	AudioOperations      []string `json:"audio_operations"`
	SubtitleOperations   []string `json:"subtitle_operations"`
	SubtitleEncodeCodecs []string `json:"subtitle_encode_codecs"`
	SubtitleCopyCodecs   []string `json:"subtitle_copy_codecs"`
	SubtitleConversions  []string `json:"subtitle_conversions"`
	Preservation         []string `json:"preservation"`
}

func Capabilities() WorkerCapabilities {
	return WorkerCapabilities{
		Containers:           []string{"mkv"},
		VideoCodecs:          []string{"hevc_videotoolbox", "libx265"},
		AudioOperations:      []string{"copy"},
		SubtitleOperations:   []string{"copy", "transcode"},
		SubtitleEncodeCodecs: []string{"subrip"},
		SubtitleCopyCodecs:   []string{"subrip", "srt", "ass", "ssa", "hdmv_pgs_subtitle", "pgs", "dvd_subtitle", "vobsub", "dvb_subtitle", "dvb_teletext", "webvtt", "text"},
		SubtitleConversions:  []string{"mov_text->subrip"},
		Preservation:         []string{"metadata", "chapters", "attachments"},
	}
}

func ValidatePlan(p *transcode.Plan) error {
	if p == nil {
		return fmt.Errorf("transcode plan is nil (fail closed)")
	}
	if normalizeContainer(p.Container) != "mkv" {
		return fmt.Errorf("unsupported container %q (only mkv is allowed; fail closed)", p.Container)
	}
	if norm(p.VideoCodec) != "hevc_videotoolbox" && norm(p.VideoCodec) != "libx265" {
		return fmt.Errorf("unsupported video codec %q (fail closed)", p.VideoCodec)
	}
	// VideoToolbox rate control is exactly one dimension: quality mode
	// (-q:v 1..100) or average-bitrate mode (-b:v with Quality unset). Full
	// cross-field rules live in BuildVideoEncoderArgs; this gate only admits
	// the two shapes. libx265 CRF bounds are enforced by BuildVideoEncoderArgs.
	if norm(p.VideoCodec) == "hevc_videotoolbox" {
		if p.AverageBitrateKbps > 0 {
			if p.Quality != 0 {
				return fmt.Errorf("quality (%d) and average_bitrate_kbps (%d) are mutually exclusive (fail closed)", p.Quality, p.AverageBitrateKbps)
			}
		} else if p.Quality < 1 || p.Quality > 100 {
			return fmt.Errorf("invalid quality level %d (must be 1-100; fail closed)", p.Quality)
		}
	}
	if _, err := BuildVideoEncoderArgs(p); err != nil {
		return err
	}
	if norm(p.AudioMode) != "copy" {
		return fmt.Errorf("unsupported audio mode %q (fail closed)", p.AudioMode)
	}
	if norm(p.SubtitleMode) != "preserve" {
		return fmt.Errorf("unsupported subtitle mode %q (fail closed)", p.SubtitleMode)
	}
	if !p.PreserveMetadata || !p.PreserveChapters || !p.PreserveAttachments {
		return fmt.Errorf("current worker requires metadata, chapters, and attachments preservation (fail closed)")
	}
	if q := p.QualityValidation; q != nil {
		if q.Quality.FinalValidation == nil || q.Quality.FinalValidation.Mode != "sampled" {
			return fmt.Errorf("quality validation plan must use sampled mode")
		}
		if q.Metric != "vmaf" && q.Metric != "both" && q.Metric != "vmaf+ssim" && q.Metric != "ssim" {
			return fmt.Errorf("unsupported final quality metric")
		}
		if q.Metric != "ssim" && (q.Quality.VMAF == nil || q.Quality.VMAF.Model == "") {
			return fmt.Errorf("final VMAF validation requires an explicit model")
		}
		if len(q.Samples) == 0 || len(q.Samples) > transcode.MaxBenchmarkSamples {
			return fmt.Errorf("invalid final quality sample count")
		}
		if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(q.BenchmarkRequestDigest) {
			return fmt.Errorf("final quality benchmark request digest is missing")
		}
		seenSample := map[int]bool{}
		for _, sample := range q.Samples {
			if sample.Index < 0 || seenSample[sample.Index] || math.IsNaN(sample.StartSeconds) || math.IsInf(sample.StartSeconds, 0) || sample.StartSeconds < 0 || math.IsNaN(sample.DurationSeconds) || math.IsInf(sample.DurationSeconds, 0) || sample.DurationSeconds <= 0 || sample.DurationSeconds > 120 {
				return fmt.Errorf("invalid final quality sample")
			}
			seenSample[sample.Index] = true
		}
		if q.Quality.VMAF != nil && q.Quality.VMAF.Model != "" {
			if _, err := quality.Model(q.Quality.VMAF.Model); err != nil {
				return err
			}
		}
	}
	if strings.TrimSpace(p.RecipeVersion) == "" || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(p.RecipeDigest) {
		return fmt.Errorf("valid recipe identity is required in resolved plans (fail closed)")
	}
	expectedDigest, err := transcode.DigestPlan(p)
	if err != nil || p.PlanDigest == "" || p.PlanDigest != expectedDigest {
		return fmt.Errorf("plan digest is missing or does not match the resolved plan (fail closed)")
	}
	seen := map[int]bool{}
	copyCodecs := map[string]bool{"subrip": true, "srt": true, "ass": true, "ssa": true, "hdmv_pgs_subtitle": true, "pgs": true, "dvd_subtitle": true, "vobsub": true, "dvb_subtitle": true, "dvb_teletext": true, "webvtt": true, "text": true}
	for _, a := range p.SubtitleActions {
		if a.SourceStreamIndex < 0 || a.TypeIndex < 0 {
			return fmt.Errorf("invalid subtitle stream index in plan (fail closed)")
		}
		if seen[a.TypeIndex] {
			return fmt.Errorf("duplicate subtitle action for subtitle #%d (fail closed)", a.TypeIndex)
		}
		seen[a.TypeIndex] = true
		if norm(a.SourceCodec) == "" {
			return fmt.Errorf("subtitle action #%d missing source codec", a.TypeIndex)
		}
		switch norm(a.Operation) {
		case "copy":
			if norm(a.Codec) != "copy" {
				return fmt.Errorf("subtitle copy action #%d must use codec=copy", a.TypeIndex)
			}
			if !copyCodecs[norm(a.SourceCodec)] {
				return fmt.Errorf("subtitle copy action #%d requests unsupported source codec %q", a.TypeIndex, a.SourceCodec)
			}
		case "transcode":
			if norm(a.Codec) != "subrip" || norm(a.SourceCodec) != "mov_text" {
				return fmt.Errorf("subtitle action #%d requests unsupported worker conversion %s -> %s", a.TypeIndex, a.SourceCodec, a.Codec)
			}
		default:
			return fmt.Errorf("subtitle action #%d requests unsupported operation %q", a.TypeIndex, a.Operation)
		}
	}
	if p.Resilience.MaxAttempts < 1 || p.Resilience.MaxAttempts > 10 {
		return fmt.Errorf("invalid max_attempts %d", p.Resilience.MaxAttempts)
	}
	if p.Resilience.TransientRetries < 0 || p.Resilience.TransientRetries >= p.Resilience.MaxAttempts {
		return fmt.Errorf("invalid transient_retries %d", p.Resilience.TransientRetries)
	}
	if p.Resilience.MaxFallbacks < 0 || p.Resilience.MaxFallbacks > 4 {
		return fmt.Errorf("invalid max_fallbacks %d", p.Resilience.MaxFallbacks)
	}
	if len(p.AppliedFallbacks) > p.Resilience.MaxFallbacks {
		return fmt.Errorf("applied fallback count exceeds max_fallbacks (fail closed)")
	}
	seenFallbacks := map[string]bool{}
	for _, f := range p.AppliedFallbacks {
		if f != "container_subtitle_incompatible:apply_container_conversion" || seenFallbacks[f] {
			return fmt.Errorf("unsupported or repeated applied fallback %q", f)
		}
		seenFallbacks[f] = true
	}
	allowedRetry := map[string]bool{"worker_busy": true, "ssh_transient": true, "worker_unreachable": true, "encoder_temporarily_unavailable": true}
	for _, c := range p.Resilience.RetryOn {
		if !allowedRetry[c] {
			return fmt.Errorf("unsupported retry failure class %q", c)
		}
	}
	return nil
}

// ResolveWorkerPlan is a defense-in-depth boundary. Normal Navigatorr submissions MUST
// carry a fully resolved Plan. The profile-only shim exists solely for legacy hevc-vt
// callers and intentionally supports media without subtitle policy decisions.
func ResolveWorkerPlan(profile string, supplied *transcode.Plan) (*transcode.Plan, error) {
	if supplied != nil {
		if err := ValidatePlan(supplied); err != nil {
			return nil, fmt.Errorf("validating transcode plan: %w", err)
		}
		cp := *supplied
		cp.Container = normalizeContainer(cp.Container)
		cp.VideoCodec = norm(cp.VideoCodec)
		cp.AudioMode = norm(cp.AudioMode)
		cp.SubtitleMode = norm(cp.SubtitleMode)
		cp.SubtitleActions = append([]transcode.SubtitleAction(nil), supplied.SubtitleActions...)
		cp.AppliedFallbacks = append([]string(nil), supplied.AppliedFallbacks...)
		cp.Resilience.RetryBackoffSeconds = append([]int(nil), supplied.Resilience.RetryBackoffSeconds...)
		cp.Resilience.RetryOn = append([]string(nil), supplied.Resilience.RetryOn...)
		return &cp, nil
	}
	name := strings.TrimSpace(profile)
	if name == "" {
		name = "hevc-vt"
	}
	if name != "hevc-vt" {
		return nil, fmt.Errorf("profile-only worker submissions are deprecated; unknown legacy profile %q (fail closed)", name)
	}
	p := &transcode.Plan{Container: "mkv", VideoCodec: "hevc_videotoolbox", Quality: 65, AudioMode: "copy", SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "legacy-worker-shim", RecipeDigest: "sha256:0000000000000000000000000000000000000000000000000000000000000000", Resilience: transcode.ResiliencePlan{MaxAttempts: 1}}
	digest, err := transcode.DigestPlan(p)
	if err != nil {
		return nil, err
	}
	p.PlanDigest = digest
	return p, nil
}
func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func normalizeContainer(s string) string {
	s = norm(s)
	if s == "matroska" {
		return "mkv"
	}
	return s
}
