package mediainspect

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Dynamic range classes. Only HDR10 (and Dolby Vision whose base layer is
// HDR10, after explicit per-file approval) can be converted; every other HDR
// class is preserved untouched.
const (
	DynamicRangeSDR         = "sdr"
	DynamicRangeHDR10       = "hdr10"
	DynamicRangePQ10        = "pq10" // PQ without SMPTE ST 2086 static metadata
	DynamicRangeHLG         = "hlg"
	DynamicRangeDolbyVision = "dolby_vision"
	DynamicRangeHDR10Plus   = "hdr10plus"
	DynamicRangeUnknownHDR  = "unknown_hdr"
)

// FrameSignalSampleFrames bounds the decoded-frame evidence used to classify a
// source. HDR10+ and Dolby Vision RPUs are carried per frame, so stream-level
// ffprobe output alone cannot prove their absence.
const FrameSignalSampleFrames = 8

// FrameSignals is the decoded-frame view of a video stream's colour signalling
// and HDR side data, as a decoder (and therefore a display) receives it.
type FrameSignals struct {
	Probed            bool                       `json:"probed"`
	Frames            int                        `json:"frames"`
	ColorTransfer     string                     `json:"color_transfer,omitempty"`
	ColorPrimaries    string                     `json:"color_primaries,omitempty"`
	ColorSpace        string                     `json:"color_space,omitempty"`
	ColorRange        string                     `json:"color_range,omitempty"`
	MasteringDisplay  *MasteringDisplayMetadata  `json:"mastering_display,omitempty"`
	ContentLightLevel *ContentLightLevelMetadata `json:"content_light_level,omitempty"`
	HDR10Plus         bool                       `json:"hdr10_plus,omitempty"`
	DolbyVisionRPU    bool                       `json:"dolby_vision_rpu,omitempty"`
	DynamicMetadata   []string                   `json:"dynamic_metadata,omitempty"`
	Inconsistent      []string                   `json:"inconsistent,omitempty"`
}

// DolbyVisionInfo is the stream's DOVI decoder configuration record.
type DolbyVisionInfo struct {
	Profile           int  `json:"profile"`
	Level             int  `json:"level,omitempty"`
	BLCompatibilityID int  `json:"bl_compatibility_id"`
	RPUPresent        bool `json:"rpu_present"`
	ELPresent         bool `json:"el_present"`
	BLPresent         bool `json:"bl_present"`
	ConfigRecord      bool `json:"config_record"`
}

// DynamicRange is the fail-closed classification of a single video stream.
type DynamicRange struct {
	Class             string                     `json:"class"`
	Label             string                     `json:"label"`
	Transfer          string                     `json:"transfer,omitempty"`
	Primaries         string                     `json:"primaries,omitempty"`
	Matrix            string                     `json:"matrix,omitempty"`
	Range             string                     `json:"range,omitempty"`
	MasteringDisplay  *MasteringDisplayMetadata  `json:"mastering_display,omitempty"`
	ContentLightLevel *ContentLightLevelMetadata `json:"content_light_level,omitempty"`
	DolbyVision       *DolbyVisionInfo           `json:"dolby_vision,omitempty"`
	// BaseLayer classifies what a non-Dolby-Vision decoder shows for a Dolby
	// Vision stream: hdr10, sdr, hlg or none (profile 5).
	BaseLayer    string `json:"base_layer,omitempty"`
	HDR10Plus    bool   `json:"hdr10_plus,omitempty"`
	FramesProbed int    `json:"frames_probed,omitempty"`
	// Reason explains, with a stable code, why the source cannot be converted.
	// Empty means HDR10 conversion is eligible (Dolby Vision still requires
	// explicit approval; see RequiresDolbyVisionRemoval).
	Reason string `json:"reason,omitempty"`
}

// IsHDR reports whether the source carries any high dynamic range signal.
func (d DynamicRange) IsHDR() bool { return d.Class != DynamicRangeSDR }

// HDR10Convertible reports whether the source can become an HDR10 candidate.
func (d DynamicRange) HDR10Convertible() bool {
	return d.Reason == "" && (d.Class == DynamicRangeHDR10 || d.Class == DynamicRangeDolbyVision && d.BaseLayer == DynamicRangeHDR10)
}

// RequiresDolbyVisionRemoval reports whether converting the source discards
// Dolby Vision dynamic metadata (and any enhancement layer), keeping HDR10.
func (d DynamicRange) RequiresDolbyVisionRemoval() bool {
	return d.Class == DynamicRangeDolbyVision && d.HDR10Convertible()
}

// ProbeFrameSignals decodes the first frames of the first video stream with a
// fixed argv. The file path is the only variable element.
func ProbeFrameSignals(ctx context.Context, ffprobePath, path string, frames int) (FrameSignals, error) {
	if frames <= 0 || frames > 120 {
		frames = FrameSignalSampleFrames
	}
	if ffprobePath == "" {
		ffprobePath, _ = exec.LookPath("ffprobe")
	}
	if ffprobePath == "" {
		return FrameSignals{}, fmt.Errorf("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobePath, "-v", "error", "-select_streams", "v:0",
		"-read_intervals", fmt.Sprintf("%%+#%d", frames), "-show_frames",
		"-show_entries", "frame=color_transfer,color_primaries,color_space,color_range:frame_side_data_list",
		"-of", "json", path)
	out, err := cmd.Output()
	if err != nil {
		return FrameSignals{}, fmt.Errorf("ffprobe frame signals: %w", err)
	}
	return ParseFrameSignals(out)
}

// ParseFrameSignals parses `ffprobe -show_frames -of json` output.
func ParseFrameSignals(data []byte) (FrameSignals, error) {
	if len(data) > 8<<20 {
		return FrameSignals{}, fmt.Errorf("frame signal evidence oversized")
	}
	var probe struct {
		Frames []struct {
			ColorTransfer  string            `json:"color_transfer"`
			ColorPrimaries string            `json:"color_primaries"`
			ColorSpace     string            `json:"color_space"`
			ColorRange     string            `json:"color_range"`
			SideDataList   []json.RawMessage `json:"side_data_list"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return FrameSignals{}, fmt.Errorf("parse frame signals: %w", err)
	}
	if len(probe.Frames) == 0 {
		return FrameSignals{}, fmt.Errorf("no decoded video frames")
	}
	fs := FrameSignals{Probed: true, Frames: len(probe.Frames)}
	merge := func(name string, dst *string, v string) {
		v = normalizeColorValue(v)
		if v == "" {
			return
		}
		if *dst == "" {
			*dst = v
		} else if *dst != v {
			fs.Inconsistent = appendUnique(fs.Inconsistent, name)
		}
	}
	for _, f := range probe.Frames {
		merge("color_transfer", &fs.ColorTransfer, f.ColorTransfer)
		merge("color_primaries", &fs.ColorPrimaries, f.ColorPrimaries)
		merge("color_space", &fs.ColorSpace, f.ColorSpace)
		merge("color_range", &fs.ColorRange, f.ColorRange)
		for _, raw := range f.SideDataList {
			var sd map[string]any
			if json.Unmarshal(raw, &sd) != nil {
				continue
			}
			typ, _ := sd["side_data_type"].(string)
			lt := strings.ToLower(typ)
			switch {
			case lt == "mastering display metadata":
				md := parseMasteringDisplayMetadata(sd)
				if fs.MasteringDisplay == nil {
					fs.MasteringDisplay = md
				} else if md == nil || *md != *fs.MasteringDisplay {
					fs.Inconsistent = appendUnique(fs.Inconsistent, "mastering_display")
				}
			case lt == "content light level metadata":
				cll := parseContentLightLevelMetadata(sd)
				if fs.ContentLightLevel == nil {
					fs.ContentLightLevel = cll
				} else if cll == nil || *cll != *fs.ContentLightLevel {
					fs.Inconsistent = appendUnique(fs.Inconsistent, "content_light_level")
				}
			case strings.Contains(lt, "2094-40") || strings.Contains(lt, "hdr10+"):
				fs.HDR10Plus = true
			case strings.Contains(lt, "dolby vision") || strings.Contains(lt, "dovi"):
				fs.DolbyVisionRPU = true
			case strings.Contains(lt, "dynamic metadata") || strings.Contains(lt, "dynamic hdr"):
				fs.DynamicMetadata = appendUnique(fs.DynamicMetadata, typ)
			}
		}
	}
	return fs, nil
}

// ClassifyDynamicRange combines stream-level and decoded-frame evidence. It
// fails closed: HDR signals without frame evidence, inconsistent metadata or
// unknown dynamic metadata never classify as convertible HDR10.
func ClassifyDynamicRange(rep *DetailedReport, frames *FrameSignals) DynamicRange {
	d := DynamicRange{Class: DynamicRangeUnknownHDR}
	if rep == nil || len(rep.Video) != 1 {
		d.Reason = "hdr_single_video_stream_required"
		return d.labelled()
	}
	v := rep.Video[0]
	streamHDR := IsHDRorDolbyVisionReport(rep)
	if frames != nil && frames.Probed {
		d.FramesProbed = frames.Frames
	}
	inconsistent := false
	choose := func(stream, frame string) string {
		stream, frame = normalizeColorValue(stream), normalizeColorValue(frame)
		switch {
		case frame == "":
			return stream
		case stream == "":
			return frame
		case stream != frame:
			inconsistent = true
		}
		return frame
	}
	var fTransfer, fPrimaries, fMatrix, fRange string
	if frames != nil && frames.Probed {
		fTransfer, fPrimaries, fMatrix, fRange = frames.ColorTransfer, frames.ColorPrimaries, frames.ColorSpace, frames.ColorRange
		if len(frames.Inconsistent) > 0 {
			inconsistent = true
		}
	}
	d.Transfer = choose(v.ColorTransfer, fTransfer)
	d.Primaries = choose(v.ColorPrimaries, fPrimaries)
	d.Matrix = choose(v.ColorSpace, fMatrix)
	d.Range = choose(v.ColorRange, fRange)
	d.MasteringDisplay = v.MasteringDisplay
	d.ContentLightLevel = v.ContentLightLevel
	if frames != nil && frames.Probed {
		if frames.MasteringDisplay != nil {
			if d.MasteringDisplay != nil && !sameMasteringDisplay(*d.MasteringDisplay, *frames.MasteringDisplay) {
				inconsistent = true
			}
			d.MasteringDisplay = frames.MasteringDisplay
		}
		if frames.ContentLightLevel != nil {
			if d.ContentLightLevel != nil && *d.ContentLightLevel != *frames.ContentLightLevel {
				inconsistent = true
			}
			d.ContentLightLevel = frames.ContentLightLevel
		}
		d.HDR10Plus = frames.HDR10Plus
	}
	d.DolbyVision = dolbyVisionInfo(v, frames)

	// Stream-level SDR without HDR signals keeps the established behaviour:
	// SDR classification never requires a frame probe.
	if !streamHDR && d.DolbyVision == nil && !d.HDR10Plus && (frames == nil || !frames.Probed || (!frameHDRSignals(frames) && d.Transfer != "smpte2084" && d.Transfer != "arib-std-b67")) {
		d.Class, d.Reason = DynamicRangeSDR, ""
		return d.labelled()
	}
	if frames == nil || !frames.Probed {
		d.Reason = "hdr_frame_evidence_missing"
		return d.labelled()
	}
	switch {
	case d.DolbyVision != nil:
		d.Class = DynamicRangeDolbyVision
	case d.HDR10Plus:
		d.Class = DynamicRangeHDR10Plus
	case len(frames.DynamicMetadata) > 0:
		d.Class = DynamicRangeUnknownHDR
		d.Reason = "hdr_dynamic_metadata_unsupported"
		return d.labelled()
	case d.Transfer == "arib-std-b67":
		d.Class = DynamicRangeHLG
	case d.Transfer == "smpte2084":
		d.Class = DynamicRangePQ10
		if d.Primaries == "bt2020" && d.MasteringDisplay != nil {
			d.Class = DynamicRangeHDR10
		}
	default:
		d.Class = DynamicRangeUnknownHDR
	}
	base := baseLayerClass(d)
	switch d.Class {
	case DynamicRangeHDR10Plus:
		d.Reason = "hdr10plus_dynamic_metadata_not_preserved"
	case DynamicRangeHLG:
		d.Reason = "hlg_not_supported"
	case DynamicRangePQ10:
		d.Reason = "hdr10_static_metadata_missing"
	case DynamicRangeUnknownHDR:
		d.Reason = "hdr_signal_unrecognized"
	case DynamicRangeDolbyVision:
		d.BaseLayer = dolbyVisionBaseLayer(d.DolbyVision, base)
		d.Reason = dolbyVisionReason(d)
	}
	if d.Reason == "" && inconsistent {
		d.Reason = "hdr_metadata_inconsistent"
	}
	if d.Reason == "" {
		d.Reason = hdr10ShapeReason(v, d)
	}
	return d.labelled()
}

func frameHDRSignals(f *FrameSignals) bool {
	return f.MasteringDisplay != nil || f.ContentLightLevel != nil || f.HDR10Plus || f.DolbyVisionRPU || len(f.DynamicMetadata) > 0
}

// baseLayerClass classifies the decodable base signal ignoring Dolby Vision.
func baseLayerClass(d DynamicRange) string {
	switch {
	case d.Transfer == "smpte2084" && d.Primaries == "bt2020" && d.MasteringDisplay != nil:
		return DynamicRangeHDR10
	case d.Transfer == "smpte2084":
		return DynamicRangePQ10
	case d.Transfer == "arib-std-b67":
		return DynamicRangeHLG
	case d.Transfer == "bt709" || d.Transfer == "smpte170m" || d.Transfer == "bt470bg" || d.Transfer == "":
		if d.Primaries == "" || d.Primaries == "bt709" || d.Primaries == "smpte170m" || d.Primaries == "bt470bg" {
			return DynamicRangeSDR
		}
	}
	return DynamicRangeUnknownHDR
}

// dolbyVisionBaseLayer maps the DOVI signal compatibility id to the base layer
// a non-Dolby-Vision decoder presents. Profile 7 (compatibility 6) and profile
// 8.1 (compatibility 1) carry an HDR10 base layer; 8.2 is SDR, 8.4 is HLG and
// profile 5 has no compatible base layer at all.
func dolbyVisionBaseLayer(dv *DolbyVisionInfo, observed string) string {
	if dv == nil || !dv.ConfigRecord {
		return "unknown"
	}
	switch {
	case dv.Profile == 5:
		return "none"
	case dv.Profile == 7 && dv.BLCompatibilityID == 6, dv.Profile == 8 && dv.BLCompatibilityID == 1:
		if observed == DynamicRangeHDR10 {
			return DynamicRangeHDR10
		}
		return observed
	case dv.Profile == 8 && dv.BLCompatibilityID == 2:
		return DynamicRangeSDR
	case dv.Profile == 8 && dv.BLCompatibilityID == 4:
		return DynamicRangeHLG
	}
	return "unknown"
}

func dolbyVisionReason(d DynamicRange) string {
	dv := d.DolbyVision
	switch {
	case dv == nil || !dv.ConfigRecord:
		return "dolby_vision_configuration_missing"
	case dv.Profile == 5:
		return "dolby_vision_profile_5_no_compatible_base_layer"
	case dv.Profile != 7 && dv.Profile != 8:
		return fmt.Sprintf("dolby_vision_profile_%d_unsupported", dv.Profile)
	case d.HDR10Plus:
		return "hdr10plus_dynamic_metadata_not_preserved"
	case !dv.BLPresent:
		return "dolby_vision_base_layer_missing"
	case d.BaseLayer == DynamicRangeSDR:
		return "dolby_vision_sdr_base_layer_unsupported"
	case d.BaseLayer == DynamicRangeHLG:
		return "dolby_vision_hlg_base_layer_unsupported"
	case d.BaseLayer != DynamicRangeHDR10:
		return "dolby_vision_base_layer_not_hdr10"
	}
	return ""
}

// hdr10ShapeReason enforces the encode/measurement envelope for HDR10: native
// 10-bit limited-range 4:2:0 BT.2020 non-constant-luminance.
func hdr10ShapeReason(v DetailedStream, d DynamicRange) string {
	pf := strings.ToLower(strings.TrimSpace(v.PixelFormat))
	switch {
	case v.BitDepth != 10 || (pf != "yuv420p10le" && pf != "p010le"):
		return "hdr10_requires_10bit_420"
	case d.Range != "" && d.Range != "tv":
		return "hdr10_requires_limited_range"
	case d.Matrix != "" && d.Matrix != "bt2020nc":
		return "hdr10_requires_bt2020nc_matrix"
	}
	if _, err := X265MasterDisplay(d.MasteringDisplay); err != nil {
		return "hdr10_mastering_display_invalid"
	}
	if d.ContentLightLevel != nil && (d.ContentLightLevel.MaxCLL > 10000 || d.ContentLightLevel.MaxFALL > 10000 || d.ContentLightLevel.MaxFALL > d.ContentLightLevel.MaxCLL && d.ContentLightLevel.MaxCLL > 0) {
		return "hdr10_content_light_level_invalid"
	}
	return ""
}

func dolbyVisionInfo(v DetailedStream, frames *FrameSignals) *DolbyVisionInfo {
	var dv *DolbyVisionInfo
	for _, sd := range v.SideData {
		lt := strings.ToLower(sd.SideDataType)
		if !strings.Contains(lt, "dovi") && !strings.Contains(lt, "dolby vision") {
			continue
		}
		if dv == nil {
			dv = &DolbyVisionInfo{}
		}
		if strings.Contains(lt, "configuration") {
			dv.ConfigRecord = true
			dv.Profile = sideDataInt(sd.Data, "dv_profile")
			dv.Level = sideDataInt(sd.Data, "dv_level")
			dv.BLCompatibilityID = sideDataInt(sd.Data, "dv_bl_signal_compatibility_id")
			dv.RPUPresent = sideDataInt(sd.Data, "rpu_present_flag") == 1
			dv.ELPresent = sideDataInt(sd.Data, "el_present_flag") == 1
			dv.BLPresent = sideDataInt(sd.Data, "bl_present_flag") == 1
		}
	}
	codec := strings.ToLower(v.Codec + " " + v.Profile + " " + v.Tags["codec_tag_string"])
	signalled := strings.Contains(codec, "dvh1") || strings.Contains(codec, "dvhe") || strings.Contains(codec, "dolby vision") || strings.Contains(codec, "dovi")
	for k, val := range v.Tags {
		kl, vl := strings.ToLower(k), strings.ToLower(val)
		if strings.Contains(kl, "dovi") || strings.Contains(kl, "dolby") || strings.Contains(vl, "dovi") || strings.Contains(vl, "dolby vision") || strings.Contains(vl, "dvh1") || strings.Contains(vl, "dvhe") {
			signalled = true
		}
	}
	if frames != nil && frames.DolbyVisionRPU {
		signalled = true
	}
	if dv == nil && signalled {
		dv = &DolbyVisionInfo{}
	}
	return dv
}

func sideDataInt(data map[string]any, key string) int {
	switch v := data[key].(type) {
	case float64:
		if v >= 0 && v <= 1<<20 && v == math.Trunc(v) {
			return int(v)
		}
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return -1
}

func sameMasteringDisplay(a, b MasteringDisplayMetadata) bool {
	x, errA := X265MasterDisplay(&a)
	y, errB := X265MasterDisplay(&b)
	if errA != nil || errB != nil {
		return a == b
	}
	return x == y
}

func normalizeColorValue(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "unknown", "unspecified", "reserved", "n/a":
		return ""
	case "arib_std_b67", "hlg":
		return "arib-std-b67"
	case "pq":
		return "smpte2084"
	}
	return v
}

func (d DynamicRange) labelled() DynamicRange {
	switch d.Class {
	case DynamicRangeSDR:
		d.Label = "SDR"
	case DynamicRangeHDR10:
		d.Label = "HDR10"
	case DynamicRangePQ10:
		d.Label = "PQ HDR (no mastering metadata)"
	case DynamicRangeHLG:
		d.Label = "HLG"
	case DynamicRangeHDR10Plus:
		d.Label = "HDR10+"
	case DynamicRangeDolbyVision:
		d.Label = "Dolby Vision"
		if d.DolbyVision != nil && d.DolbyVision.ConfigRecord {
			d.Label = fmt.Sprintf("Dolby Vision profile %d", d.DolbyVision.Profile)
			if d.DolbyVision.Profile == 8 && d.DolbyVision.BLCompatibilityID > 0 {
				d.Label = fmt.Sprintf("Dolby Vision profile 8.%d", d.DolbyVision.BLCompatibilityID)
			}
			switch d.BaseLayer {
			case DynamicRangeHDR10:
				d.Label += " (HDR10 base layer)"
			case "none":
				d.Label += " (no compatible base layer)"
			case DynamicRangeSDR:
				d.Label += " (SDR base layer)"
			case DynamicRangeHLG:
				d.Label += " (HLG base layer)"
			}
		}
	default:
		d.Label = "Unrecognized HDR"
	}
	return d
}

// X265MasterDisplay converts SMPTE ST 2086 metadata into the x265
// master-display string: chromaticities in 0.00002 units and luminance in
// 0.0001 cd/m², ordered G, B, R, WP, L(max,min). Values are validated against
// the ranges HEVC can signal and that a plausible HDR10 master uses.
func X265MasterDisplay(md *MasteringDisplayMetadata) (string, error) {
	if md == nil {
		return "", fmt.Errorf("mastering display metadata missing")
	}
	chroma := func(name, raw string) (int64, error) {
		r, ok := parseRational(raw)
		if !ok {
			return 0, fmt.Errorf("mastering display %s unreadable", name)
		}
		v := roundRat(new(big.Rat).Mul(r, big.NewRat(50000, 1)))
		if v < 0 || v > 50000 {
			return 0, fmt.Errorf("mastering display %s out of range", name)
		}
		return v, nil
	}
	var p [8]int64
	for i, f := range []struct{ name, raw string }{{"green_x", md.GreenX}, {"green_y", md.GreenY}, {"blue_x", md.BlueX}, {"blue_y", md.BlueY}, {"red_x", md.RedX}, {"red_y", md.RedY}, {"white_point_x", md.WhitePointX}, {"white_point_y", md.WhitePointY}} {
		v, err := chroma(f.name, f.raw)
		if err != nil {
			return "", err
		}
		p[i] = v
	}
	maxR, ok1 := parseRational(md.MaxLuminance)
	minR, ok2 := parseRational(md.MinLuminance)
	if !ok1 || !ok2 {
		return "", fmt.Errorf("mastering display luminance unreadable")
	}
	maxL := roundRat(new(big.Rat).Mul(maxR, big.NewRat(10000, 1)))
	minL := roundRat(new(big.Rat).Mul(minR, big.NewRat(10000, 1)))
	if maxL < 50*10000 || maxL > 10000*10000 || minL < 0 || minL >= maxL {
		return "", fmt.Errorf("mastering display luminance out of range")
	}
	return fmt.Sprintf("G(%d,%d)B(%d,%d)R(%d,%d)WP(%d,%d)L(%d,%d)", p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7], maxL, minL), nil
}

// MasteringDisplayPeakNits returns the mastering display maximum luminance.
func MasteringDisplayPeakNits(md *MasteringDisplayMetadata) (int, error) {
	if _, err := X265MasterDisplay(md); err != nil {
		return 0, err
	}
	r, _ := parseRational(md.MaxLuminance)
	return int(roundRat(r)), nil
}

func parseRational(raw string) (*big.Rat, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 64 {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok || r.Sign() < 0 {
		return nil, false
	}
	return r, true
}

func roundRat(r *big.Rat) int64 {
	f, _ := r.Float64()
	return int64(math.Round(f))
}

func appendUnique(list []string, v string) []string {
	for _, s := range list {
		if s == v {
			return list
		}
	}
	return append(list, v)
}

// HasDolbyVision reports any stream-level Dolby Vision signalling: a DOVI
// configuration record, side data, codec tag or stream tag.
func HasDolbyVision(v DetailedStream) bool {
	return dolbyVisionInfo(v, nil) != nil
}
