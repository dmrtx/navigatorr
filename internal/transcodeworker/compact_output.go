package transcodeworker

import (
	"context"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// validateCompactOutput fails closed unless the encoded candidate is SDR
// BT.709 limited range at the planned size, in both the container and the
// decoded frames, with no HDR or Dolby Vision signalling left anywhere.
func validateCompactOutput(ctx context.Context, ffprobePath, candidate string, rep *mediainspect.DetailedReport, plan *transcode.CompactPlan) *transcode.CompactOutputEvidence {
	e := &transcode.CompactOutputEvidence{Verdict: "fail"}
	video := rep.MainVideo()
	if len(video) != 1 || plan == nil {
		e.ReasonCodes = []string{"compact_output_probe_failed"}
		return e
	}
	frames, err := mediainspect.ProbeFrameSignals(ctx, ffprobePath, candidate, video[0].Index, mediainspect.FrameSignalSampleFrames)
	if err != nil || !frames.Probed {
		e.ReasonCodes = []string{"compact_output_probe_failed"}
		return e
	}
	return checkCompactOutput(video[0], frames, plan)
}

// checkCompactOutput is the pure decision of validateCompactOutput.
func checkCompactOutput(v mediainspect.DetailedStream, frames mediainspect.FrameSignals, plan *transcode.CompactPlan) *transcode.CompactOutputEvidence {
	e := &transcode.CompactOutputEvidence{
		Verdict: "fail", Width: v.Width, Height: v.Height, FramesProbed: frames.Frames,
		ColorPrimaries: v.ColorPrimaries, ColorTransfer: v.ColorTransfer, ColorMatrix: v.ColorSpace, ColorRange: v.ColorRange,
	}
	reason := func(code string) {
		for _, c := range e.ReasonCodes {
			if c == code {
				return
			}
		}
		e.ReasonCodes = append(e.ReasonCodes, code)
	}
	if v.Width != plan.TargetWidth || v.Height != plan.TargetHeight {
		reason("compact_output_size_mismatch")
	}
	want := map[string][2]string{
		"primaries": {transcode.CompactColorPrimaries, frames.ColorPrimaries},
		"transfer":  {transcode.CompactColorTransfer, frames.ColorTransfer},
		"matrix":    {transcode.CompactColorMatrix, frames.ColorSpace},
		"range":     {transcode.CompactColorRange, frames.ColorRange},
	}
	stream := map[string]string{"primaries": v.ColorPrimaries, "transfer": v.ColorTransfer, "matrix": v.ColorSpace, "range": v.ColorRange}
	for _, k := range []string{"primaries", "transfer", "matrix", "range"} {
		w := want[k]
		if norm(stream[k]) != w[0] {
			reason("compact_output_stream_not_bt709")
		}
		if f := norm(w[1]); f != "" && f != w[0] {
			reason("compact_output_frames_not_bt709")
		}
	}
	if frames.MasteringDisplay != nil || v.MasteringDisplay != nil {
		e.HDRSideData = append(e.HDRSideData, "mastering_display")
	}
	if frames.ContentLightLevel != nil || v.ContentLightLevel != nil {
		e.HDRSideData = append(e.HDRSideData, "content_light_level")
	}
	if frames.HDR10Plus {
		e.HDRSideData = append(e.HDRSideData, "hdr10_plus")
	}
	if frames.DolbyVisionRPU || mediainspect.HasDolbyVision(v) {
		e.HDRSideData = append(e.HDRSideData, "dolby_vision")
	}
	for _, m := range frames.DynamicMetadata {
		e.HDRSideData = append(e.HDRSideData, strings.ToLower(m))
	}
	// Structural signalling only: the encode clears the stream title, and a
	// stray free-text tag must never fail hours of finished work.
	if len(e.HDRSideData) > 0 || mediainspect.HasStructuralHDRSignalling(v) {
		reason("compact_output_hdr_signalling_present")
	}
	if len(e.ReasonCodes) == 0 {
		e.Verdict = "pass"
	}
	return e
}

// compactOutputError summarizes a failed output check.
func compactOutputError(e *transcode.CompactOutputEvidence) error {
	return fmt.Errorf("compact output is not clean SDR BT.709: %v", e.ReasonCodes)
}
