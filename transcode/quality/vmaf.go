package quality

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// VMAFModel describes the measurement contract, independently of the encoded bit depth.
type VMAFModel struct {
	ID                  string  `json:"id"`
	LibvmafModel        string  `json:"libvmaf_model"`
	MinScore            float64 `json:"min_score"`
	MaxScore            float64 `json:"max_score"`
	MeasurementBitDepth int     `json:"measurement_bit_depth"`
	Width               int     `json:"width"`
	Height              int     `json:"height"`
	ViewingDistance     string  `json:"viewing_distance"`
	HFR                 bool    `json:"hfr"`
}

var models = map[string]VMAFModel{
	"v1_1080p_3h": {ID: "v1_1080p_3h", LibvmafModel: "vmaf_v1.0.16_3d0h", MinScore: 0, MaxScore: 100, MeasurementBitDepth: 10, Width: 1920, Height: 1080, ViewingDistance: "3H"},
}

func Model(id string) (VMAFModel, error) {
	m, ok := models[id]
	if !ok {
		return VMAFModel{}, fmt.Errorf("unsupported VMAF model %q", id)
	}
	return m, nil
}

// VMAFStats is a bounded summary. Window times use nominal FPS, never a VFR claim.
type VMAFStats struct {
	ModelID                string   `json:"model_id"`
	LibvmafModel           string   `json:"libvmaf_model"`
	LibvmafVersion         string   `json:"libvmaf_version,omitempty"`
	MeasurementBitDepth    int      `json:"measurement_bit_depth"`
	ScoreMin               float64  `json:"score_min"`
	ScoreMax               float64  `json:"score_max"`
	Mean                   float64  `json:"mean"`
	P5                     float64  `json:"p5"`
	Min                    float64  `json:"min"`
	Max                    float64  `json:"max"`
	FramesTotal            int      `json:"frames_total"`
	FrameThreshold         *float64 `json:"frame_threshold,omitempty"`
	FramesBelowThreshold   int      `json:"frames_below_threshold,omitempty"`
	WorstWindowSeconds     float64  `json:"worst_window_seconds,omitempty"`
	WorstWindowMean        float64  `json:"worst_window_mean,omitempty"`
	WorstWindowSampleIndex int      `json:"worst_window_sample_index"`
	WorstWindowStartSec    float64  `json:"worst_window_start_sec,omitempty"`
	WorstWindowSourceSec   float64  `json:"worst_window_source_sec,omitempty"`
	WindowTiming           string   `json:"window_timing,omitempty"`
	frameScores            []float64
}

type metricLog struct {
	Version       string `json:"version"`
	PooledMetrics map[string]struct {
		Mean *float64 `json:"mean"`
	} `json:"pooled_metrics"`
	Frames []struct {
		FrameNum *int               `json:"frameNum"`
		Metrics  map[string]float64 `json:"metrics"`
	} `json:"frames"`
}

// ParseVMAF validates every frame even when a pooled score is present. The
// nearest-rank p5 is sortedScores[ceil(.05*n)-1], clamped to the first item.
func ParseVMAF(data []byte, maxBytes int, model VMAFModel, sampleIndex int, sourceStartSec, fps, windowSeconds float64, frameThreshold *float64) (VMAFStats, error) {
	var out VMAFStats
	if len(data) == 0 || len(data) > maxBytes || maxBytes <= 0 {
		return out, fmt.Errorf("empty or oversized VMAF JSON log")
	}
	if model.ID == "" || !finite(model.MinScore) || !finite(model.MaxScore) || model.MinScore >= model.MaxScore {
		return out, fmt.Errorf("invalid VMAF model bounds")
	}
	if !finite(sourceStartSec) || sourceStartSec < 0 || !finite(fps) || fps <= 0 || !finite(windowSeconds) || windowSeconds <= 0 {
		return out, fmt.Errorf("invalid sample timing")
	}
	if frameThreshold != nil && (!finite(*frameThreshold) || *frameThreshold < model.MinScore || *frameThreshold > model.MaxScore) {
		return out, fmt.Errorf("invalid VMAF frame threshold")
	}
	var log metricLog
	if err := json.Unmarshal(data, &log); err != nil {
		return out, fmt.Errorf("malformed VMAF JSON: %w", err)
	}
	if len(log.Frames) == 0 {
		return out, fmt.Errorf("VMAF per-frame evidence missing")
	}
	scores := make([]float64, len(log.Frames))
	sum := 0.0
	for i, frame := range log.Frames {
		if frame.FrameNum == nil || *frame.FrameNum != i {
			return out, fmt.Errorf("VMAF frame numbers are not contiguous from zero at index %d", i)
		}
		s, ok := frame.Metrics["vmaf"]
		if !ok || !finite(s) || s < model.MinScore || s > model.MaxScore {
			return out, fmt.Errorf("missing or out-of-bounds VMAF score at frame %d", i)
		}
		scores[i] = s
		sum += s
		if frameThreshold != nil && s < *frameThreshold {
			out.FramesBelowThreshold++
		}
	}
	mean := sum / float64(len(scores))
	if pm, ok := log.PooledMetrics["vmaf"]; ok && pm.Mean != nil {
		if !finite(*pm.Mean) || *pm.Mean < model.MinScore || *pm.Mean > model.MaxScore || math.Abs(*pm.Mean-mean) > 0.01 {
			return VMAFStats{}, fmt.Errorf("pooled VMAF mean is inconsistent with frame evidence")
		}
	}
	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	windowFrames := int(math.Round(windowSeconds * fps))
	if windowFrames < 1 {
		windowFrames = 1
	}
	if windowFrames > len(scores) {
		windowFrames = len(scores)
	}
	rolling := 0.0
	for i := 0; i < windowFrames; i++ {
		rolling += scores[i]
	}
	worst, worstStart := rolling/float64(windowFrames), 0
	for i := windowFrames; i < len(scores); i++ {
		rolling += scores[i] - scores[i-windowFrames]
		avg := rolling / float64(windowFrames)
		if avg < worst {
			worst, worstStart = avg, i-windowFrames+1
		}
	}
	p5Index := int(math.Ceil(0.05*float64(len(sorted)))) - 1
	if p5Index < 0 {
		p5Index = 0
	}
	out.ModelID = model.ID
	out.LibvmafModel = model.LibvmafModel
	out.LibvmafVersion = log.Version
	out.MeasurementBitDepth = model.MeasurementBitDepth
	out.ScoreMin, out.ScoreMax = model.MinScore, model.MaxScore
	out.Mean, out.P5, out.Min, out.Max = mean, sorted[p5Index], sorted[0], sorted[len(sorted)-1]
	out.FramesTotal = len(scores)
	out.FrameThreshold = frameThreshold
	out.WorstWindowSeconds = float64(windowFrames) / fps
	out.WorstWindowMean = worst
	out.WorstWindowSampleIndex = sampleIndex
	out.WorstWindowStartSec = float64(worstStart) / fps
	out.WorstWindowSourceSec = sourceStartSec + out.WorstWindowStartSec
	out.WindowTiming = "nominal_fps"
	out.frameScores = scores
	return out, nil
}

// CombineVMAF combines sample-local evidence without ever rolling a window
// across two unrelated source locations. It must run before JSON persistence,
// because raw frames intentionally stay worker-local and unexported.
func CombineVMAF(samples []VMAFStats) (VMAFStats, error) {
	if len(samples) == 0 {
		return VMAFStats{}, fmt.Errorf("no VMAF samples")
	}
	out := samples[0]
	out.FramesBelowThreshold = 0
	all := make([]float64, 0)
	sum := 0.0
	for i, sample := range samples {
		if sample.ModelID != out.ModelID || sample.LibvmafModel != out.LibvmafModel || sample.MeasurementBitDepth != out.MeasurementBitDepth || sample.ScoreMin != out.ScoreMin || sample.ScoreMax != out.ScoreMax || sample.FrameThreshold == nil != (out.FrameThreshold == nil) {
			return VMAFStats{}, fmt.Errorf("incompatible VMAF sample %d", i)
		}
		if len(sample.frameScores) == 0 || len(sample.frameScores) != sample.FramesTotal {
			return VMAFStats{}, fmt.Errorf("sample %d has no raw frame evidence", i)
		}
		if sample.FrameThreshold != nil && *sample.FrameThreshold != *out.FrameThreshold {
			return VMAFStats{}, fmt.Errorf("sample %d frame threshold changed", i)
		}
		for _, score := range sample.frameScores {
			all = append(all, score)
			sum += score
		}
		out.FramesBelowThreshold += sample.FramesBelowThreshold
		if i == 0 || sample.WorstWindowMean < out.WorstWindowMean {
			out.WorstWindowMean = sample.WorstWindowMean
			out.WorstWindowSeconds = sample.WorstWindowSeconds
			out.WorstWindowSampleIndex = sample.WorstWindowSampleIndex
			out.WorstWindowStartSec = sample.WorstWindowStartSec
			out.WorstWindowSourceSec = sample.WorstWindowSourceSec
		}
	}
	sort.Float64s(all)
	p5Index := int(math.Ceil(0.05*float64(len(all)))) - 1
	if p5Index < 0 {
		p5Index = 0
	}
	out.Mean, out.P5, out.Min, out.Max = sum/float64(len(all)), all[p5Index], all[0], all[len(all)-1]
	out.FramesTotal = len(all)
	out.frameScores = nil
	return out, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
