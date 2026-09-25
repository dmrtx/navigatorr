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
	if result.ProbeError == "" && ctx.Err() == nil {
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
	modelFilter := fmt.Sprintf("[0:v]format=yuv420p10le,setpts=PTS-STARTPTS[d];[1:v]format=yuv420p10le,setpts=PTS-STARTPTS[r];[d][r]libvmaf=model=version=%s:log_fmt=json:log_path=%s:shortest=1:repeatlast=0", model.LibvmafModel, escapeFFmpegFilterPath(modelLog))
	if output, err := runSyntheticQualityProbe(probeCtx, ffmpegPath, modelFilter); err != nil {
		entry.Reason = "model_initialization_failed"
		out.Models[model.ID] = entry
		if !knownQualityUnsupported(string(output)) {
			out.ProbeError = "vmaf_probe_execution_failed"
		}
	} else if data, err := os.ReadFile(modelLog); err != nil {
		out.ProbeError = "vmaf_probe_log_missing"
	} else if stats, err := quality.ParseVMAF(data, MaxMetricLogSizeBytes, model, 0, 0, 24, 0.2, nil); err != nil {
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
	cambiFilter := fmt.Sprintf("[0:v]format=yuv420p10le,setpts=PTS-STARTPTS[d];[1:v]format=yuv420p10le,setpts=PTS-STARTPTS[r];[d][r]libvmaf=model=version=vmaf_v0.6.1:feature=name=cambi\\\\:full_ref=true:log_fmt=json:log_path=%s:shortest=1:repeatlast=0", escapeFFmpegFilterPath(cambiLog))
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
	return out
}

func runSyntheticQualityProbe(ctx context.Context, ffmpegPath, filter string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-nostdin", "-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=0.25",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=0.25",
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
