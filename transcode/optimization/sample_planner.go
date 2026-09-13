package optimization

import (
	"fmt"
	"math"
	"sort"
)

// SamplePlanConfig specifies the parameters for planning deterministic sample windows.
type SamplePlanConfig struct {
	// Duration is the total media duration in seconds.
	Duration float64 `json:"duration"`
	// SampleSeconds is the target duration in seconds for each sample window (e.g. 60.0).
	SampleSeconds float64 `json:"sample_seconds"`
	// SampleCount is the desired number of samples (e.g. 1, 3). Defaults to 1 if <= 0 and Positions is empty.
	SampleCount int `json:"sample_count,omitempty"`
	// Positions specifies optional explicit sample centers.
	// When RelativePositions is false, values are interpreted as absolute seconds.
	// When RelativePositions is true, values are interpreted as relative fractions in [0.0, 1.0].
	Positions []float64 `json:"positions,omitempty"`
	// RelativePositions indicates whether values in Positions are relative fractions (0.0 to 1.0).
	RelativePositions bool `json:"relative_positions,omitempty"`
}

// SampleWindow describes a single temporal sample segment.
type SampleWindow struct {
	Index           int     `json:"index"`
	StartSeconds    float64 `json:"start_seconds"`
	EndSeconds      float64 `json:"end_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	CenterSeconds   float64 `json:"center_seconds"`
}

// SamplePlan contains the deterministic sample plan output.
type SamplePlan struct {
	DurationSeconds      float64        `json:"duration_seconds"`
	SampleSeconds        float64        `json:"sample_seconds"`
	RequestedSampleCount int            `json:"requested_sample_count"`
	ActualSampleCount    int            `json:"actual_sample_count"`
	TotalSampleDuration  float64        `json:"total_sample_duration"`
	CoverageRatio        float64        `json:"coverage_ratio"`
	IsFullDuration       bool           `json:"is_full_duration"`
	ReducedSampleCount   bool           `json:"reduced_sample_count"`
	Reason               string         `json:"reason,omitempty"`
	Samples              []SampleWindow `json:"samples"`
}

// PlanSamples generates deterministic sample windows adhering to duration clamping,
// overlap avoidance, short video full-duration fallback, and sample count reduction.
func PlanSamples(cfg SamplePlanConfig) (SamplePlan, error) {
	if cfg.Duration <= 0 {
		return SamplePlan{}, fmt.Errorf("%s: duration must be positive (%f)", ReasonInvalidDuration, cfg.Duration)
	}
	if cfg.SampleSeconds <= 0 {
		return SamplePlan{}, fmt.Errorf("%s: sample seconds must be positive (%f)", ReasonInvalidSampleConfig, cfg.SampleSeconds)
	}

	requestedCount := cfg.SampleCount
	if requestedCount <= 0 {
		if len(cfg.Positions) > 0 {
			requestedCount = len(cfg.Positions)
		} else {
			requestedCount = 1
		}
	}

	plan := SamplePlan{
		DurationSeconds:      round3(cfg.Duration),
		SampleSeconds:        round3(cfg.SampleSeconds),
		RequestedSampleCount: requestedCount,
		Samples:              make([]SampleWindow, 0),
	}

	// Rule: If video duration is less than or equal to sample duration, use full duration for single sample.
	if cfg.Duration <= cfg.SampleSeconds {
		plan.IsFullDuration = true
		plan.ReducedSampleCount = requestedCount > 1
		plan.ActualSampleCount = 1
		plan.TotalSampleDuration = round3(cfg.Duration)
		plan.CoverageRatio = 1.0
		plan.Reason = ReasonFullDurationSample

		plan.Samples = append(plan.Samples, SampleWindow{
			Index:           0,
			StartSeconds:    0.0,
			EndSeconds:      round3(cfg.Duration),
			DurationSeconds: round3(cfg.Duration),
			CenterSeconds:   round3(cfg.Duration / 2.0),
		})
		return plan, nil
	}

	// Video duration is greater than sample duration.
	// Determine sample centers.
	var centers []float64

	if len(cfg.Positions) > 0 {
		// Explicit positions provided
		for _, p := range cfg.Positions {
			c := p
			if cfg.RelativePositions {
				c = p * cfg.Duration
			}
			centers = append(centers, c)
		}
		sort.Float64s(centers)
	} else {
		// Calculate default centers based on SampleCount
		count := requestedCount
		// Rule: Avoid overlaps when possible.
		// Maximum non-overlapping samples of length SampleSeconds in Duration:
		maxNonOverlapping := int(math.Floor(cfg.Duration / cfg.SampleSeconds))
		if maxNonOverlapping < 1 {
			maxNonOverlapping = 1
		}

		if count > maxNonOverlapping {
			count = maxNonOverlapping
			plan.ReducedSampleCount = true
			plan.Reason = ReasonReducedSampleCount
		}

		segmentWidth := cfg.Duration / float64(count)
		for i := 0; i < count; i++ {
			c := (float64(i) + 0.5) * segmentWidth
			centers = append(centers, c)
		}
	}

	// Build sample windows from centers, clamping to duration and avoiding overlaps.
	half := cfg.SampleSeconds / 2.0
	for _, c := range centers {
		// Desired window [c - half, c + half]
		start := c - half
		end := c + half

		// Clamp window to [0, Duration] while preserving sample duration if possible
		if start < 0 {
			start = 0.0
			end = cfg.SampleSeconds
		} else if end > cfg.Duration {
			end = cfg.Duration
			start = cfg.Duration - cfg.SampleSeconds
			if start < 0 {
				start = 0.0
			}
		}

		start = round3(start)
		end = round3(end)

		// Overlap check against previous window
		if len(plan.Samples) > 0 {
			prev := plan.Samples[len(plan.Samples)-1]
			if start < prev.EndSeconds {
				// Overlaps with previous sample! Avoid overlap by skipping.
				plan.ReducedSampleCount = true
				if plan.Reason == "" {
					plan.Reason = ReasonReducedSampleCount
				}
				continue
			}
		}

		center := round3((start + end) / 2.0)
		dur := round3(end - start)

		plan.Samples = append(plan.Samples, SampleWindow{
			Index:           len(plan.Samples),
			StartSeconds:    start,
			EndSeconds:      end,
			DurationSeconds: dur,
			CenterSeconds:   center,
		})
	}

	// Edge case: if all explicit centers somehow overlapped and resulted in 0 samples (e.g. invalid inputs)
	if len(plan.Samples) == 0 {
		// Fallback to single center sample
		start := round3((cfg.Duration - cfg.SampleSeconds) / 2.0)
		if start < 0 {
			start = 0
		}
		end := round3(start + cfg.SampleSeconds)
		if end > cfg.Duration {
			end = round3(cfg.Duration)
		}
		plan.Samples = append(plan.Samples, SampleWindow{
			Index:           0,
			StartSeconds:    start,
			EndSeconds:      end,
			DurationSeconds: round3(end - start),
			CenterSeconds:   round3((start + end) / 2.0),
		})
		plan.ReducedSampleCount = true
	}

	plan.ActualSampleCount = len(plan.Samples)
	var totalDur float64
	for _, s := range plan.Samples {
		totalDur += s.DurationSeconds
	}
	plan.TotalSampleDuration = round3(totalDur)
	plan.CoverageRatio = round4(plan.TotalSampleDuration / cfg.Duration)

	return plan, nil
}

func round3(val float64) float64 {
	return math.Round(val*1000.0) / 1000.0
}

func round4(val float64) float64 {
	return math.Round(val*10000.0) / 10000.0
}
