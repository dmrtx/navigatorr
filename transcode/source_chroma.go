package transcode

import (
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/transcode/quality"
)

// SourceClassUnsupportedError is the refusal of a mode source whose picture no
// measurement class covers. The worker's mode preflight returns it on the
// staged copy and the coordinator's preflight on the library file, word for
// word, so a refusal reads and classifies the same wherever it was found.
func SourceClassUnsupportedError() error {
	return fmt.Errorf("quality_source_class_unsupported: native SDR 8/10-bit 4:2:0, 4:2:2 or 4:4:4 evidence at a measurable frame rate (up to %.0f FPS) required", quality.HFRMaxFPS)
}

// SupportedSourceChroma reports a pixel format with 4:2:0 chroma at the given
// depth: what every encode writes. yuvj420p is 4:2:0 at full range: an encode
// of a full-range source keeps that range (libx265 and VideoToolbox alike), so
// its candidate reports yuvj420p, and the metric graph's format conversion
// reads both inputs by their own range.
func SupportedSourceChroma(pixFmt string, bitDepth int) bool {
	p := normPixelFormat(pixFmt)
	switch bitDepth {
	case 8:
		switch p {
		case "yuv420p", "yuvj420p", "nv12":
			return true
		}
	case 10:
		switch p {
		case "yuv420p10le", "p010le":
			return true
		}
	}
	return false
}

// MeasurableSourceChroma reports whether a source can be encoded and
// measured. Every encode writes 4:2:0 (yuv420p or p010le), and both metric
// inputs are converted to yuv420p10le before libvmaf, whose features (and
// CAMBI) read luma only; a 4:2:2 or 4:4:4 source is therefore measured
// exactly as a 4:2:0 one, and its chroma is subsampled the way every encode
// already subsamples it. The candidate itself must still be 4:2:0
// (SupportedSourceChroma).
func MeasurableSourceChroma(pixFmt string, bitDepth int) bool {
	if SupportedSourceChroma(pixFmt, bitDepth) {
		return true
	}
	p := normPixelFormat(pixFmt)
	switch bitDepth {
	case 8:
		switch p {
		case "yuv422p", "yuvj422p", "yuv444p", "yuvj444p":
			return true
		}
	case 10:
		switch p {
		case "yuv422p10le", "yuv444p10le":
			return true
		}
	}
	return false
}

// SourceChromaAdmitted is MeasurableSourceChroma for a plan: a source beyond
// 4:2:0 is admitted only when the plan pins a 4:2:0 output pixel format, since
// an encode without -pix_fmt would keep the source's chroma and its candidate
// would fail validation after the whole encode. Any other pixel format (RGB,
// 4:1:1, alpha, grey, a format the probe could not name) is never admitted.
func SourceChromaAdmitted(plan *Plan, pixFmt string, bitDepth int) bool {
	if SupportedSourceChroma(pixFmt, bitDepth) {
		return true
	}
	if plan == nil {
		return false
	}
	out := plan.PixelFormat
	return (SupportedSourceChroma(out, 8) || SupportedSourceChroma(out, 10)) && MeasurableSourceChroma(pixFmt, bitDepth)
}

func normPixelFormat(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
