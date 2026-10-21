package transcodeworker

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// useTempCompactLUTDir keeps LUTs written by a test inside its temp dir.
func useTempCompactLUTDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "lut")
	prev := compactLUTDir
	compactLUTDir = func() string { return dir }
	t.Cleanup(func() { compactLUTDir = prev })
	return dir
}

func compactPQPlan() *transcode.CompactPlan {
	return &transcode.CompactPlan{
		ChainVersion: transcode.CompactChainVersion, SourceClass: mediainspect.DynamicRangeHDR10, SourceSignal: mediainspect.SignalPQ,
		SourceWidth: 3840, SourceHeight: 2160, MaxHeight: 1080, TargetWidth: 1920, TargetHeight: 1080,
		ToneMap: &transcode.CompactToneMap{Method: transcode.CompactToneMapPQ, SourcePeakNits: 1000, TargetPeakNits: transcode.CompactTargetPeakNits},
	}
}

func compactHLGPlan() *transcode.CompactPlan {
	p := compactPQPlan()
	p.SourceClass, p.SourceSignal = mediainspect.DynamicRangeHLG, mediainspect.SignalHLG
	p.ToneMap = &transcode.CompactToneMap{Method: transcode.CompactToneMapHLG, SourcePeakNits: transcode.CompactHLGDisplayPeakNits, TargetPeakNits: transcode.CompactTargetPeakNits}
	return p
}

func compactSDRPlan() *transcode.CompactPlan {
	p := compactPQPlan()
	p.SourceClass, p.SourceSignal, p.ToneMap = mediainspect.DynamicRangeSDR, mediainspect.SignalSDR, nil
	return p
}

func TestCompactTransferFunctions(t *testing.T) {
	for _, nits := range []float64{0.01, 1, 100, 203, 1000, 4000, 10000} {
		if got := pqEOTF(pqInverseEOTF(nits)); math.Abs(got-nits)/nits > 1e-9 {
			t.Errorf("PQ round trip %v -> %v", nits, got)
		}
	}
	if pqEOTF(1) != 10000 || pqEOTF(0) != 0 {
		t.Fatal("PQ endpoints")
	}
	// The two HLG inverse OETF segments meet at E'=0.5 (scene light 1/12).
	if lo, hi := hlgInverseOETF(0.5-1e-9), hlgInverseOETF(0.5+1e-9); math.Abs(lo-hi) > 1e-6 || math.Abs(lo-1.0/12) > 1e-6 {
		t.Fatalf("HLG discontinuity %v %v", lo, hi)
	}
	if math.Abs(hlgInverseOETF(1)-1) > 1e-6 {
		t.Fatalf("HLG peak %v", hlgInverseOETF(1))
	}
	// The EETF is monotonic, never exceeds the target and leaves shadows alone.
	prev := -1.0
	for l := 0.0; l <= 1000; l += 5 {
		v := bt2390EETF(l, 1000, 100)
		if v < prev-1e-9 || v > 100+1e-6 {
			t.Fatalf("EETF(%v)=%v after %v", l, v, prev)
		}
		prev = v
	}
	if v := bt2390EETF(1, 1000, 100); math.Abs(v-1) > 0.05 {
		t.Fatalf("EETF moved shadows: %v", v)
	}
	if v := bt2390EETF(1000, 1000, 100); math.Abs(v-100) > 0.5 {
		t.Fatalf("EETF peak maps to %v", v)
	}
}

func TestCompactToneMapPixel(t *testing.T) {
	// Neutral greys stay neutral in both signals and the output stays in range.
	for _, signal := range []string{mediainspect.SignalPQ, mediainspect.SignalHLG} {
		prev := -1.0
		for _, e := range []float64{0, 0.1, 0.3, 0.5, 0.58, 0.75, 1} {
			o := compactToneMapPixel(signal, e, e, e, 1000, 100)
			if math.Abs(o[0]-o[1]) > 1e-6 || math.Abs(o[1]-o[2]) > 1e-6 {
				t.Fatalf("%s grey %v became %v", signal, e, o)
			}
			if o[0] < prev-1e-9 || o[0] < 0 || o[0] > 1 {
				t.Fatalf("%s grey ramp not monotonic in range at %v: %v", signal, e, o)
			}
			prev = o[0]
		}
	}
	// PQ 100 cd/m² diffuse white lands near SDR white minus the EETF knee.
	o := compactToneMapPixel(mediainspect.SignalPQ, pqInverseEOTF(100), pqInverseEOTF(100), pqInverseEOTF(100), 1000, 100)
	if o[0] < 0.75 || o[0] > 1 {
		t.Fatalf("diffuse white %v", o)
	}
	// Saturated BT.2020 primaries are clipped into BT.709, never negative.
	o = compactToneMapPixel(mediainspect.SignalPQ, 0.6, 0, 0, 1000, 100)
	if o[0] <= 0 || o[1] != 0 || o[2] != 0 {
		t.Fatalf("bt2020 red %v", o)
	}
}

func TestCompactLUTDeterministicAndVerified(t *testing.T) {
	dir := useTempCompactLUTDir(t)
	a, err := buildCompactLUT(mediainspect.SignalPQ, compactPQPlan().ToneMap)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := buildCompactLUT(mediainspect.SignalPQ, compactPQPlan().ToneMap)
	if !bytes.Equal(a, b) || !bytes.HasPrefix(a, []byte("TITLE")) || !bytes.Contains(a, []byte("LUT_3D_SIZE 33\n")) {
		t.Fatal("LUT not deterministic or malformed")
	}
	if lines := bytes.Count(a, []byte("\n")); lines != 4+33*33*33 {
		t.Fatalf("LUT has %d lines", lines)
	}
	h, _ := buildCompactLUT(mediainspect.SignalHLG, compactHLGPlan().ToneMap)
	if bytes.Equal(a, h) {
		t.Fatal("PQ and HLG LUTs identical")
	}
	for _, bad := range []struct {
		signal string
		tm     *transcode.CompactToneMap
	}{
		{mediainspect.SignalPQ, nil},
		{mediainspect.SignalSDR, compactPQPlan().ToneMap},
		{mediainspect.SignalHLG, compactPQPlan().ToneMap},
		{mediainspect.SignalPQ, &transcode.CompactToneMap{Method: transcode.CompactToneMapPQ, SourcePeakNits: 1000, TargetPeakNits: 203}},
	} {
		if _, err := buildCompactLUT(bad.signal, bad.tm); err == nil {
			t.Errorf("invalid LUT input accepted: %s %+v", bad.signal, bad.tm)
		}
	}

	p := compactPQPlan()
	d1, err := ensureCompactLUT(p)
	if err != nil || !strings.HasPrefix(d1, "sha256:") || len(d1) != 71 {
		t.Fatalf("digest %q %v", d1, err)
	}
	path := compactLUTPath(p)
	if filepath.Dir(path) != dir {
		t.Fatalf("LUT outside the configured dir: %s", path)
	}
	// A tampered LUT is rewritten, never trusted.
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	d2, err := ensureCompactLUT(p)
	got, _ := os.ReadFile(path)
	if err != nil || d2 != d1 || !bytes.Equal(got, a) {
		t.Fatalf("tampered LUT not repaired: %v", err)
	}
	if d, err := ensureCompactLUT(compactSDRPlan()); d != "" || err != nil {
		t.Fatalf("SDR needs no LUT: %q %v", d, err)
	}
	invalid := compactPQPlan()
	invalid.TargetWidth = 1280
	if _, err := ensureCompactLUT(invalid); err == nil {
		t.Fatal("invalid plan wrote a LUT")
	}
}

func TestCompactVideoFilter(t *testing.T) {
	sideData := ",sidedata=mode=delete:type=MASTERING_DISPLAY_METADATA,sidedata=mode=delete:type=CONTENT_LIGHT_LEVEL,sidedata=mode=delete:type=DYNAMIC_HDR_PLUS,sidedata=mode=delete:type=DYNAMIC_HDR_VIVID,sidedata=mode=delete:type=DOVI_RPU_BUFFER,sidedata=mode=delete:type=DOVI_METADATA,sidedata=mode=delete:type=AMBIENT_VIEWING_ENVIRONMENT"
	tail := ",setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=pc,scale=in_color_matrix=bt709:out_color_matrix=bt709:in_range=pc:out_range=tv:flags=accurate_rnd+full_chroma_int,format=yuv420p10le,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv" + sideData
	cases := []struct {
		name string
		plan *transcode.CompactPlan
		want string
	}{
		{"pq", compactPQPlan(), "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv,scale=w=1920:h=1080:in_color_matrix=bt2020:in_range=tv:out_range=pc:flags=lanczos+accurate_rnd+full_chroma_int,format=gbrpf32le,lut3d=file=L:interp=tetrahedral" + tail},
		{"hlg", compactHLGPlan(), "setparams=color_primaries=bt2020:color_trc=arib-std-b67:colorspace=bt2020nc:range=tv,scale=w=1920:h=1080:in_color_matrix=bt2020:in_range=tv:out_range=pc:flags=lanczos+accurate_rnd+full_chroma_int,format=gbrpf32le,lut3d=file=L:interp=tetrahedral" + tail},
		{"sdr resize", compactSDRPlan(), "scale=w=1920:h=1080:flags=lanczos+accurate_rnd+full_chroma_int,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv" + sideData},
	}
	for _, c := range cases {
		got, err := compactVideoFilter(c.plan, "L")
		if err != nil || got != c.want {
			t.Errorf("%s:\n got %s %v\nwant %s", c.name, got, err, c.want)
		}
	}
	// A 4K SDR source at max 2160 is rejected as not needed, but a 4K Dolby
	// Vision 8.2 (SDR base layer) source is only retagged and stripped.
	dv := compactSDRPlan()
	dv.SourceClass, dv.DolbyVisionProfile, dv.MaxHeight, dv.TargetWidth, dv.TargetHeight = mediainspect.DynamicRangeDolbyVision, 8, 2160, 3840, 2160
	if got, err := compactVideoFilter(dv, ""); err != nil || got != "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"+sideData {
		t.Fatalf("no-resize SDR chain %q %v", got, err)
	}
	if _, err := compactVideoFilter(nil, ""); err == nil {
		t.Fatal("nil plan accepted")
	}
	bad := compactPQPlan()
	bad.ToneMap.SourcePeakNits = 50
	if _, err := compactVideoFilter(bad, "L"); err == nil {
		t.Fatal("invalid plan accepted")
	}
}

func TestCompactEncoderArgsAndEvidence(t *testing.T) {
	useTempCompactLUTDir(t)
	p := compactPQPlan()
	plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, Compact: p}
	args, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c:v libx265", "-filter:v:0 setparams=color_primaries=bt2020", "lut3d=file=", "-color_primaries:v:0 bt709", "-color_trc:v:0 bt709", "-colorspace:v:0 bt709", "-color_range:v:0 tv", "-pix_fmt yuv420p10le"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	// The LUT path in the filter is escaped like every other filter path.
	if !strings.Contains(joined, "lut3d=file="+escapeFFmpegFilterPath(compactLUTPath(p))+":interp") {
		t.Fatal("LUT path not escaped")
	}
	// Without a compact plan nothing is added.
	plain := *plan
	plain.Compact = nil
	args, _ = BuildVideoEncoderArgs(&plain)
	if strings.Contains(strings.Join(args, " "), "-filter:v:0") || strings.Contains(strings.Join(args, " "), "bt709") {
		t.Fatal("compact args leaked into a normal encode")
	}

	e, err := compactChainEvidence(p, "sha256:abc")
	if err != nil || e.ChainVersion != transcode.CompactChainVersion || e.ToneMapMethod != transcode.CompactToneMapPQ || e.SourcePeakNits != 1000 || e.TargetWidth != 1920 || e.LUTSHA256 != "sha256:abc" || e.AppliedTo != "encoder_input_and_reference" {
		t.Fatalf("evidence %+v %v", e, err)
	}
	// Evidence never records a worker path.
	if !strings.Contains(e.ReferenceChain, "lut3d=file=compact.cube:") || strings.Contains(e.ReferenceChain, compactLUTDir()) {
		t.Fatalf("evidence chain %s", e.ReferenceChain)
	}
}

func TestWithReferenceChain(t *testing.T) {
	in := []string{"-i", "src.mkv", "-map", "0:v:0", "-c:v", "ffv1", "-c:v", "ffv1", "out.mkv"}
	got := strings.Join(withReferenceChain(in, "CHAIN"), " ")
	if got != "-i src.mkv -map 0:v:0 -filter:v:0 CHAIN -c:v ffv1 -c:v ffv1 out.mkv" {
		t.Fatal(got)
	}
}

func TestCheckCompactOutput(t *testing.T) {
	p := compactPQPlan()
	clean := mediainspect.DetailedStream{Kind: "video", Width: 1920, Height: 1080, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorSpace: "bt709", ColorRange: "tv"}
	frames := mediainspect.FrameSignals{Probed: true, Frames: 8, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorSpace: "bt709", ColorRange: "tv"}
	if e := checkCompactOutput(clean, frames, p); e.Verdict != "pass" || len(e.ReasonCodes) != 0 || e.FramesProbed != 8 {
		t.Fatalf("clean output rejected: %+v", e)
	}
	cases := map[string]struct {
		mutate func(*mediainspect.DetailedStream, *mediainspect.FrameSignals)
		reason string
	}{
		"size":       {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.Width = 3840 }, "compact_output_size_mismatch"},
		"untagged":   {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.ColorTransfer = "" }, "compact_output_stream_not_bt709"},
		"full range": {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.ColorRange = "pc" }, "compact_output_stream_not_bt709"},
		"pq frames":  {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.ColorTransfer = "smpte2084" }, "compact_output_frames_not_bt709"},
		"mastering": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) {
			f.MasteringDisplay = &mediainspect.MasteringDisplayMetadata{}
		}, "compact_output_hdr_signalling_present"},
		"cll in stream": {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) {
			v.ContentLightLevel = &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000}
		}, "compact_output_hdr_signalling_present"},
		"hdr10plus": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.HDR10Plus = true }, "compact_output_hdr_signalling_present"},
		"dovi rpu":  {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.DolbyVisionRPU = true }, "compact_output_hdr_signalling_present"},
		"dynamic": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) {
			f.DynamicMetadata = []string{"HDR Vivid"}
		}, "compact_output_hdr_signalling_present"},
		"dovi config rec": {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) {
			v.SideData = []mediainspect.SideDataRecord{dovi8Record()}
		}, "compact_output_hdr_signalling_present"},
	}
	for name, c := range cases {
		v, f := clean, frames
		c.mutate(&v, &f)
		e := checkCompactOutput(v, f, p)
		if e.Verdict != "fail" || !containsString(e.ReasonCodes, c.reason) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	if err := compactOutputError(&transcode.CompactOutputEvidence{ReasonCodes: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "x") {
		t.Fatal("error lost reason")
	}
}

func dovi8Record() mediainspect.SideDataRecord {
	return mediainspect.SideDataRecord{SideDataType: "DOVI configuration record", Data: map[string]any{"dv_version_major": float64(1), "dv_profile": float64(8), "dv_level": float64(6), "rpu_present_flag": float64(1), "el_present_flag": float64(0), "bl_present_flag": float64(1), "dv_bl_signal_compatibility_id": float64(1)}}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// compactModePlan is a resolved compact-mode plan as the server freezes it.
func compactModePlan(t *testing.T, c *transcode.CompactPlan) *transcode.Plan {
	t.Helper()
	return compactModePlanFor(t, c, 10)
}

// compactModePlanFor derives the output shape from the source bit depth: the
// compact chain keeps it (HDR sources are 10-bit), which fixes the HEVC
// profile and pixel format.
func compactModePlanFor(t *testing.T, c *transcode.CompactPlan, sourceBitDepth int) *transcode.Plan {
	t.Helper()
	p := signedPlan(t, nil)
	p.VideoCodec, p.Quality, p.Preset = transcode.VideoCodecLibX265, 24, "ultrafast"
	prof, pix, bd := "main", "yuv420p", 8
	if sourceBitDepth == 10 {
		prof, pix, bd = "main10", "p010le", 10
	}
	p.VideoProfile, p.PixelFormat, p.ExpectedBitDepth = prof, pix, bd
	p.AudioMode = "compact"
	p.Mode, p.ModePolicyVersion = "compact", transcode.ModePolicyV2
	p.PolicyDigest = "sha256:" + strings.Repeat("0", 64)
	p.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: transcode.CompactMinimumSavingsPercent}
	p.QualityValidation = &transcode.QualityValidationPlan{
		Metric: "vmaf", BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64),
		Quality: transcode.BenchmarkQualityConfig{
			VMAF:            &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 88, Target: 92},
			FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"},
		},
		Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0, DurationSeconds: 1}},
	}
	p.Compact = c
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

func TestValidatePlanCompactRules(t *testing.T) {
	if err := ValidatePlan(compactModePlan(t, compactPQPlan())); err != nil {
		t.Fatal(err)
	}
	resign := func(p *transcode.Plan) *transcode.Plan { p.PlanDigest, _ = transcode.DigestPlan(p); return p }
	cases := map[string]*transcode.Plan{
		"compact mode without plan": compactModePlan(t, nil),
		"plan without compact mode": func() *transcode.Plan {
			p := compactModePlan(t, compactPQPlan())
			p.Mode = "size"
			p.SizePolicy.MinSavingsPercent = 15
			return resign(p)
		}(),
		"invalid compact plan": func() *transcode.Plan { c := compactPQPlan(); c.TargetHeight = 720; return compactModePlan(t, c) }(),
		"loosened savings": func() *transcode.Plan {
			p := compactModePlan(t, compactPQPlan())
			p.SizePolicy.MinSavingsPercent = 15
			return resign(p)
		}(),
		"audio copied": func() *transcode.Plan {
			p := compactModePlan(t, compactPQPlan())
			p.AudioMode = "copy"
			return resign(p)
		}(),
		"v1 policy": func() *transcode.Plan {
			p := compactModePlan(t, compactPQPlan())
			p.ModePolicyVersion = "transcode-modes-v1"
			return resign(p)
		}(),
	}
	for name, p := range cases {
		if err := ValidatePlan(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// compactRealEnv skips unless ffmpeg can execute the compact chain with
// libx265 and AAC.
func compactRealEnv(t *testing.T) (ctx context.Context, ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	t.Cleanup(cancel)
	enc, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !enc["libx265"] || !enc["aac"] {
		t.Skip("libx265/AAC unavailable")
	}
	useTempCompactLUTDir(t)
	if err := probeCompactChain(ctx, ffmpeg); err != nil {
		t.Skipf("compact chain not executable here: %v", err)
	}
	return ctx, ffmpeg, ffprobe
}

// compactRealSource writes a short synthetic 4K clip (10-bit HDR10 or 8-bit
// SDR) with a 5.1 PCM track, a second stereo FLAC track and an SRT subtitle.
func compactRealSource(t *testing.T, ctx context.Context, ffmpeg, path string, hdr10 bool) {
	t.Helper()
	srt := filepath.Join(filepath.Dir(path), "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=24:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000:duration=1",
		"-i", srt,
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:s",
		"-filter:a:0", "pan=5.1|FL=c0|FR=c0|FC=c0|LFE=c0|BL=c0|BR=c0",
		"-c:a:0", "pcm_s24le", "-c:a:1", "flac", "-ac:a:1", "2", "-c:s", "srt",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=spa", "-metadata:s:s:0", "language=eng",
		// Titles and mkvmerge statistics as release groups write them; the
		// video title claims Dolby Vision although the stream has none.
		"-metadata:s:v:0", "title=HEVC Dolby Vision HDR10", "-metadata:s:v:0", "BPS=40000000",
		"-metadata:s:a:0", "title=LPCM 5.1", "-metadata:s:a:0", "BPS=6912000", "-metadata:s:a:0", "NUMBER_OF_BYTES=864000",
		"-metadata:s:a:1", "title=Commentary",
	}
	if hdr10 {
		args = append(args, "-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
			"-c:v", "libx265", "-preset", "ultrafast", "-crf", "16", "-pix_fmt", "yuv420p10le",
			"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:hdr10-opt=1:repeat-headers=1:master-display=G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50):max-cll=1000,400:log-level=error",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv")
	} else {
		args = append(args, "-vf", "format=yuv420p,setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv",
			"-c:v", "libx264", "-preset", "ultrafast", "-crf", "16",
			"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv")
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("synthetic source: %v %s", err, out)
	}
}

// TestRealCompactEncode encodes synthetic 4K HDR10 and 4K SDR sources with
// the production compact plan and arguments and checks the result is clean
// 1080p SDR BT.709 with every audio and subtitle track kept.
func TestRealCompactEncode(t *testing.T) {
	ctx, ffmpeg, ffprobe := compactRealEnv(t)
	for _, tc := range []struct {
		name  string
		hdr10 bool
	}{{"hdr10", true}, {"sdr", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.mkv")
			compactRealSource(t, ctx, ffmpeg, source, tc.hdr10)
			rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
			if err != nil {
				t.Fatal(err)
			}
			c, err := transcode.NewCompactPlan(d, rep.Video[0], transcode.CompactDefaultMaxHeight)
			if err != nil {
				t.Fatalf("plan for %+v: %v", d, err)
			}
			if wantSignal := map[bool]string{true: mediainspect.SignalPQ, false: mediainspect.SignalSDR}[tc.hdr10]; c.SourceSignal != wantSignal || c.TargetWidth != 1920 || c.TargetHeight != 1080 {
				t.Fatalf("plan %+v", c)
			}
			if tc.hdr10 && c.ToneMap.SourcePeakNits != 1000 {
				t.Fatalf("peak %+v", c.ToneMap)
			}
			if err := c.MatchSource(d, rep.Video[0]); err != nil {
				t.Fatal(err)
			}

			// The output shape comes from the production derivation: 8-bit
			// SDR 4K stays Main, HDR10 becomes Main10.
			plan := compactModePlanFor(t, c, rep.Video[0].BitDepth)
			if want := map[bool]int{true: 10, false: 8}[tc.hdr10]; plan.ExpectedBitDepth != want || rep.Video[0].BitDepth != want {
				t.Fatalf("source %d-bit gave a %d-bit plan (%s/%s)", rep.Video[0].BitDepth, plan.ExpectedBitDepth, plan.VideoProfile, plan.PixelFormat)
			}
			probe, err := ProbeSourceDetails(ctx, ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range probe.Streams {
				if s.Kind == "subtitle" {
					plan.SubtitleActions = append(plan.SubtitleActions, transcode.SubtitleAction{SourceStreamIndex: s.Index, TypeIndex: s.TypeIndex, SourceCodec: s.Codec, Operation: "copy", Codec: "copy"})
				}
			}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ensureCompactLUT(c); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(dir, "candidate.mkv")
			args, err := BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
			if err != nil {
				t.Fatal(err)
			}
			if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("compact encode: %v %s", err, out)
			}

			out, err := mediainspect.InspectDetailed(ctx, ffprobe, candidate)
			if err != nil {
				t.Fatal(err)
			}
			e := validateCompactOutput(ctx, ffprobe, candidate, &out, c)
			if e.Verdict != "pass" || e.Width != 1920 || e.Height != 1080 || e.FramesProbed == 0 {
				t.Fatalf("output not clean SDR: %+v", e)
			}
			if v := out.Video[0]; v.Codec != "hevc" || v.BitDepth != plan.ExpectedBitDepth || v.PixelFormat != map[int]string{8: "yuv420p", 10: "yuv420p10le"}[plan.ExpectedBitDepth] {
				t.Fatalf("video %+v", v)
			}
			// The misleading title and the source statistics are gone; the
			// converted track is retitled, the commentary title is kept.
			if v := out.Video[0]; v.Tags["title"] != "" || v.Tags["BPS"] != "" || mediainspect.HasDolbyVision(v) {
				t.Fatalf("video tags survived: %v", v.Tags)
			}
			if a := out.Audio; a[0].Tags["title"] != "AAC 5.1" || a[0].Tags["BPS"] != "" || a[0].Tags["NUMBER_OF_BYTES"] != "" || a[1].Tags["title"] != "Commentary" {
				t.Fatalf("audio tags: %v / %v", a[0].Tags, a[1].Tags)
			}
			// Re-classifying the output gives SDR: nothing HDR survived.
			if od, err := classifySourceDynamicRange(ctx, ffprobe, candidate, &out); err != nil || od.Class != mediainspect.DynamicRangeSDR {
				t.Fatalf("output classified %+v %v", od, err)
			}
			result, err := ProbeSourceDetails(ctx, ffprobe, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateAudioPreservation(plan, probe.Streams, result.Streams); err != nil {
				t.Fatal(err)
			}
			audio := streamsOfKind(result.Streams, "audio")
			if len(audio) != 2 || audio[0].Codec != "aac" || audio[0].Channels != 6 || audio[0].Language != "eng" || audio[1].Codec != "aac" || audio[1].Channels != 2 || audio[1].Language != "spa" {
				t.Fatalf("audio %+v", audio)
			}
			if subs := streamsOfKind(result.Streams, "subtitle"); len(subs) != 1 || subs[0].Language != "eng" {
				t.Fatalf("subtitles %+v", subs)
			}
			if a := ep.Streams; len(a) == 0 {
				t.Fatal("no stream actions")
			}
			for _, s := range ep.Streams {
				if s.Kind == "audio" && s.TypeIndex == 0 && (s.TargetCodec != "aac" || s.BitrateKbps != 384) {
					t.Fatalf("5.1 PCM action %+v", s)
				}
			}
		})
	}
}

// TestRealCompactChainRejectsUnstrippedEncode proves the output check sees
// HDR10 signalling when the chain is bypassed.
func TestRealCompactChainRejectsUnstrippedEncode(t *testing.T) {
	ctx, ffmpeg, ffprobe := compactRealEnv(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	compactRealSource(t, ctx, ffmpeg, source, true)
	bypass := filepath.Join(dir, "bypass.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", source, "-map", "0:v", "-vf", "scale=1920:1080", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", bypass).CombinedOutput(); err != nil {
		t.Fatalf("bypass encode: %v %s", err, out)
	}
	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, bypass)
	if err != nil {
		t.Fatal(err)
	}
	e := validateCompactOutput(ctx, ffprobe, bypass, &rep, compactPQPlan())
	if e.Verdict != "fail" || len(e.ReasonCodes) == 0 {
		t.Fatalf("unstripped encode accepted: %+v", e)
	}
	t.Logf("bypass rejected: %v %v", e.ReasonCodes, e.HDRSideData)
}
