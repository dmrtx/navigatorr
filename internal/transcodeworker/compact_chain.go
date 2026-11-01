package transcodeworker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// Compact mode renders HDR, Dolby Vision (base layer) and oversized SDR
// sources as SDR BT.709 at the frozen target size. The same picture chain
// feeds the encoder and builds the metric reference, so VMAF/CAMBI measure
// only encode loss, never the intended tone map or downscale:
//
//  1. HDR: Y'CbCr BT.2020-NCL limited range -> R'G'B' (still PQ or HLG
//     encoded) with the lanczos downscale folded into the same swscale step.
//  2. HDR: a 33^3 tetrahedral 3D LUT generated here. PQ: EOTF. HLG: inverse
//     OETF plus the BT.2100 OOTF for a 1000 cd/m² display (gamma 1.2). Then
//     the ITU-R BT.2390 EETF on max(R,G,B) with hue-preserving ratio scaling
//     onto a 100 cd/m² display, BT.2020 -> BT.709 primaries with clipping,
//     and the BT.1886 inverse EOTF (gamma 2.4).
//  3. HDR: R'G'B' -> Y'CbCr BT.709 limited range 10-bit.
//  4. All: explicit BT.709 colour parameters and deletion of every HDR and
//     Dolby Vision frame side data type, so encoders cannot re-emit them.
//
// The LUT replaces zscale+tonemap, which need libzimg that the worker FFmpeg
// may lack, and swscale linearization, which clips above reference white.
//
// A keep-HDR plan (compact-hdr-v1) only rescales: its encoder input keeps
// the source's BT.2020 PQ/HLG Y'CbCr, every frame side data type above is
// still deleted, and x265 writes the frozen static metadata itself. Its
// metrics compare SDR pictures: the reference through the SDR chain of the
// same source, the candidate through the same LUT at its own size.
const compactLUTSize = 33

const (
	pqM1 = 2610.0 / 16384.0
	pqM2 = 2523.0 / 4096.0 * 128.0
	pqC1 = 3424.0 / 4096.0
	pqC2 = 2413.0 / 4096.0 * 32.0
	pqC3 = 2392.0 / 4096.0 * 32.0

	hlgA = 0.17883277
	hlgB = 1 - 4*hlgA

	hlgSystemGamma = 1.2
)

// hlgC is the BT.2100 HLG constant 0.5 - a*ln(4a) (about 0.55991073).
var hlgC = 0.5 - hlgA*math.Log(4*hlgA)

// compactScaleFlags is the swscale configuration of every compact resize.
const compactScaleFlags = "lanczos+accurate_rnd+full_chroma_int"

// compactMeasurementSpace describes the pictures VMAF/CAMBI compare.
const compactMeasurementSpace = "bt709-sdr-limited-target-size"

// compactHDRMeasurementSpace is the measurement space of a keep-HDR plan:
// reference and candidate both tone mapped by the frozen LUT.
const compactHDRMeasurementSpace = "bt709-sdr-tone-mapped-reference-and-candidate-target-size"

// compactStrippedSideData are the frame side data types removed from every
// compact frame before encoding.
var compactStrippedSideData = []string{
	"MASTERING_DISPLAY_METADATA",
	"CONTENT_LIGHT_LEVEL",
	"DYNAMIC_HDR_PLUS",
	"DYNAMIC_HDR_VIVID",
	"DOVI_RPU_BUFFER",
	"DOVI_METADATA",
	"AMBIENT_VIEWING_ENVIRONMENT",
}

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

// hlgInverseOETF maps an HLG code value in [0,1] to normalized scene light.
func hlgInverseOETF(e float64) float64 {
	e = math.Min(math.Max(e, 0), 1)
	if e <= 0.5 {
		return e * e / 3
	}
	return (math.Exp((e-hlgC)/hlgA) + hlgB) / 12
}

// hlgDisplayLight renders HLG R'G'B' for a displayPeak cd/m² display with the
// BT.2100 OOTF (zero black level).
func hlgDisplayLight(r, g, b, displayPeak float64) [3]float64 {
	s := [3]float64{hlgInverseOETF(r), hlgInverseOETF(g), hlgInverseOETF(b)}
	ys := 0.2627*s[0] + 0.6780*s[1] + 0.0593*s[2]
	gain := 0.0
	if ys > 0 {
		gain = displayPeak * math.Pow(ys, hlgSystemGamma-1)
	}
	return [3]float64{s[0] * gain, s[1] * gain, s[2] * gain}
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

// compactToneMapPixel maps one BT.2020 R'G'B' triple encoded with signal
// (pq or hlg) to BT.709 gamma-2.4 R'G'B' for a targetPeak display.
func compactToneMapPixel(signal string, r, g, b, sourcePeak, targetPeak float64) [3]float64 {
	var lin [3]float64
	if signal == "hlg" {
		lin = hlgDisplayLight(r, g, b, sourcePeak)
	} else {
		lin = [3]float64{pqEOTF(r), pqEOTF(g), pqEOTF(b)}
	}
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

// buildCompactLUT renders the .cube LUT of a tone map. Output is
// byte-for-byte deterministic for a given method and peaks.
func buildCompactLUT(signal string, tm *transcode.CompactToneMap) ([]byte, error) {
	if tm == nil {
		return nil, errors.New("compact tone map missing")
	}
	want := transcode.CompactToneMapPQ
	if signal == "hlg" {
		want = transcode.CompactToneMapHLG
	} else if signal != "pq" {
		return nil, fmt.Errorf("unsupported compact tone map signal %q", signal)
	}
	if tm.Method != want || tm.SourcePeakNits < 200 || tm.SourcePeakNits > 10000 || tm.TargetPeakNits != transcode.CompactTargetPeakNits {
		return nil, errors.New("invalid compact tone map")
	}
	var buf bytes.Buffer
	n := compactLUTSize
	fmt.Fprintf(&buf, "TITLE \"%s peak %d to %d\"\nLUT_3D_SIZE %d\nDOMAIN_MIN 0.0 0.0 0.0\nDOMAIN_MAX 1.0 1.0 1.0\n", tm.Method, tm.SourcePeakNits, tm.TargetPeakNits, n)
	step := 1 / float64(n-1)
	// .cube order: red varies fastest, then green, then blue.
	for bi := 0; bi < n; bi++ {
		for gi := 0; gi < n; gi++ {
			for ri := 0; ri < n; ri++ {
				o := compactToneMapPixel(signal, float64(ri)*step, float64(gi)*step, float64(bi)*step, float64(tm.SourcePeakNits), float64(tm.TargetPeakNits))
				fmt.Fprintf(&buf, "%.6f %.6f %.6f\n", o[0], o[1], o[2])
			}
		}
	}
	return buf.Bytes(), nil
}

// compactLUTDir holds content-addressed tone map LUTs. Tests override it.
var compactLUTDir = func() string { return filepath.Join(os.TempDir(), "navigatorr-compact-lut") }

var compactLUTMu sync.Mutex

// compactLUTPath is where the LUT of a plan lives. It is a pure function of
// the frozen plan so argument builders stay side-effect free.
func compactLUTPath(p *transcode.CompactPlan) string {
	return filepath.Join(compactLUTDir(), fmt.Sprintf("%s_%s_%d_%d.cube", transcode.CompactChainVersion, p.SourceSignal, p.ToneMap.SourcePeakNits, p.ToneMap.TargetPeakNits))
}

// ensureCompactLUT writes (or re-verifies) the LUT of a tone-mapped plan
// before any FFmpeg run that uses it and returns its digest. SDR plans need
// no LUT and return "".
func ensureCompactLUT(p *transcode.CompactPlan) (string, error) {
	if p == nil || p.ToneMap == nil {
		return "", nil
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	lut, err := buildCompactLUT(p.SourceSignal, p.ToneMap)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(lut)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	compactLUTMu.Lock()
	defer compactLUTMu.Unlock()
	dir := compactLUTDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating compact LUT directory: %w", err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("compact LUT directory is not a plain directory (fail closed)")
	}
	path := compactLUTPath(p)
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
		if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, lut) {
			return digest, nil
		}
	}
	tmp, err := os.CreateTemp(dir, ".lut-*")
	if err != nil {
		return "", fmt.Errorf("writing compact LUT: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(lut); err != nil {
		tmp.Close()
		return "", fmt.Errorf("writing compact LUT: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("writing compact LUT: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("installing compact LUT: %w", err)
	}
	return digest, nil
}

// compactVideoFilter is the deterministic picture chain of a compact plan.
// lutRef names the LUT in the filter; evidence passes a placeholder so no
// worker path is recorded. A keep-HDR plan's chain uses no LUT.
func compactVideoFilter(p *transcode.CompactPlan, lutRef string) (string, error) {
	if p == nil {
		return "", errors.New("compact plan missing")
	}
	if err := p.Validate(); err != nil {
		return "", err
	}
	size := "w=" + strconv.Itoa(p.TargetWidth) + ":h=" + strconv.Itoa(p.TargetHeight)
	resize := p.TargetWidth != p.SourceWidth || p.TargetHeight != p.SourceHeight
	out := "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"
	var chain string
	switch {
	case p.KeepsHDR():
		tag := "setparams=color_primaries=" + transcode.CompactHDRColorPrimaries + ":color_trc=" + p.HDR.Transfer + ":colorspace=" + transcode.CompactHDRColorMatrix + ":range=" + transcode.CompactHDRColorRange
		chain = tag + ","
		if resize {
			chain += "scale=" + size + ":flags=" + compactScaleFlags + ","
		}
		chain += "format=yuv420p10le," + tag
	case p.SourceSignal == "pq" || p.SourceSignal == "hlg":
		trc := "smpte2084"
		if p.SourceSignal == "hlg" {
			trc = "arib-std-b67"
		}
		chain = "setparams=color_primaries=bt2020:color_trc=" + trc + ":colorspace=bt2020nc:range=tv," +
			"scale=" + size + ":in_color_matrix=bt2020:in_range=tv:out_range=pc:flags=" + compactScaleFlags + ",format=gbrpf32le," +
			"lut3d=file=" + lutRef + ":interp=tetrahedral," +
			"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=pc," +
			"scale=in_color_matrix=bt709:out_color_matrix=bt709:in_range=pc:out_range=tv:flags=accurate_rnd+full_chroma_int,format=yuv420p10le," + out
	case p.SourceSignal == "sdr":
		chain = out
		if resize {
			chain = "scale=" + size + ":flags=" + compactScaleFlags + "," + out
		}
	default:
		return "", fmt.Errorf("unsupported compact source signal %q", p.SourceSignal)
	}
	for _, t := range compactStrippedSideData {
		chain += ",sidedata=mode=delete:type=" + t
	}
	return chain, nil
}

// compactFilterForRun returns the chain bound to the worker LUT file.
func compactFilterForRun(p *transcode.CompactPlan) (string, error) {
	ref := ""
	if p != nil && p.ToneMap != nil && !p.KeepsHDR() {
		ref = escapeFFmpegFilterPath(compactLUTPath(p))
	}
	return compactVideoFilter(p, ref)
}

// compactCandidateMeasurementPlan tone maps a keep-HDR candidate, which is
// already at the target size, onto the reference's measurement space.
func compactCandidateMeasurementPlan(p *transcode.CompactPlan) *transcode.CompactPlan {
	m := p.MeasurementPlan().Clone()
	m.SourceWidth, m.SourceHeight = m.TargetWidth, m.TargetHeight
	return m
}

// compactMeasurementChains are the chains the final validation applies to
// the reference and the candidate extracts, bound to the worker LUT file. An
// SDR candidate is measured as encoded (empty candidate chain).
func compactMeasurementChains(p *transcode.CompactPlan) (reference, candidate string, err error) {
	if p == nil {
		return "", "", errors.New("compact plan missing")
	}
	if !p.KeepsHDR() {
		reference, err = compactFilterForRun(p.MeasurementPlan())
		return reference, "", err
	}
	return compactKeepHDRMeasurementChains(p, escapeFFmpegFilterPath(compactLUTPath(p)))
}

// compactKeepHDRMeasurementChains builds a keep-HDR plan's measurement
// chains with the LUT reference lut. The reference first takes the encode's
// own picture chain (scaled to the target as 10-bit 4:2:0), so both sides
// reach the tone map at the same size and chroma resolution and only the
// encode differs.
func compactKeepHDRMeasurementChains(p *transcode.CompactPlan, lut string) (reference, candidate string, err error) {
	encode, err := compactVideoFilter(p, "")
	if err != nil {
		return "", "", err
	}
	if candidate, err = compactVideoFilter(compactCandidateMeasurementPlan(p), lut); err != nil {
		return "", "", err
	}
	return encode + "," + candidate, candidate, nil
}

// compactEncoderArgs are the video args a compact plan adds to an encode:
// the picture chain on the first output video stream and explicit stream
// signalling, SDR BT.709 or the frozen HDR. A keep-HDR plan also returns the
// x265 parameters BuildVideoEncoderArgs merges into -x265-params.
func compactEncoderArgs(p *transcode.CompactPlan, videoCodec string) (args []string, x265Params string, err error) {
	if p == nil {
		return nil, "", nil
	}
	chain, err := compactFilterForRun(p)
	if err != nil {
		return nil, "", err
	}
	if p.KeepsHDR() {
		if norm(videoCodec) != libX265Encoder {
			return nil, "", errors.New("keep-HDR compact encodes need libx265 (fail closed)")
		}
		args = []string{
			"-filter:v:0", chain,
			"-color_primaries:v:0", transcode.CompactHDRColorPrimaries,
			"-color_trc:v:0", p.HDR.Transfer,
			"-colorspace:v:0", transcode.CompactHDRColorMatrix,
			"-color_range:v:0", transcode.CompactHDRColorRange,
			"-dolbyvision:v:0", "0",
		}
		return args, compactX265Params(p.HDR), nil
	}
	args = []string{
		"-filter:v:0", chain,
		"-color_primaries:v:0", transcode.CompactColorPrimaries,
		"-color_trc:v:0", transcode.CompactColorTransfer,
		"-colorspace:v:0", transcode.CompactColorMatrix,
		"-color_range:v:0", transcode.CompactColorRange,
	}
	if norm(videoCodec) == libX265Encoder {
		// The Dolby Vision configuration record is stream side data that
		// survives the chain; "auto" would re-emit Dolby Vision signalling.
		args = append(args, "-dolbyvision:v:0", "0")
	}
	return args, "", nil
}

// compactX265Params are the x265 parameters of a keep-HDR encode: SEI on
// every keyframe and, for PQ, HDR10 coding with the frozen static metadata.
// The frame side data was deleted, so these are the only HDR metadata x265
// sees.
func compactX265Params(o *transcode.CompactHDROutput) string {
	params := "repeat-headers=1"
	if o.Transfer != transcode.CompactHDRTransferPQ {
		return params
	}
	params += ":hdr10=1:hdr10-opt=1"
	if o.MasterDisplay != "" {
		params += ":master-display=" + o.MasterDisplay
	}
	if o.MaxCLL > 0 || o.MaxFALL > 0 {
		params += ":max-cll=" + strconv.Itoa(o.MaxCLL) + "," + strconv.Itoa(o.MaxFALL)
	}
	return params
}

// mergeX265Params appends extra to the encoder's -x265-params, or adds the
// option, since FFmpeg keeps only the last one given.
func mergeX265Params(args []string, extra string) []string {
	if extra == "" {
		return args
	}
	for i, a := range args {
		if a == "-x265-params" && i+1 < len(args) {
			out := append([]string(nil), args...)
			out[i+1] += ":" + extra
			return out
		}
	}
	return append(args, "-x265-params", extra)
}

// compactChainEvidence records the transform for the final validation
// evidence. lutDigest is the value ensureCompactLUT returned.
func compactChainEvidence(p *transcode.CompactPlan, lutDigest string) (*transcode.CompactChainEvidence, error) {
	if p == nil {
		return nil, errors.New("compact plan missing")
	}
	ref := ""
	if p.ToneMap != nil {
		ref = "compact.cube"
	}
	chain, candidate := "", ""
	var err error
	if p.KeepsHDR() {
		chain, candidate, err = compactKeepHDRMeasurementChains(p, ref)
	} else {
		chain, err = compactVideoFilter(p.MeasurementPlan(), ref)
	}
	if err != nil {
		return nil, err
	}
	e := &transcode.CompactChainEvidence{
		ChainVersion:     p.ChainVersion,
		SourceSignal:     p.SourceSignal,
		LUTSHA256:        lutDigest,
		TargetWidth:      p.TargetWidth,
		TargetHeight:     p.TargetHeight,
		Scaler:           compactScaleFlags,
		ReferenceChain:   chain,
		MeasurementSpace: compactMeasurementSpace,
		AppliedTo:        "encoder_input_and_reference",
	}
	if p.KeepsHDR() {
		e.CandidateChain, e.MeasurementSpace, e.AppliedTo = candidate, compactHDRMeasurementSpace, "reference_and_candidate"
	}
	if t := p.ToneMap; t != nil {
		e.ToneMapMethod, e.SourcePeakNits, e.TargetPeakNits = t.Method, t.SourcePeakNits, t.TargetPeakNits
	}
	return e, nil
}

// probeCompactChain proves the worker FFmpeg runs the compact tone-map chain
// (every filter and side data type name included) on one synthetic frame.
func probeCompactChain(ctx context.Context, ffmpegPath string) error {
	p := compactProbePlan()
	if _, err := ensureCompactLUT(p); err != nil {
		return err
	}
	chain, err := compactFilterForRun(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-nostats", "-f", "lavfi", "-i", "testsrc2=s=1440x1080:r=24:d=1,format=yuv420p10le",
		"-frames:v", "1", "-vf", chain, "-f", "null", "-").CombinedOutput()
	if err != nil {
		if len(out) > 240 {
			out = out[len(out)-240:]
		}
		return fmt.Errorf("compact chain probe failed: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// probeCompactHDRChain proves the worker encodes one synthetic frame through
// the production keep-HDR arguments (BuildVideoEncoderArgs) with libx265, and
// that the decoded frame carries the planned BT.2020 PQ tags and exactly the
// probe plan's mastering display and light levels.
func probeCompactHDRChain(ctx context.Context, ffmpegPath, scratchRoot string) error {
	plan := &transcode.Plan{VideoCodec: libX265Encoder, Quality: 28, Preset: "ultrafast", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, Compact: compactHDRProbePlan()}
	args, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(scratchRoot, "compact-hdr-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tail := func(out []byte) []byte {
		if len(out) > 240 {
			return out[len(out)-240:]
		}
		return out
	}
	out := filepath.Join(dir, "probe.hevc")
	argv := append([]string{"-hide_banner", "-nostats", "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=1440x1080:r=24:d=1,format=yuv420p10le", "-frames:v", "1"}, args...)
	if b, err := exec.CommandContext(ctx, ffmpegPath, append(argv, "-f", "hevc", out)...).CombinedOutput(); err != nil {
		return fmt.Errorf("compact keep-HDR probe failed: %v: %s", err, bytes.TrimSpace(tail(b)))
	}
	info, err := exec.CommandContext(ctx, ffmpegPath, "-hide_banner", "-nostats", "-i", out, "-vf", "showinfo", "-f", "null", "-").CombinedOutput()
	if err != nil {
		return fmt.Errorf("compact keep-HDR probe decode failed: %v: %s", err, bytes.TrimSpace(tail(info)))
	}
	for _, want := range []string{"color_space:bt2020nc color_primaries:bt2020 color_trc:smpte2084",
		"Mastering display metadata: has_primaries:1 has_luminance:1 r(0.6800,0.3200) g(0.2650,0.6900) b(0.1500 0.0600) wp(0.3127, 0.3290) min_luminance=0.005000, max_luminance=1000.000000",
		"Content light level metadata: MaxCLL=1000, MaxFALL=400"} {
		if !bytes.Contains(info, []byte(want)) {
			return fmt.Errorf("compact keep-HDR probe output lacks %q", want)
		}
	}
	return nil
}

// compactHDRProbePlan is the fixed keep-HDR plan of the capability probe.
func compactHDRProbePlan() *transcode.CompactPlan {
	p := compactProbePlan()
	p.ChainVersion = transcode.CompactHDRChainVersion
	p.HDR = &transcode.CompactHDROutput{Transfer: transcode.CompactHDRTransferPQ, MasterDisplay: "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)", MaxCLL: 1000, MaxFALL: 400}
	return p
}

// compactProbePlan is the fixed plan of the capability probe.
func compactProbePlan() *transcode.CompactPlan {
	return &transcode.CompactPlan{
		ChainVersion: transcode.CompactChainVersion, SourceClass: "hdr10", SourceSignal: "pq",
		SourceWidth: 1440, SourceHeight: 1080, MaxHeight: 720, TargetWidth: 960, TargetHeight: 720,
		ToneMap: &transcode.CompactToneMap{Method: transcode.CompactToneMapPQ, SourcePeakNits: 1000, TargetPeakNits: transcode.CompactTargetPeakNits},
	}
}

// classifySourceDynamicRange classifies the main video stream. Stream-level
// SDR needs no frame evidence; any HDR signal is confirmed against decoded
// frames of that stream because HDR10+ and Dolby Vision RPUs are per frame.
func classifySourceDynamicRange(ctx context.Context, ffprobePath, path string, rep *mediainspect.DetailedReport) (mediainspect.DynamicRange, error) {
	return mediainspect.ClassifySource(ctx, ffprobePath, path, rep)
}
