package transcode

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// HDR10 signalling written by every HDR10 encode. Only BT.2020 non-constant
// luminance PQ with limited range is supported.
const (
	HDR10ColorPrimaries = "bt2020"
	HDR10ColorTransfer  = "smpte2084"
	HDR10ColorMatrix    = "bt2020nc"
	HDR10ColorRange     = "tv"
)

// HDR source classes a plan may convert.
const (
	HDRSourceHDR10       = "hdr10"
	HDRSourceDolbyVision = "dolby_vision"
)

// HDRToneMapMethod identifies the deterministic measurement tone map. Changing
// the curve, LUT size or interpolation requires a new identifier.
const HDRToneMapMethod = "bt2390-eetf-maxrgb-lut33-v1"

// HDRToneMapTargetNits is the SDR display peak the measurement maps onto.
const HDRToneMapTargetNits = 100

// HDR10Plan freezes the HDR10 static metadata a libx265 encode must carry and
// the measurement tone map applied identically to reference and candidate
// before VMAF/CAMBI. It is part of the plan and benchmark request digests.
type HDR10Plan struct {
	SourceClass string `json:"source_class" yaml:"source_class"`
	// MasterDisplay is the x265 master-display string
	// G(x,y)B(x,y)R(x,y)WP(x,y)L(max,min) in 0.00002 / 0.0001 cd/m² units.
	MasterDisplay string `json:"master_display" yaml:"master_display"`
	MaxCLL        int    `json:"max_cll" yaml:"max_cll"`
	MaxFALL       int    `json:"max_fall" yaml:"max_fall"`
	// DolbyVisionRemoval records the explicit per-file approval to drop Dolby
	// Vision (RPU and any enhancement layer) and keep the HDR10 base layer.
	DolbyVisionRemoval bool       `json:"dolby_vision_removal,omitempty" yaml:"dolby_vision_removal,omitempty"`
	DolbyVisionProfile int        `json:"dolby_vision_profile,omitempty" yaml:"dolby_vision_profile,omitempty"`
	ToneMap            HDRToneMap `json:"tone_map" yaml:"tone_map"`
}

// HDRToneMap parameterizes the measurement-only tone map.
type HDRToneMap struct {
	Method         string `json:"method" yaml:"method"`
	SourcePeakNits int    `json:"source_peak_nits" yaml:"source_peak_nits"`
	TargetPeakNits int    `json:"target_peak_nits" yaml:"target_peak_nits"`
}

// HDRToneMapEvidence records the exact measurement transform applied to both
// the reference and the candidate before perceptual metrics.
type HDRToneMapEvidence struct {
	Method           string `json:"method"`
	SourcePeakNits   int    `json:"source_peak_nits"`
	TargetPeakNits   int    `json:"target_peak_nits"`
	LUTSHA256        string `json:"lut_sha256"`
	MeasurementSpace string `json:"measurement_space"`
	AppliedTo        string `json:"applied_to"`
}

// HDR10OutputEvidence records the post-encode HDR10 signalling check.
type HDR10OutputEvidence struct {
	Verdict          string   `json:"verdict"`
	ReasonCodes      []string `json:"reason_codes,omitempty"`
	ColorPrimaries   string   `json:"color_primaries,omitempty"`
	ColorTransfer    string   `json:"color_transfer,omitempty"`
	ColorMatrix      string   `json:"color_matrix,omitempty"`
	ColorRange       string   `json:"color_range,omitempty"`
	MasterDisplay    string   `json:"master_display,omitempty"`
	MaxCLL           int      `json:"max_cll"`
	MaxFALL          int      `json:"max_fall"`
	DolbyVisionFound bool     `json:"dolby_vision_found"`
	HDR10PlusFound   bool     `json:"hdr10_plus_found"`
	FramesProbed     int      `json:"frames_probed"`
}

var masterDisplayPattern = regexp.MustCompile(`^G\((\d{1,5}),(\d{1,5})\)B\((\d{1,5}),(\d{1,5})\)R\((\d{1,5}),(\d{1,5})\)WP\((\d{1,5}),(\d{1,5})\)L\((\d{1,9}),(\d{1,9})\)$`)

// Validate fails closed on anything other than a well-formed HDR10 plan.
func (h *HDR10Plan) Validate() error {
	if h == nil {
		return nil
	}
	switch h.SourceClass {
	case HDRSourceHDR10:
		if h.DolbyVisionRemoval || h.DolbyVisionProfile != 0 {
			return fmt.Errorf("hdr10 source cannot carry a Dolby Vision removal approval")
		}
	case HDRSourceDolbyVision:
		if !h.DolbyVisionRemoval {
			return fmt.Errorf("dolby vision source requires explicit removal approval")
		}
		if h.DolbyVisionProfile != 7 && h.DolbyVisionProfile != 8 {
			return fmt.Errorf("dolby vision profile %d cannot be converted to HDR10", h.DolbyVisionProfile)
		}
	default:
		return fmt.Errorf("unsupported HDR source class %q", h.SourceClass)
	}
	m := masterDisplayPattern.FindStringSubmatch(h.MasterDisplay)
	if m == nil {
		return fmt.Errorf("invalid HDR10 master display %q", h.MasterDisplay)
	}
	for i := 1; i <= 8; i++ {
		if v, _ := strconv.Atoi(m[i]); v > 50000 {
			return fmt.Errorf("HDR10 master display chromaticity out of range")
		}
	}
	maxL, _ := strconv.Atoi(m[9])
	minL, _ := strconv.Atoi(m[10])
	if maxL < 50*10000 || maxL > 10000*10000 || minL >= maxL {
		return fmt.Errorf("HDR10 master display luminance out of range")
	}
	if h.MaxCLL < 0 || h.MaxCLL > 10000 || h.MaxFALL < 0 || h.MaxFALL > 10000 || h.MaxCLL > 0 && h.MaxFALL > h.MaxCLL || h.MaxCLL == 0 && h.MaxFALL != 0 {
		return fmt.Errorf("invalid HDR10 content light level %d,%d", h.MaxCLL, h.MaxFALL)
	}
	t := h.ToneMap
	if t.Method != HDRToneMapMethod || t.TargetPeakNits != HDRToneMapTargetNits || t.SourcePeakNits < 200 || t.SourcePeakNits > 10000 || t.SourcePeakNits*10000 > maxL {
		return fmt.Errorf("invalid HDR measurement tone map")
	}
	return nil
}

// X265Params returns the deterministic x265 parameter fragment for HDR10.
func (h *HDR10Plan) X265Params() string {
	return fmt.Sprintf("colorprim=%s:transfer=%s:colormatrix=%s:range=limited:hdr10=1:hdr10-opt=1:repeat-headers=1:master-display=%s:max-cll=%d,%d", HDR10ColorPrimaries, HDR10ColorTransfer, HDR10ColorMatrix, h.MasterDisplay, h.MaxCLL, h.MaxFALL)
}

// Clone returns an independent copy.
func (h *HDR10Plan) Clone() *HDR10Plan {
	if h == nil {
		return nil
	}
	cp := *h
	return &cp
}

// ErrDolbyVisionRemovalApprovalRequired is returned for a convertible Dolby
// Vision source that has no explicit per-file approval to drop Dolby Vision.
var ErrDolbyVisionRemovalApprovalRequired = errors.New("dolby_vision_removal_approval_required")

// NewHDR10Plan derives the HDR10 plan from a classified source. Server and
// worker both call it, so the frozen metadata is reproducible from the source.
// dolbyVisionRemovalApproved must come from an explicit per-file approval.
func NewHDR10Plan(d mediainspect.DynamicRange, dolbyVisionRemovalApproved bool) (*HDR10Plan, error) {
	if !d.HDR10Convertible() {
		reason := d.Reason
		if reason == "" {
			reason = "hdr10_source_not_convertible"
		}
		return nil, errors.New(reason)
	}
	md, err := mediainspect.X265MasterDisplay(d.MasteringDisplay)
	if err != nil {
		return nil, fmt.Errorf("hdr10_mastering_display_invalid: %w", err)
	}
	peak, err := mediainspect.MasteringDisplayPeakNits(d.MasteringDisplay)
	if err != nil {
		return nil, fmt.Errorf("hdr10_mastering_display_invalid: %w", err)
	}
	if peak < 200 {
		return nil, errors.New("hdr10_mastering_peak_below_tone_map_range")
	}
	plan := &HDR10Plan{SourceClass: HDRSourceHDR10, MasterDisplay: md, ToneMap: HDRToneMap{Method: HDRToneMapMethod, SourcePeakNits: peak, TargetPeakNits: HDRToneMapTargetNits}}
	if d.ContentLightLevel != nil {
		plan.MaxCLL, plan.MaxFALL = d.ContentLightLevel.MaxCLL, d.ContentLightLevel.MaxFALL
	}
	if d.RequiresDolbyVisionRemoval() {
		if !dolbyVisionRemovalApproved {
			return nil, ErrDolbyVisionRemovalApprovalRequired
		}
		plan.SourceClass = HDRSourceDolbyVision
		plan.DolbyVisionRemoval = true
		plan.DolbyVisionProfile = d.DolbyVision.Profile
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// MatchSource fails closed unless the frozen plan is exactly the plan the
// source derives today: class, static metadata, tone map and approval.
func (h *HDR10Plan) MatchSource(d mediainspect.DynamicRange) error {
	if h == nil {
		if d.IsHDR() {
			return fmt.Errorf("HDR source %s requires an HDR10 plan", d.Class)
		}
		return nil
	}
	want, err := NewHDR10Plan(d, h.DolbyVisionRemoval)
	if err != nil {
		return err
	}
	if *want != *h {
		return fmt.Errorf("HDR10 plan does not match source dynamic range metadata")
	}
	return nil
}
