package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// VMAF and CAMBI are trained and calibrated on BT.709 SDR. A PQ signal fed to
// them directly is scored as if its code values were gamma-encoded SDR, which
// misweights every luminance range. HDR measurement therefore maps reference
// and candidate through the identical, deterministic transform below and
// scores the two SDR renditions:
//
//  1. Y'CbCr BT.2020-NCL limited range -> R'G'B' (still PQ-encoded), matrix only.
//  2. A 33^3 tetrahedral 3D LUT generated here: PQ EOTF, ITU-R BT.2390 EETF
//     applied to max(R,G,B) with hue-preserving ratio scaling from the frozen
//     source peak to a 100 cd/m² display, BT.2020 -> BT.709 primaries with
//     clipping, and BT.1886 inverse EOTF (gamma 2.4).
//  3. R'G'B' -> Y'CbCr BT.709 limited range 10-bit for the metric.
//
// The LUT replaces zscale+tonemap, which needs libzimg that the worker FFmpeg
// may lack, and swscale's linearization, which clips above reference white.
// The method identifier and LUT digest are recorded in evidence.
const hdrToneMapLUTSize = 33

const (
	pqM1 = 2610.0 / 16384.0
	pqM2 = 2523.0 / 4096.0 * 128.0
	pqC1 = 3424.0 / 4096.0
	pqC2 = 2413.0 / 4096.0 * 32.0
	pqC3 = 2392.0 / 4096.0 * 32.0
)

// pqEOTF maps a PQ code value in [0,1] to absolute luminance in cd/m².
func pqEOTF(e float64) float64 {
	e = math.Min(math.Max(e, 0), 1)
	p := math.Pow(e, 1/pqM2)
	return 10000 * math.Pow(math.Max(p-pqC1, 0)/(pqC2-pqC3*p), 1/pqM1)
}

// pqInverseEOTF maps absolute luminance in cd/m² to a PQ code value.
func pqInverseEOTF(l float64) float64 {
	y := math.Min(math.Max(l/10000, 0), 1)
	p := math.Pow(y, pqM1)
	return math.Pow((pqC1+pqC2*p)/(1+pqC3*p), pqM2)
}

// bt2390EETF tone maps absolute luminance l from a display mastered at
// sourcePeak onto a targetPeak display (black levels 0), per ITU-R BT.2390.
func bt2390EETF(l, sourcePeak, targetPeak float64) float64 {
	if l <= 0 {
		return 0
	}
	pb, pw := pqInverseEOTF(0), pqInverseEOTF(sourcePeak)
	e1 := (pqInverseEOTF(l) - pb) / (pw - pb)
	maxLum := (pqInverseEOTF(targetPeak) - pb) / (pw - pb)
	if maxLum >= 1 {
		return math.Min(l, sourcePeak)
	}
	e1 = math.Min(e1, 1)
	ks := 1.5*maxLum - 0.5
	e2 := e1
	if e1 >= ks {
		t := (e1 - ks) / (1 - ks)
		t2, t3 := t*t, t*t*t
		e2 = (2*t3-3*t2+1)*ks + (t3-2*t2+t)*(1-ks) + (-2*t3+3*t2)*maxLum
	}
	return pqEOTF(e2*(pw-pb) + pb)
}

// bt2020To709 converts linear BT.2020 RGB to linear BT.709 RGB (D65). Rows
// sum to one, so neutral greys stay neutral.
var bt2020To709 = [3][3]float64{
	{1.6604910021, -0.5876411388, -0.0728498633},
	{-0.1245504745, 1.1328998971, -0.0083494226},
	{-0.0181507634, -0.1005788980, 1.1187296614},
}

// hdrToneMapPixel maps one PQ BT.2020 R'G'B' triple to BT.709 gamma-2.4 R'G'B'.
func hdrToneMapPixel(r, g, b float64, sourcePeak, targetPeak float64) [3]float64 {
	lin := [3]float64{pqEOTF(r), pqEOTF(g), pqEOTF(b)}
	peak := math.Max(lin[0], math.Max(lin[1], lin[2]))
	scale := 0.0
	if peak > 0 {
		scale = bt2390EETF(peak, sourcePeak, targetPeak) / peak
	}
	for i := range lin {
		lin[i] = lin[i] * scale / targetPeak
	}
	var out [3]float64
	for i := 0; i < 3; i++ {
		v := bt2020To709[i][0]*lin[0] + bt2020To709[i][1]*lin[1] + bt2020To709[i][2]*lin[2]
		out[i] = math.Pow(math.Min(math.Max(v, 0), 1), 1/2.4)
	}
	return out
}

// buildHDRToneMapLUT renders the .cube LUT. Output is byte-for-byte
// deterministic for a given source peak.
func buildHDRToneMapLUT(sourcePeak int) ([]byte, error) {
	if sourcePeak < 200 || sourcePeak > 10000 {
		return nil, fmt.Errorf("tone map source peak %d out of range", sourcePeak)
	}
	var buf bytes.Buffer
	n := hdrToneMapLUTSize
	fmt.Fprintf(&buf, "TITLE \"%s peak %d\"\nLUT_3D_SIZE %d\nDOMAIN_MIN 0.0 0.0 0.0\nDOMAIN_MAX 1.0 1.0 1.0\n", transcode.HDRToneMapMethod, sourcePeak, n)
	step := 1 / float64(n-1)
	// .cube order: red varies fastest, then green, then blue.
	for bi := 0; bi < n; bi++ {
		for gi := 0; gi < n; gi++ {
			for ri := 0; ri < n; ri++ {
				o := hdrToneMapPixel(float64(ri)*step, float64(gi)*step, float64(bi)*step, float64(sourcePeak), transcode.HDRToneMapTargetNits)
				fmt.Fprintf(&buf, "%.6f %.6f %.6f\n", o[0], o[1], o[2])
			}
		}
	}
	return buf.Bytes(), nil
}

// hdrToneMap is a prepared measurement transform bound to one LUT file.
type hdrToneMap struct {
	lutPath  string
	evidence transcode.HDRToneMapEvidence
}

// prepareHDRToneMap writes the LUT into worker-owned scratch and returns the
// transform. A nil plan returns nil: SDR measurement is unchanged.
func prepareHDRToneMap(dir string, plan *transcode.HDR10Plan) (*hdrToneMap, error) {
	if plan == nil {
		return nil, nil
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	lut, err := buildHDRToneMapLUT(plan.ToneMap.SourcePeakNits)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(lut)
	path := filepath.Join(dir, fmt.Sprintf("hdr_tonemap_%d.cube", plan.ToneMap.SourcePeakNits))
	if err := prepareOutputFile(path); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, lut, 0o600); err != nil {
		return nil, fmt.Errorf("writing HDR tone map LUT: %w", err)
	}
	return &hdrToneMap{lutPath: path, evidence: transcode.HDRToneMapEvidence{
		Method:           plan.ToneMap.Method,
		SourcePeakNits:   plan.ToneMap.SourcePeakNits,
		TargetPeakNits:   plan.ToneMap.TargetPeakNits,
		LUTSHA256:        "sha256:" + hex.EncodeToString(sum[:]),
		MeasurementSpace: "bt709-gamma2.4-limited-10bit",
		AppliedTo:        "reference_and_candidate",
	}}, nil
}

// metricInputChain is the per-input preprocessing in VMAF/CAMBI graphs. The
// colour parameters are pinned with setparams so container tags can never
// change how either side is interpreted.
func (t *hdrToneMap) metricInputChain() string {
	if t == nil {
		return "format=yuv420p10le"
	}
	const flags = "flags=bicubic+accurate_rnd+full_chroma_int"
	return "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv," +
		"scale=in_color_matrix=bt2020:in_range=tv:out_range=pc:" + flags + ",format=gbrpf32le," +
		"lut3d=file=" + escapeFFmpegFilterPath(t.lutPath) + ":interp=tetrahedral," +
		"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=pc," +
		"scale=in_color_matrix=bt709:out_color_matrix=bt709:in_range=pc:out_range=tv:" + flags + ",format=yuv420p10le"
}

func (t *hdrToneMap) evidenceCopy() *transcode.HDRToneMapEvidence {
	if t == nil {
		return nil
	}
	e := t.evidence
	return &e
}

// classifySourceDynamicRange classifies the first video stream. Stream-level
// SDR needs no frame evidence; any HDR signal is confirmed against decoded
// frames because HDR10+ and Dolby Vision RPUs are carried per frame.
func classifySourceDynamicRange(ctx context.Context, ffprobePath, path string, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
	d := mediainspect.ClassifyDynamicRange(rep, nil)
	if !d.IsHDR() {
		return d, nil
	}
	frames, err := mediainspect.ProbeFrameSignals(ctx, ffprobePath, path, mediainspect.FrameSignalSampleFrames)
	if err != nil {
		return d, fmt.Errorf("probing HDR frame signals: %w", err)
	}
	return mediainspect.ClassifyDynamicRange(rep, &frames), nil
}

// verifySourceHDR10 fails closed unless plan is nil for an SDR source or is
// exactly the HDR10 plan the source derives (see transcode.NewHDR10Plan).
func verifySourceHDR10(ctx context.Context, ffprobePath, path string, rep *mediainspect.DetailedReport, plan *transcode.HDR10Plan) (mediainspect.DynamicRange, error) {
	d, err := classifySourceDynamicRange(ctx, ffprobePath, path, rep)
	if err != nil {
		return d, err
	}
	return d, plan.MatchSource(d)
}
