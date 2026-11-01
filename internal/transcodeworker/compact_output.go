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
// decoded frames, with no HDR or Dolby Vision signalling left anywhere. A
// keep-HDR candidate must instead signal exactly the frozen HDR: BT.2020,
// the source transfer, the frozen static metadata and nothing dynamic.
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
	if plan.KeepsHDR() {
		return checkCompactHDROutput(v, frames, plan)
	}
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

// checkCompactHDROutput is checkCompactOutput for a keep-HDR plan. The
// static metadata must be in the decoded frames (x265's SEI) exactly as
// frozen; a container copy, when the muxer writes one, must match too. Dolby
// Vision, HDR10+ and any other dynamic metadata must be gone.
func checkCompactHDROutput(v mediainspect.DetailedStream, frames mediainspect.FrameSignals, plan *transcode.CompactPlan) *transcode.CompactOutputEvidence {
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
	want := plan.HDR
	if v.Width != plan.TargetWidth || v.Height != plan.TargetHeight {
		reason("compact_output_size_mismatch")
	}
	if v.BitDepth != 10 {
		reason("compact_output_hdr_not_10bit")
	}
	signal := map[string][3]string{
		"primaries": {transcode.CompactHDRColorPrimaries, v.ColorPrimaries, frames.ColorPrimaries},
		"transfer":  {want.Transfer, v.ColorTransfer, frames.ColorTransfer},
		"matrix":    {transcode.CompactHDRColorMatrix, v.ColorSpace, frames.ColorSpace},
		"range":     {transcode.CompactHDRColorRange, v.ColorRange, frames.ColorRange},
	}
	for _, k := range []string{"primaries", "transfer", "matrix", "range"} {
		w := signal[k]
		if norm(w[1]) != w[0] {
			reason("compact_output_stream_not_hdr")
		}
		// Frames must say it too: a frame without the tag would be shown as
		// whatever the player assumes.
		if norm(w[2]) != w[0] {
			reason("compact_output_frames_not_hdr")
		}
	}
	if frames.MasteringDisplay != nil {
		e.MasterDisplay, _ = mediainspect.X265MasterDisplay(frames.MasteringDisplay)
		if e.MasterDisplay == "" {
			e.MasterDisplay = "unreadable"
		}
	}
	if cll := frames.ContentLightLevel; cll != nil {
		e.MaxCLL, e.MaxFALL = cll.MaxCLL, cll.MaxFALL
	}
	if e.MasterDisplay != want.MasterDisplay {
		reason("compact_output_master_display_mismatch")
	}
	if v.MasteringDisplay != nil {
		if md, err := mediainspect.X265MasterDisplay(v.MasteringDisplay); err != nil || md != want.MasterDisplay {
			reason("compact_output_master_display_mismatch")
		}
	}
	// A light level of 0,0 means "unknown" and is what an encoder writes
	// when it has none.
	cllMatches := func(c *mediainspect.ContentLightLevelMetadata) bool {
		if c == nil {
			return want.MaxCLL == 0 && want.MaxFALL == 0
		}
		return c.MaxCLL == want.MaxCLL && c.MaxFALL == want.MaxFALL
	}
	if !cllMatches(frames.ContentLightLevel) || v.ContentLightLevel != nil && !cllMatches(v.ContentLightLevel) {
		reason("compact_output_content_light_level_mismatch")
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
	if len(e.HDRSideData) > 0 {
		reason("compact_output_dynamic_hdr_present")
	}
	if len(frames.Inconsistent) > 0 {
		reason("compact_output_frames_inconsistent")
	}
	if len(e.ReasonCodes) == 0 {
		e.Verdict = "pass"
	}
	return e
}

// compactOutputError summarizes a failed output check.
func compactOutputError(e *transcode.CompactOutputEvidence) error {
	return fmt.Errorf("compact output does not carry the planned signalling: %v", e.ReasonCodes)
}
