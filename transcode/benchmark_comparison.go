package transcode

import (
	"time"
)

// Frame pairs are extracted from the same normalized sample frame. This is a
// visual aid, not a replacement for the quality measurements or approval.
type BenchmarkComparisonFrame struct {
	SampleIndex   int      `json:"sample_index"`
	FrameIndex    int      `json:"frame_index"`
	SourceSeconds float64  `json:"source_seconds"`
	Width         int      `json:"width"`
	Height        int      `json:"height"`
	VMAF          *float64 `json:"vmaf,omitempty"`
	SSIM          *float64 `json:"ssim,omitempty"`
}
type BenchmarkComparison struct {
	ProtocolVersion int                        `json:"protocol_version"`
	CandidateID     string                     `json:"candidate_id"`
	CapturedAt      time.Time                  `json:"captured_at"`
	ExpiresAt       time.Time                  `json:"expires_at"`
	Frames          []BenchmarkComparisonFrame `json:"frames"`
}
