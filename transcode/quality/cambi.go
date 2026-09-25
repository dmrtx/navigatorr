package quality

import (
	"encoding/json"
	"fmt"
	"math"
)

// CAMBIStats records additional banding relative to the source. Higher is worse.
type CAMBIStats struct {
	Mode             string  `json:"mode"`
	OutputMetric     string  `json:"output_metric"`
	LibvmafVersion   string  `json:"libvmaf_version,omitempty"`
	Mean             float64 `json:"mean"`
	Max              float64 `json:"max"`
	FramesTotal      int     `json:"frames_total"`
	WorstSampleIndex int     `json:"worst_sample_index"`
	WorstFrame       int     `json:"worst_frame"`
	WorstSourceSec   float64 `json:"worst_source_sec"`
	LocationTiming   string  `json:"location_timing"`
}

// ParseCAMBIFullReference accepts only a metric name proved by the worker's
// executable full-reference probe. Packaging may expose either the dedicated
// name or a normalized `cambi` output, so callers must not guess the name.
func ParseCAMBIFullReference(data []byte, maxBytes int, outputMetric string, sampleIndex int, sourceStartSec, fps float64) (CAMBIStats, error) {
	var out CAMBIStats
	if outputMetric != "cambi_full_reference" && outputMetric != "cambi" {
		return out, fmt.Errorf("unproven full-reference CAMBI output metric %q", outputMetric)
	}
	if len(data) == 0 || maxBytes <= 0 || len(data) > maxBytes {
		return out, fmt.Errorf("empty or oversized CAMBI JSON log")
	}
	if !finite(sourceStartSec) || sourceStartSec < 0 || !finite(fps) || fps <= 0 {
		return out, fmt.Errorf("invalid CAMBI sample timing")
	}
	var log metricLog
	if err := json.Unmarshal(data, &log); err != nil {
		return out, fmt.Errorf("malformed CAMBI JSON: %w", err)
	}
	if len(log.Frames) == 0 {
		return out, fmt.Errorf("CAMBI per-frame evidence missing")
	}
	sum := 0.0
	for i, frame := range log.Frames {
		if frame.FrameNum == nil || *frame.FrameNum != i {
			return CAMBIStats{}, fmt.Errorf("CAMBI frame numbers are not contiguous from zero at index %d", i)
		}
		score, ok := frame.Metrics[outputMetric]
		if !ok || !finite(score) || score < 0 {
			return CAMBIStats{}, fmt.Errorf("missing or invalid full-reference CAMBI at frame %d", i)
		}
		sum += score
		if i == 0 || score > out.Max {
			out.Max, out.WorstFrame = score, i
		}
	}
	out.Mean = sum / float64(len(log.Frames))
	if pm, ok := log.PooledMetrics[outputMetric]; ok && pm.Mean != nil {
		if !finite(*pm.Mean) || math.Abs(*pm.Mean-out.Mean) > 0.01 {
			return CAMBIStats{}, fmt.Errorf("pooled CAMBI mean is inconsistent with frame evidence")
		}
	}
	out.Mode = "full_ref"
	out.OutputMetric = outputMetric
	out.LibvmafVersion = log.Version
	out.FramesTotal = len(log.Frames)
	out.WorstSampleIndex = sampleIndex
	out.WorstSourceSec = sourceStartSec + float64(out.WorstFrame)/fps
	out.LocationTiming = "nominal_fps"
	return out, nil
}

func CombineCAMBI(samples []CAMBIStats) (CAMBIStats, error) {
	if len(samples) == 0 {
		return CAMBIStats{}, fmt.Errorf("no CAMBI samples")
	}
	out := samples[0]
	weightedSum, frames := 0.0, 0
	for i, sample := range samples {
		if sample.Mode != "full_ref" || sample.OutputMetric != out.OutputMetric || sample.FramesTotal <= 0 || !finite(sample.Mean) || !finite(sample.Max) {
			return CAMBIStats{}, fmt.Errorf("incompatible CAMBI sample %d", i)
		}
		weightedSum += sample.Mean * float64(sample.FramesTotal)
		frames += sample.FramesTotal
		if i == 0 || sample.Max > out.Max {
			out.Max, out.WorstFrame, out.WorstSampleIndex, out.WorstSourceSec = sample.Max, sample.WorstFrame, sample.WorstSampleIndex, sample.WorstSourceSec
		}
	}
	out.Mean, out.FramesTotal = weightedSum/float64(frames), frames
	return out, nil
}
