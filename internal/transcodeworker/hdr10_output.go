package transcodeworker

import (
	"context"
	"fmt"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// validateHDR10Output proves the encoded candidate is HDR10 carrying exactly
// the planned static metadata, both in the container (stream side data) and
// in the bitstream (decoded-frame SEI), with no Dolby Vision or HDR10+ left.
// Every missing or different field is a reason code; any reason fails closed.
func validateHDR10Output(ctx context.Context, ffprobePath, candidate string, rep *mediainspect.DetailedReport, plan *transcode.HDR10Plan) (*transcode.HDR10OutputEvidence, error) {
	e := &transcode.HDR10OutputEvidence{Verdict: "fail"}
	if plan == nil {
		return nil, nil
	}
	if rep == nil || len(rep.Video) != 1 {
		e.ReasonCodes = []string{"hdr10_output_single_video_stream_required"}
		return e, fmt.Errorf("hdr10_output_invalid: %v", e.ReasonCodes)
	}
	frames, err := mediainspect.ProbeFrameSignals(ctx, ffprobePath, candidate, mediainspect.FrameSignalSampleFrames)
	if err != nil {
		e.ReasonCodes = []string{"hdr10_output_frame_probe_failed"}
		return e, fmt.Errorf("hdr10_output_invalid: %w", err)
	}
	e.ReasonCodes = hdr10OutputReasons(rep.Video[0], frames, plan, e)
	if len(e.ReasonCodes) > 0 {
		return e, fmt.Errorf("hdr10_output_invalid: %v", e.ReasonCodes)
	}
	e.Verdict = "pass"
	return e, nil
}

// hdr10OutputReasons is the pure comparison behind validateHDR10Output.
func hdr10OutputReasons(v mediainspect.DetailedStream, frames mediainspect.FrameSignals, plan *transcode.HDR10Plan, e *transcode.HDR10OutputEvidence) []string {
	var reasons []string
	add := func(code string) {
		for _, c := range reasons {
			if c == code {
				return
			}
		}
		reasons = append(reasons, code)
	}
	e.FramesProbed = frames.Frames
	e.ColorPrimaries, e.ColorTransfer, e.ColorMatrix, e.ColorRange = v.ColorPrimaries, v.ColorTransfer, v.ColorSpace, v.ColorRange
	if !frames.Probed || frames.Frames == 0 {
		add("hdr10_output_frame_evidence_missing")
	}
	for _, c := range []struct{ code, stream, frame, want string }{
		{"hdr10_output_primaries_mismatch", v.ColorPrimaries, frames.ColorPrimaries, transcode.HDR10ColorPrimaries},
		{"hdr10_output_transfer_mismatch", v.ColorTransfer, frames.ColorTransfer, transcode.HDR10ColorTransfer},
		{"hdr10_output_matrix_mismatch", v.ColorSpace, frames.ColorSpace, transcode.HDR10ColorMatrix},
		{"hdr10_output_range_mismatch", v.ColorRange, frames.ColorRange, transcode.HDR10ColorRange},
	} {
		if c.stream != c.want || c.frame != c.want {
			add(c.code)
		}
	}
	if v.BitDepth != 10 {
		add("hdr10_output_bit_depth_mismatch")
	}
	for _, md := range []struct {
		code string
		meta *mediainspect.MasteringDisplayMetadata
	}{{"hdr10_output_container_mastering_display_mismatch", v.MasteringDisplay}, {"hdr10_output_bitstream_mastering_display_mismatch", frames.MasteringDisplay}} {
		got, err := mediainspect.X265MasterDisplay(md.meta)
		if err != nil || got != plan.MasterDisplay {
			add(md.code)
		}
		if err == nil && md.meta == v.MasteringDisplay {
			e.MasterDisplay = got
		}
	}
	cll := func(c *mediainspect.ContentLightLevelMetadata) (int, int) {
		if c == nil {
			return 0, 0
		}
		return c.MaxCLL, c.MaxFALL
	}
	streamCLL, streamFALL := cll(v.ContentLightLevel)
	frameCLL, frameFALL := cll(frames.ContentLightLevel)
	e.MaxCLL, e.MaxFALL = streamCLL, streamFALL
	if plan.MaxCLL > 0 && (v.ContentLightLevel == nil || frames.ContentLightLevel == nil) {
		add("hdr10_output_content_light_level_missing")
	}
	if streamCLL != plan.MaxCLL || streamFALL != plan.MaxFALL || frameCLL != plan.MaxCLL || frameFALL != plan.MaxFALL {
		add("hdr10_output_content_light_level_mismatch")
	}
	e.DolbyVisionFound = frames.DolbyVisionRPU || mediainspect.HasDolbyVision(v)
	if e.DolbyVisionFound {
		add("hdr10_output_dolby_vision_present")
	}
	e.HDR10PlusFound = frames.HDR10Plus
	if e.HDR10PlusFound || len(frames.DynamicMetadata) > 0 {
		add("hdr10_output_dynamic_metadata_present")
	}
	return reasons
}
