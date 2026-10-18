package transcodeworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

var qualityProbeCache = struct {
	sync.Mutex
	results map[string]struct {
		caps transcode.QualityCapabilities
		at   time.Time
	}
}{results: make(map[string]struct {
	caps transcode.QualityCapabilities
	at   time.Time
})}

func cachedQualityCapabilities(ctx context.Context, ffmpegPath, scratchRoot string) transcode.QualityCapabilities {
	info, err := os.Stat(ffmpegPath)
	if err != nil {
		return probeQualityCapabilities(ctx, ffmpegPath, scratchRoot)
	}
	key := fmt.Sprintf("%s:%d:%d", ffmpegPath, info.Size(), info.ModTime().UnixNano())
	qualityProbeCache.Lock()
	if cached, ok := qualityProbeCache.results[key]; ok && time.Since(cached.at) < 5*time.Minute {
		qualityProbeCache.Unlock()
		return cloneQualityCapabilities(cached.caps)
	}
	qualityProbeCache.Unlock()
	result := probeQualityCapabilities(ctx, ffmpegPath, scratchRoot)
	// A timed-out HFR probe proves nothing either way; retry it next time.
	if result.ProbeError == "" && result.HFRProbeReason != hfrProbeTimeout && ctx.Err() == nil {
		qualityProbeCache.Lock()
		qualityProbeCache.results[key] = struct {
			caps transcode.QualityCapabilities
			at   time.Time
		}{cloneQualityCapabilities(result), time.Now()}
		qualityProbeCache.Unlock()
	}
	return result
}

func cloneQualityCapabilities(c transcode.QualityCapabilities) transcode.QualityCapabilities {
	clone := c
	if c.Models != nil {
		clone.Models = make(map[string]transcode.QualityModelCapability, len(c.Models))
		for k, v := range c.Models {
			clone.Models[k] = v
		}
	}
	return clone
}

// probeQualityCapabilities uses a tiny synthetic pair and strict local timeout.
// A known model/feature rejection is an unavailable capability; a failed or
// malformed probe is retained separately and must never be treated as support.
func probeQualityCapabilities(ctx context.Context, ffmpegPath, scratchRoot string) transcode.QualityCapabilities {
	out := transcode.QualityCapabilities{Models: map[string]transcode.QualityModelCapability{}}
	model, _ := quality.Model("v1_1080p_3h")
	entry := transcode.QualityModelCapability{MeasurementBitDepth: model.MeasurementBitDepth, ScoreMin: model.MinScore, ScoreMax: model.MaxScore}
	out.Models[model.ID] = entry
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := os.MkdirAll(scratchRoot, 0700); err != nil {
		out.ProbeError = "quality_probe_scratch_unavailable"
		return out
	}
	dir, err := os.MkdirTemp(scratchRoot, "quality-probe-")
	if err != nil {
		out.ProbeError = "quality_probe_scratch_unavailable"
		return out
	}
	defer os.RemoveAll(dir)
	modelLog := filepath.Join(dir, "vmaf.json")
	nativeInputs, _ := quality.MetricPairInputs(1)
	modelFilter := nativeInputs + vmafModelFilter(model, 10, modelLog)
	if output, err := runSyntheticQualityProbe(probeCtx, ffmpegPath, modelFilter); err != nil {
		entry.Reason = "model_initialization_failed"
		out.Models[model.ID] = entry
		if !knownQualityUnsupported(string(output)) {
			out.ProbeError = "vmaf_probe_execution_failed"
		}
	} else if data, err := os.ReadFile(modelLog); err != nil {
		out.ProbeError = "vmaf_probe_log_missing"
	} else if stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 0, 0, 24, 1, 0.2, nil); err != nil {
		out.ProbeError = fmt.Sprintf("invalid VMAF probe evidence: %v", err)
	} else {
		entry.Available = true
		entry.Reason = ""
		out.Models[model.ID] = entry
		out.LibvmafVersion = stats.LibvmafVersion
	}
	if probeCtx.Err() != nil {
		out.ProbeError = "quality_probe_timeout_or_cancelled"
		return out
	}
	// Keep the full-reference guardrail in a separate pass: VMAF v1 already
	// registers CAMBI internally and may reject a second CAMBI instance.
	cambiLog := filepath.Join(dir, "cambi.json")
	cambiFilter := nativeInputs + fmt.Sprintf("libvmaf=model=version=vmaf_v0.6.1:feature=name=cambi\\\\:full_ref=true:log_fmt=json:log_path=%s:shortest=1:repeatlast=0", escapeFFmpegFilterPath(cambiLog))
	if output, err := runSyntheticQualityProbe(probeCtx, ffmpegPath, cambiFilter); err != nil {
		if !knownQualityUnsupported(string(output)) {
			out.ProbeError = "cambi_probe_execution_failed"
		}
	} else if data, err := os.ReadFile(cambiLog); err != nil {
		out.ProbeError = "cambi_probe_log_missing"
	} else {
		var shape struct {
			Version string `json:"version"`
			Frames  []struct {
				Metrics map[string]float64 `json:"metrics"`
			} `json:"frames"`
		}
		if err := json.Unmarshal(data, &shape); err != nil || len(shape.Frames) == 0 {
			out.ProbeError = "invalid CAMBI probe JSON"
		} else {
			// The dedicated name is unambiguous. Some libvmaf builds expose only
			// `cambi`; without a differential proof those remain unsupported.
			if _, ok := shape.Frames[0].Metrics["cambi_full_reference"]; ok {
				stats, parseErr := quality.ParseCAMBIFullReference(data, MaxMetricLogSizeBytes, "cambi_full_reference", 0, 0, 24)
				if parseErr != nil || stats.Max > 0.01 {
					out.ProbeError = "CAMBI full-reference identity probe produced invalid/nonzero deterioration"
				} else {
					out.CAMBIFullRef, out.CAMBIOutput = true, "cambi_full_reference"
					if out.LibvmafVersion == "" {
						out.LibvmafVersion = shape.Version
					}
				}
			}
		}
	}
	out.Native10Bit = out.ProbeError == "" && out.Models[model.ID].Available && out.CAMBIFullRef
	switch {
	case out.ProbeError != "":
		out.HFRProbeReason = "quality_probe_failed"
	case !out.Models[model.ID].Available:
		out.HFRProbeReason = "vmaf_model_unavailable"
	default:
		// A separate budget: a slow HFR probe must not mark the whole quality
		// probe as timed out and disable native-rate VMAF/CAMBI with it.
		hfrCtx, hfrCancel := context.WithTimeout(ctx, hfrProbeTimeoutDuration)
		out.HFRTemporalDecimation, out.HFRProbeReason = probeHFRDecimation(hfrCtx, ffmpegPath, dir, model)
		hfrCancel()
	}
	return out
}

// probeHFRDecimation executes the production decimated model graph on a
// temporally varying 48 fps pair. It requires every second frame, an aligned
// pair scoring near the top, and a candidate one source frame late scoring
// clearly lower, so it proves index alignment and not only the frame count.
// A worker that cannot prove it advertises zero with a reason and receives no
// HFR source.
func probeHFRDecimation(ctx context.Context, ffmpegPath, dir string, model quality.VMAFModel) (int, string) {
	measure := func(name, candidate string) (quality.VMAFStats, string) {
		logPath := filepath.Join(dir, name)
		inputs, err := quality.MetricPairInputs(quality.HFRDecimationFactor)
		if err != nil {
			return quality.VMAFStats{}, "hfr_probe_graph_invalid"
		}
		if _, err := runSyntheticQualityPair(ctx, ffmpegPath, inputs+vmafModelFilter(model, 10, logPath), candidate, hfrProbeSource); err != nil {
			if ctx.Err() != nil {
				return quality.VMAFStats{}, hfrProbeTimeout
			}
			return quality.VMAFStats{}, "hfr_probe_execution_failed"
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			return quality.VMAFStats{}, "hfr_probe_log_missing"
		}
		stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 0, 0, hfrProbeFPS, quality.HFRDecimationFactor, 0.2, nil)
		if err != nil {
			return quality.VMAFStats{}, "hfr_probe_log_invalid"
		}
		return stats, ""
	}
	aligned, reason := measure("vmaf-hfr.json", hfrProbeSource)
	if reason != "" {
		return 0, reason
	}
	if aligned.FramesTotal != quality.DecimatedFrameCount(hfrProbeFrames, quality.HFRDecimationFactor) {
		return 0, "hfr_probe_frame_count_mismatch"
	}
	if aligned.Min < hfrProbeAlignedMin {
		return 0, "hfr_probe_aligned_score_low"
	}
	shifted, reason := measure("vmaf-hfr-shifted.json", hfrProbeSource+",trim=start_frame=1")
	if reason != "" {
		return 0, reason
	}
	if shifted.Mean > aligned.Mean-hfrProbeShiftMargin {
		return 0, "hfr_probe_alignment_unproven"
	}
	return quality.HFRDecimationFactor, ""
}

const (
	hfrProbeFPS    = 48
	hfrProbeFrames = 24 // 0.5 s at hfrProbeFPS
	// Every frame moves the pattern, so a one-frame shift is a real mismatch.
	hfrProbeSource      = "nullsrc=size=640x360:rate=48:duration=0.5,format=yuv420p10le,geq=lum='64+mod(X+3*Y+29*N,877)':cb=512:cr=512"
	hfrProbeAlignedMin  = 90.0
	hfrProbeShiftMargin = 10.0
	hfrProbeTimeout     = "hfr_probe_timeout"
)

// hfrProbeTimeoutDuration bounds the HFR probe separately from the quality
// probe's own budget.
var hfrProbeTimeoutDuration = 20 * time.Second

func runSyntheticQualityProbe(ctx context.Context, ffmpegPath, filter string) ([]byte, error) {
	// The low two bits carry actual signal. Disabling automatic conversion
	// proves the metric accepts native 10-bit inputs instead of downconverting.
	source := "nullsrc=size=640x360:rate=24:duration=0.25,format=yuv420p10le,geq=lum='64+mod(X+3*Y,877)':cb=512:cr=512"
	return runSyntheticQualityPair(ctx, ffmpegPath, filter, source, source)
}

// runSyntheticQualityPair feeds candidate as [0:v] and reference as [1:v].
func runSyntheticQualityPair(ctx context.Context, ffmpegPath, filter, candidate, reference string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-nostdin", "-hide_banner", "-v", "error", "-noauto_conversion_filters",
		"-f", "lavfi", "-i", candidate,
		"-f", "lavfi", "-i", reference,
		"-filter_complex", filter, "-f", "null", "-")
	return cmd.CombinedOutput()
}

func knownQualityUnsupported(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "problem during vmaf_use_features_from_model") ||
		strings.Contains(lower, "could not initialize feature extractor") ||
		strings.Contains(lower, "model not found") ||
		strings.Contains(lower, "problem loading model") ||
		strings.Contains(lower, "feature extractor not found") ||
		strings.Contains(lower, "option not found")
}
