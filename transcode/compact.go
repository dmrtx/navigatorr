package transcode

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/jakenesler/navigatorr/mediainspect"
)

// Compact mode turns HDR, Dolby Vision and oversized SDR sources into a much
// smaller SDR BT.709 file no taller than a frozen maximum height. The video
// transform below is deterministic: the worker applies the same chain to the
// encoder input and to the metric reference, so VMAF/CAMBI measure only the
// encode, never the intended tone map or downscale.
const CompactChainVersion = "compact-sdr-v1"

// CompactHDRChainVersion keeps an HDR source HDR: the picture is only
// rescaled and re-encoded as x265 Main10 with the source's transfer (PQ or
// HLG), BT.2020 and its static HDR10 metadata. A Dolby Vision 7/8 source is
// encoded from its base layer and an HDR10+ source loses its dynamic
// metadata, so both come out as HDR10 (or HLG). The tone map of the SDR chain
// is still frozen: the worker measures VMAF/CAMBI with both the reference and
// the candidate tone mapped by it, so the SDR thresholds keep their meaning.
const CompactHDRChainVersion = "compact-hdr-v1"

// Tone map methods, by source signal. Changing the curve, LUT size or
// interpolation requires a new identifier.
const (
	CompactToneMapPQ  = "pq-bt2390-eetf-maxrgb-lut33-v1"
	CompactToneMapHLG = "hlg-ootf1000-bt2390-eetf-maxrgb-lut33-v1"
)

// CompactTargetPeakNits is the SDR display peak the tone map maps onto.
const CompactTargetPeakNits = 100

// CompactHLGDisplayPeakNits is the nominal HLG display the OOTF renders for.
const CompactHLGDisplayPeakNits = 1000

// CompactDefaultMaxHeight is the default output height limit (1080p).
const CompactDefaultMaxHeight = 1080

// CompactMaxHeights are the selectable output height limits. The output fits
// inside a 16:9 box of that height without upscaling.
var CompactMaxHeights = []int{720, 1080, 1440, 2160}

// HDR signalling written by every keep-HDR compact encode, besides the
// transfer of CompactHDROutput.
const (
	CompactHDRColorPrimaries = "bt2020"
	CompactHDRColorMatrix    = "bt2020nc"
	CompactHDRColorRange     = "tv"
	CompactHDRTransferPQ     = "smpte2084"
	CompactHDRTransferHLG    = "arib-std-b67"
)

// SDR BT.709 signalling written by every SDR compact encode.
const (
	CompactColorPrimaries = "bt709"
	CompactColorTransfer  = "bt709"
	CompactColorMatrix    = "bt709"
	CompactColorRange     = "tv"
)

// CompactPlan freezes the video transform of one compact encode. It is part
// of the plan and benchmark request digests and is re-derived from the
// source by the worker, which requires an exact match.
type CompactPlan struct {
	ChainVersion string `json:"chain_version" yaml:"chain_version"`
	// SourceClass is the mediainspect dynamic range class of the source.
	SourceClass string `json:"source_class" yaml:"source_class"`
	// SourceSignal is the transfer of the decoded picture: sdr, pq or hlg.
	// A Dolby Vision source is decoded from its base layer; the RPU and any
	// enhancement layer are not applied and are not carried to the output.
	SourceSignal       string          `json:"source_signal" yaml:"source_signal"`
	DolbyVisionProfile int             `json:"dolby_vision_profile,omitempty" yaml:"dolby_vision_profile,omitempty"`
	SourceWidth        int             `json:"source_width" yaml:"source_width"`
	SourceHeight       int             `json:"source_height" yaml:"source_height"`
	MaxHeight          int             `json:"max_height" yaml:"max_height"`
	TargetWidth        int             `json:"target_width" yaml:"target_width"`
	TargetHeight       int             `json:"target_height" yaml:"target_height"`
	ToneMap            *CompactToneMap `json:"tone_map,omitempty" yaml:"tone_map,omitempty"`
	// HDR is the output signalling of a keep-HDR plan (CompactHDRChainVersion)
	// and is absent from every SDR plan.
	HDR *CompactHDROutput `json:"hdr,omitempty" yaml:"hdr,omitempty"`
}

// CompactHDROutput freezes what a keep-HDR encode signals: the source's
// transfer and, for PQ, its static metadata as x265 writes them. HLG carries
// no static metadata.
type CompactHDROutput struct {
	Transfer string `json:"transfer" yaml:"transfer"`
	// MasterDisplay is the source's SMPTE ST 2086 metadata in x265's
	// master-display notation; empty when the source has none.
	MasterDisplay string `json:"master_display,omitempty" yaml:"master_display,omitempty"`
	// MaxCLL and MaxFALL are the source's CTA-861.3 content light levels in
	// cd/m², both zero when the source has none.
	MaxCLL  int `json:"max_cll,omitempty" yaml:"max_cll,omitempty"`
	MaxFALL int `json:"max_fall,omitempty" yaml:"max_fall,omitempty"`
}

// CompactToneMap parameterizes the HDR to SDR tone map.
type CompactToneMap struct {
	Method         string `json:"method" yaml:"method"`
	SourcePeakNits int    `json:"source_peak_nits" yaml:"source_peak_nits"`
	TargetPeakNits int    `json:"target_peak_nits" yaml:"target_peak_nits"`
}

// CompactChainEvidence records the exact transform applied to the encoder
// input and, identically, to the metric reference.
type CompactChainEvidence struct {
	ChainVersion   string `json:"chain_version"`
	SourceSignal   string `json:"source_signal"`
	ToneMapMethod  string `json:"tone_map_method,omitempty"`
	SourcePeakNits int    `json:"source_peak_nits,omitempty"`
	TargetPeakNits int    `json:"target_peak_nits,omitempty"`
	LUTSHA256      string `json:"lut_sha256,omitempty"`
	TargetWidth    int    `json:"target_width"`
	TargetHeight   int    `json:"target_height"`
	Scaler         string `json:"scaler"`
	ReferenceChain string `json:"reference_chain"`
	// CandidateChain is the tone map a keep-HDR candidate is measured
	// through; SDR candidates are measured as encoded.
	CandidateChain   string `json:"candidate_chain,omitempty"`
	MeasurementSpace string `json:"measurement_space"`
	AppliedTo        string `json:"applied_to"`
}

// CompactOutputEvidence records the post-encode signalling check: clean SDR
// BT.709, or for a keep-HDR plan exactly the frozen HDR signalling.
type CompactOutputEvidence struct {
	Verdict        string   `json:"verdict"`
	ReasonCodes    []string `json:"reason_codes,omitempty"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
	ColorPrimaries string   `json:"color_primaries,omitempty"`
	ColorTransfer  string   `json:"color_transfer,omitempty"`
	ColorMatrix    string   `json:"color_matrix,omitempty"`
	ColorRange     string   `json:"color_range,omitempty"`
	HDRSideData    []string `json:"hdr_side_data,omitempty"`
	MasterDisplay  string   `json:"master_display,omitempty"`
	MaxCLL         int      `json:"max_cll,omitempty"`
	MaxFALL        int      `json:"max_fall,omitempty"`
	FramesProbed   int      `json:"frames_probed"`
}

// ValidCompactMaxHeight reports a selectable output height limit.
func ValidCompactMaxHeight(h int) bool {
	for _, v := range CompactMaxHeights {
		if v == h {
			return true
		}
	}
	return false
}

// CompactTargetSize fits w×h inside the 16:9 box of maxHeight, preserving the
// aspect ratio with even dimensions and never upscaling.
func CompactTargetSize(w, h, maxHeight int) (int, int, error) {
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return 0, 0, errors.New("compact_source_dimensions_invalid")
	}
	if !ValidCompactMaxHeight(maxHeight) {
		return 0, 0, fmt.Errorf("compact max height %d is not one of %v", maxHeight, CompactMaxHeights)
	}
	maxW := maxHeight * 16 / 9
	if w <= maxW && h <= maxHeight {
		return w, h, nil
	}
	even := func(num, den int) int {
		v := (2*num + den) / (2 * den) // round half up
		if v%2 != 0 {
			v--
		}
		return v
	}
	if w*maxHeight >= h*maxW {
		return maxW, even(h*maxW, w), nil
	}
	return even(w*maxHeight, h), maxHeight, nil
}

// compactSourcePeak picks the luminance the PQ tone map treats as the source
// peak: the mastering display peak, lowered to MaxCLL when the content is
// known to stay darker, and BT.2390's 1000 cd/m² when neither is signalled.
func compactSourcePeak(d mediainspect.DynamicRange) int {
	peak := 0
	if d.MasteringDisplay != nil {
		if p, err := mediainspect.MasteringDisplayPeakNits(d.MasteringDisplay); err == nil {
			peak = p
		}
	}
	if cll := d.ContentLightLevel; cll != nil && cll.MaxCLL >= 200 && (peak == 0 || cll.MaxCLL < peak) {
		peak = cll.MaxCLL
	}
	if peak == 0 {
		peak = 1000
	}
	return min(max(peak, 200), 10000)
}

// NewCompactPlan derives the SDR compact plan from a classified source.
// Server and worker both call it, so the frozen transform is reproducible.
func NewCompactPlan(d mediainspect.DynamicRange, v mediainspect.DetailedStream, maxHeight int) (*CompactPlan, error) {
	return NewCompactPlanForChain(CompactChainVersion, d, v, maxHeight)
}

// ValidCompactChain reports a known compact chain version.
func ValidCompactChain(chain string) bool {
	return chain == CompactChainVersion || chain == CompactHDRChainVersion
}

// NewCompactPlanForChain derives the compact plan of a chain version from a
// classified source.
func NewCompactPlanForChain(chain string, d mediainspect.DynamicRange, v mediainspect.DetailedStream, maxHeight int) (*CompactPlan, error) {
	if !ValidCompactChain(chain) {
		return nil, errors.New("unsupported compact chain")
	}
	signal := d.Signal()
	if signal == "" {
		if d.Reason != "" {
			return nil, errors.New(d.Reason)
		}
		return nil, errors.New("compact_source_unrecognized")
	}
	if chain == CompactHDRChainVersion && signal == mediainspect.SignalSDR {
		return nil, errors.New("compact_keep_hdr_requires_hdr")
	}
	switch v.SampleAspect {
	case "", "1:1", "0:1", "N/A":
	default:
		return nil, errors.New("compact_requires_square_pixels")
	}
	tw, th, err := CompactTargetSize(v.Width, v.Height, maxHeight)
	if err != nil {
		return nil, err
	}
	p := &CompactPlan{ChainVersion: chain, SourceClass: d.Class, SourceSignal: signal, SourceWidth: v.Width, SourceHeight: v.Height, MaxHeight: maxHeight, TargetWidth: tw, TargetHeight: th}
	if d.Class == mediainspect.DynamicRangeDolbyVision && d.DolbyVision != nil {
		p.DolbyVisionProfile = d.DolbyVision.Profile
	}
	switch signal {
	case mediainspect.SignalPQ:
		p.ToneMap = &CompactToneMap{Method: CompactToneMapPQ, SourcePeakNits: compactSourcePeak(d), TargetPeakNits: CompactTargetPeakNits}
	case mediainspect.SignalHLG:
		p.ToneMap = &CompactToneMap{Method: CompactToneMapHLG, SourcePeakNits: CompactHLGDisplayPeakNits, TargetPeakNits: CompactTargetPeakNits}
	default:
		// The SDR chain only rescales and retags as BT.709, so it must not
		// silently reinterpret wide-gamut SDR.
		if (d.Primaries != "" && d.Primaries != "bt709") || (d.Matrix != "" && d.Matrix != "bt709") {
			return nil, errors.New("compact_sdr_requires_bt709")
		}
		if tw == v.Width && th == v.Height && d.Class == mediainspect.DynamicRangeSDR {
			return nil, errors.New("compact_not_needed")
		}
	}
	if chain == CompactHDRChainVersion {
		if p.HDR, err = compactHDROutput(signal, d); err != nil {
			return nil, err
		}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// compactHDROutput freezes the HDR signalling a keep-HDR encode writes.
func compactHDROutput(signal string, d mediainspect.DynamicRange) (*CompactHDROutput, error) {
	if signal == mediainspect.SignalHLG {
		return &CompactHDROutput{Transfer: CompactHDRTransferHLG}, nil
	}
	o := &CompactHDROutput{Transfer: CompactHDRTransferPQ}
	if d.MasteringDisplay != nil {
		md, err := mediainspect.X265MasterDisplay(d.MasteringDisplay)
		if err != nil {
			return nil, errors.New("hdr_mastering_display_invalid")
		}
		o.MasterDisplay = md
	}
	if cll := d.ContentLightLevel; cll != nil && (cll.MaxCLL > 0 || cll.MaxFALL > 0) {
		o.MaxCLL, o.MaxFALL = cll.MaxCLL, cll.MaxFALL
	}
	return o, nil
}

// Validate fails closed on anything but a well-formed compact plan.
func (p *CompactPlan) Validate() error {
	if p == nil {
		return nil
	}
	if !ValidCompactChain(p.ChainVersion) || p.SourceClass == "" {
		return errors.New("unsupported compact chain")
	}
	tw, th, err := CompactTargetSize(p.SourceWidth, p.SourceHeight, p.MaxHeight)
	if err != nil {
		return err
	}
	if p.TargetWidth != tw || p.TargetHeight != th {
		return errors.New("compact target size does not match source and max height")
	}
	t := p.ToneMap
	switch p.SourceSignal {
	case mediainspect.SignalSDR:
		if t != nil {
			return errors.New("SDR compact sources are not tone mapped")
		}
		if tw == p.SourceWidth && th == p.SourceHeight && p.SourceClass == mediainspect.DynamicRangeSDR {
			return errors.New("compact_not_needed")
		}
	case mediainspect.SignalPQ:
		if t == nil || t.Method != CompactToneMapPQ || t.SourcePeakNits < 200 || t.SourcePeakNits > 10000 || t.TargetPeakNits != CompactTargetPeakNits {
			return errors.New("invalid PQ compact tone map")
		}
	case mediainspect.SignalHLG:
		if t == nil || t.Method != CompactToneMapHLG || t.SourcePeakNits != CompactHLGDisplayPeakNits || t.TargetPeakNits != CompactTargetPeakNits {
			return errors.New("invalid HLG compact tone map")
		}
	default:
		return fmt.Errorf("unsupported compact source signal %q", p.SourceSignal)
	}
	if p.DolbyVisionProfile != 0 && (p.SourceClass != mediainspect.DynamicRangeDolbyVision || p.DolbyVisionProfile != 7 && p.DolbyVisionProfile != 8) {
		return errors.New("invalid compact Dolby Vision profile")
	}
	if p.ChainVersion != CompactHDRChainVersion {
		if p.HDR != nil {
			return errors.New("SDR compact plans carry no HDR output")
		}
		return nil
	}
	return p.HDR.validate(p.SourceSignal)
}

// KeepsHDR reports a plan that keeps the source HDR.
func (p *CompactPlan) KeepsHDR() bool { return p != nil && p.ChainVersion == CompactHDRChainVersion }

// MeasurementPlan is the SDR plan whose tone map a keep-HDR encode is
// measured through: the same source, size and tone map, tone mapped instead
// of kept. An SDR plan is its own measurement plan.
func (p *CompactPlan) MeasurementPlan() *CompactPlan {
	if !p.KeepsHDR() {
		return p
	}
	m := p.Clone()
	m.ChainVersion, m.HDR = CompactChainVersion, nil
	return m
}

func (o *CompactHDROutput) validate(signal string) error {
	if o == nil {
		return errors.New("keep-HDR compact plan without HDR output")
	}
	switch signal {
	case mediainspect.SignalPQ:
		if o.Transfer != CompactHDRTransferPQ {
			return errors.New("keep-HDR PQ plan must signal PQ")
		}
	case mediainspect.SignalHLG:
		if o.Transfer != CompactHDRTransferHLG || o.MasterDisplay != "" || o.MaxCLL != 0 || o.MaxFALL != 0 {
			return errors.New("keep-HDR HLG plan must signal HLG without static metadata")
		}
	default:
		return errors.New("compact_keep_hdr_requires_hdr")
	}
	// The values go to x265 inside -x265-params, so only the exact notation
	// X265MasterDisplay writes is accepted.
	if o.MasterDisplay != "" && !compactMasterDisplayPattern.MatchString(o.MasterDisplay) {
		return errors.New("invalid keep-HDR master display")
	}
	if o.MaxCLL < 0 || o.MaxFALL < 0 || o.MaxCLL > 10000 || o.MaxFALL > 10000 {
		return errors.New("invalid keep-HDR content light level")
	}
	return nil
}

var compactMasterDisplayPattern = regexp.MustCompile(`^G\(\d{1,5},\d{1,5}\)B\(\d{1,5},\d{1,5}\)R\(\d{1,5},\d{1,5}\)WP\(\d{1,5},\d{1,5}\)L\(\d{1,9},\d{1,9}\)$`)

// Clone returns an independent copy.
func (p *CompactPlan) Clone() *CompactPlan {
	if p == nil {
		return nil
	}
	cp := *p
	if p.ToneMap != nil {
		t := *p.ToneMap
		cp.ToneMap = &t
	}
	if p.HDR != nil {
		h := *p.HDR
		cp.HDR = &h
	}
	return &cp
}

// Equal reports identical frozen transforms.
func (p *CompactPlan) Equal(o *CompactPlan) bool {
	if p == nil || o == nil {
		return p == o
	}
	a, b := *p, *o
	a.ToneMap, b.ToneMap, a.HDR, b.HDR = nil, nil, nil, nil
	if a != b || (p.ToneMap == nil) != (o.ToneMap == nil) || (p.HDR == nil) != (o.HDR == nil) {
		return false
	}
	return (p.ToneMap == nil || *p.ToneMap == *o.ToneMap) && (p.HDR == nil || *p.HDR == *o.HDR)
}

// MatchSource fails closed unless the frozen plan is exactly the plan the
// source derives today.
func (p *CompactPlan) MatchSource(d mediainspect.DynamicRange, v mediainspect.DetailedStream) error {
	if p == nil {
		return errors.New("compact plan missing")
	}
	want, err := NewCompactPlanForChain(p.ChainVersion, d, v, p.MaxHeight)
	if err != nil {
		return err
	}
	if !want.Equal(p) {
		return errors.New("compact plan does not match source")
	}
	return nil
}
