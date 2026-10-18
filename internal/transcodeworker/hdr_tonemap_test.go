package transcodeworker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

const testHDRMasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"

func testHDR10Plan() *transcode.HDR10Plan {
	return &transcode.HDR10Plan{SourceClass: transcode.HDRSourceHDR10, MasterDisplay: testHDRMasterDisplay, MaxCLL: 1000, MaxFALL: 400, ToneMap: transcode.HDRToneMap{Method: transcode.HDRToneMapMethod, SourcePeakNits: 1000, TargetPeakNits: transcode.HDRToneMapTargetNits}}
}

func TestPQRoundTrip(t *testing.T) {
	for _, nits := range []float64{0.005, 1, 100, 203, 1000, 4000, 10000} {
		if got := pqEOTF(pqInverseEOTF(nits)); math.Abs(got-nits) > nits*1e-9+1e-9 {
			t.Fatalf("PQ round trip %g -> %g", nits, got)
		}
	}
}

func TestBT2390EETF(t *testing.T) {
	const peak, target = 1000.0, 100.0
	if got := bt2390EETF(peak, peak, target); math.Abs(got-target) > 0.01 {
		t.Fatalf("source peak maps to %g, want display peak %g", got, target)
	}
	// Below the knee the curve is identity.
	for _, l := range []float64{0.1, 1, 5, 10} {
		if got := bt2390EETF(l, peak, target); math.Abs(got-l) > l*1e-9 {
			t.Fatalf("%g nits below the knee changed to %g", l, got)
		}
	}
	prev := 0.0
	for l := 0.01; l <= peak; l *= 1.05 {
		got := bt2390EETF(l, peak, target)
		if got < prev || got > target+1e-9 {
			t.Fatalf("EETF not monotonic or above display peak at %g: %g", l, got)
		}
		prev = got
	}
	// Highlights between the knee and the peak are compressed, not clipped.
	if a, b := bt2390EETF(400, peak, target), bt2390EETF(800, peak, target); !(a < b && b < target) {
		t.Fatalf("highlights clipped: 400->%g 800->%g", a, b)
	}
}

func TestHDRToneMapLUTDeterministic(t *testing.T) {
	a, err := buildHDRToneMapLUT(1000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := buildHDRToneMapLUT(1000)
	if !bytes.Equal(a, b) {
		t.Fatal("LUT is not byte-for-byte deterministic")
	}
	lines := strings.Split(strings.TrimSuffix(string(a), "\n"), "\n")
	if len(lines) != 4+hdrToneMapLUTSize*hdrToneMapLUTSize*hdrToneMapLUTSize || lines[1] != "LUT_3D_SIZE 33" {
		t.Fatalf("unexpected LUT shape: %d lines, %q", len(lines), lines[1])
	}
	if lines[4] != "0.000000 0.000000 0.000000" || lines[len(lines)-1] != "1.000000 1.000000 1.000000" {
		t.Fatalf("black/white endpoints: %q %q", lines[4], lines[len(lines)-1])
	}
	// The digest identifies the method in evidence; changing the curve must
	// come with a new transcode.HDRToneMapMethod.
	sum := sha256.Sum256(a)
	t.Logf("LUT sha256:%s", hex.EncodeToString(sum[:]))
	c, _ := buildHDRToneMapLUT(4000)
	if bytes.Equal(a, c) {
		t.Fatal("source peak must change the LUT")
	}
	for _, peak := range []int{0, 199, 10001} {
		if _, err := buildHDRToneMapLUT(peak); err == nil {
			t.Fatalf("peak %d must be rejected", peak)
		}
	}
}

func TestHDRToneMapGrayAxis(t *testing.T) {
	prev := -1.0
	for i := 0; i <= 64; i++ {
		e := float64(i) / 64
		o := hdrToneMapPixel(e, e, e, 1000, 100)
		if math.Abs(o[0]-o[1]) > 1e-7 || math.Abs(o[1]-o[2]) > 1e-7 {
			t.Fatalf("gray %g gained a tint: %v", e, o)
		}
		if o[0] < prev {
			t.Fatalf("gray axis not monotonic at %g", e)
		}
		prev = o[0]
	}
	// 10 nits is below the knee: BT.1886 encoding of 10% of display white.
	e := pqInverseEOTF(10)
	if o := hdrToneMapPixel(e, e, e, 1000, 100); math.Abs(o[0]-math.Pow(0.1, 1/2.4)) > 1e-6 {
		t.Fatalf("10 nits encoded as %g", o[0])
	}
}

func TestPrepareHDRToneMapAndMetricArgs(t *testing.T) {
	dir := t.TempDir()
	if tm, err := prepareHDRToneMap(dir, nil); err != nil || tm != nil {
		t.Fatalf("SDR has no tone map: %v %v", tm, err)
	}
	bad := testHDR10Plan()
	bad.ToneMap.Method = "hable"
	if _, err := prepareHDRToneMap(dir, bad); err == nil {
		t.Fatal("invalid plan must fail closed")
	}
	tm, err := prepareHDRToneMap(dir, testHDR10Plan())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tm.lutPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	ev := tm.evidenceCopy()
	if ev.LUTSHA256 != "sha256:"+hex.EncodeToString(sum[:]) || ev.Method != transcode.HDRToneMapMethod || ev.AppliedTo != "reference_and_candidate" || ev.SourcePeakNits != 1000 || ev.TargetPeakNits != 100 {
		t.Fatalf("tone map evidence %+v", ev)
	}
	model, err := quality.Model("v1_1080p_3h")
	if err != nil {
		t.Fatal(err)
	}
	sdr, err := BuildVMAFModelArgs("cand.mkv", "ref.mkv", "vmaf.json", model, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	nilTM, _ := buildVMAFModelArgs("cand.mkv", "ref.mkv", "vmaf.json", model, 10, 2, nil)
	if strings.Join(sdr, " ") != strings.Join(nilTM, " ") {
		t.Fatal("SDR VMAF arguments changed")
	}
	hdr, err := buildVMAFModelArgs("cand.mkv", "ref.mkv", "vmaf.json", model, 10, 2, tm)
	if err != nil {
		t.Fatal(err)
	}
	cambi := buildCAMBIArgs("cand.mkv", "ref.mkv", "cambi.json", 10, 2, tm)
	chain := tm.metricInputChain()
	for name, args := range map[string][]string{"vmaf": hdr, "cambi": cambi} {
		graph := args[len(args)-4]
		if strings.Count(graph, chain) != 2 || !strings.Contains(graph, "[0:v]"+chain+",setpts") || !strings.Contains(graph, "[1:v]"+chain+",setpts") {
			t.Fatalf("%s graph does not apply the identical tone map to both inputs: %s", name, graph)
		}
	}
	if !strings.Contains(chain, "lut3d=file=") || !strings.Contains(chain, "interp=tetrahedral") || !strings.Contains(chain, "color_trc=smpte2084") || !strings.HasSuffix(chain, "format=yuv420p10le") {
		t.Fatalf("unexpected tone map chain %s", chain)
	}
}

func TestHDR10OutputReasons(t *testing.T) {
	display := &mediainspect.MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: "10000000/10000"}
	stream := func() mediainspect.DetailedStream {
		return mediainspect.DetailedStream{Kind: "video", Codec: "hevc", BitDepth: 10, PixelFormat: "yuv420p10le", ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc", ColorRange: "tv", MasteringDisplay: display, ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	}
	frames := func() mediainspect.FrameSignals {
		return mediainspect.FrameSignals{Probed: true, Frames: 8, ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc", ColorRange: "tv", MasteringDisplay: display, ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	}
	e := &transcode.HDR10OutputEvidence{}
	if reasons := hdr10OutputReasons(stream(), frames(), testHDR10Plan(), e); len(reasons) != 0 {
		t.Fatalf("preserved HDR10 rejected: %v", reasons)
	}
	if e.MasterDisplay != testHDRMasterDisplay || e.MaxCLL != 1000 || e.MaxFALL != 400 || e.FramesProbed != 8 {
		t.Fatalf("evidence %+v", e)
	}
	cases := []struct {
		want   string
		stream func(*mediainspect.DetailedStream)
		frames func(*mediainspect.FrameSignals)
	}{
		{"hdr10_output_transfer_mismatch", func(s *mediainspect.DetailedStream) { s.ColorTransfer = "bt709" }, nil},
		{"hdr10_output_transfer_mismatch", nil, func(f *mediainspect.FrameSignals) { f.ColorTransfer = "" }},
		{"hdr10_output_primaries_mismatch", func(s *mediainspect.DetailedStream) { s.ColorPrimaries = "bt709" }, nil},
		{"hdr10_output_matrix_mismatch", nil, func(f *mediainspect.FrameSignals) { f.ColorSpace = "bt709" }},
		{"hdr10_output_range_mismatch", func(s *mediainspect.DetailedStream) { s.ColorRange = "pc" }, nil},
		{"hdr10_output_bit_depth_mismatch", func(s *mediainspect.DetailedStream) { s.BitDepth = 8 }, nil},
		{"hdr10_output_container_mastering_display_mismatch", func(s *mediainspect.DetailedStream) { s.MasteringDisplay = nil }, nil},
		{"hdr10_output_bitstream_mastering_display_mismatch", nil, func(f *mediainspect.FrameSignals) {
			md := *display
			md.MaxLuminance = "4000"
			f.MasteringDisplay = &md
		}},
		{"hdr10_output_content_light_level_missing", nil, func(f *mediainspect.FrameSignals) { f.ContentLightLevel = nil }},
		{"hdr10_output_content_light_level_mismatch", func(s *mediainspect.DetailedStream) { s.ContentLightLevel.MaxFALL = 300 }, nil},
		{"hdr10_output_dolby_vision_present", nil, func(f *mediainspect.FrameSignals) { f.DolbyVisionRPU = true }},
		{"hdr10_output_dolby_vision_present", func(s *mediainspect.DetailedStream) {
			s.SideData = []mediainspect.SideDataRecord{{SideDataType: "DOVI configuration record"}}
		}, nil},
		{"hdr10_output_dynamic_metadata_present", nil, func(f *mediainspect.FrameSignals) { f.HDR10Plus = true }},
		{"hdr10_output_frame_evidence_missing", nil, func(f *mediainspect.FrameSignals) { f.Probed, f.Frames = false, 0 }},
	}
	for _, c := range cases {
		s, f := stream(), frames()
		if c.stream != nil {
			c.stream(&s)
		}
		if c.frames != nil {
			c.frames(&f)
		}
		reasons := hdr10OutputReasons(s, f, testHDR10Plan(), &transcode.HDR10OutputEvidence{})
		found := false
		for _, r := range reasons {
			found = found || r == c.want
		}
		if !found {
			t.Errorf("want %s, got %v", c.want, reasons)
		}
	}
}

func TestBuildLibX265HDR10Args(t *testing.T) {
	plan := &transcode.Plan{VideoCodec: "libx265", Quality: 18, Preset: "slow", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, HDR10: testHDR10Plan()}
	got, err := BuildVideoEncoderArgs(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c:v", "libx265", "-crf", "18", "-preset", "slow", "-x265-params", testHDR10Plan().X265Params(), "-profile:v", "main10", "-pix_fmt", "yuv420p10le", "-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv", "-dolbyvision", "0"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("HDR10 libx265 args\nwant %v\n got %v", want, got)
	}

	abr := *plan
	abr.Quality = 0
	abr.X265 = &transcode.X265Settings{TwoPass: true, BitrateKbps: 8000, Lookahead: 40, BFrames: 6, RefFrames: 4, AQMode: 3, AQStrength: 0.8, PsyRD: 2, PsyRDOQ: 1}
	got, err = BuildVideoEncoderArgs(&abr)
	if err != nil {
		t.Fatal(err)
	}
	pass := withX265Pass(got, 2)
	params := ""
	for i := range pass {
		if pass[i] == "-x265-params" {
			params = pass[i+1]
		}
	}
	if !strings.Contains(params, ":"+testHDR10Plan().X265Params()+":pass=2:stats=x265.log") {
		t.Fatalf("two-pass HDR10 params lost metadata: %s", params)
	}

	for name, mutate := range map[string]func(*transcode.Plan){
		"videotoolbox": func(p *transcode.Plan) { p.VideoCodec, p.Quality, p.Preset = "hevc_videotoolbox", 70, "" },
		"8-bit":        func(p *transcode.Plan) { p.VideoProfile, p.PixelFormat, p.ExpectedBitDepth = "main", "yuv420p", 8 },
		"invalid plan": func(p *transcode.Plan) { p.HDR10 = testHDR10Plan(); p.HDR10.MasterDisplay = "bad" },
	} {
		p := *plan
		p.HDR10 = testHDR10Plan()
		mutate(&p)
		if _, err := BuildVideoEncoderArgs(&p); err == nil {
			t.Errorf("%s: HDR10 must fail closed", name)
		}
	}
}

func TestHDR10CapabilityGate(t *testing.T) {
	if !hasEncoderOption("  -x265-params       <dictionary> E..V.......\n  -dolbyvision       <boolean>    E..V....... Enable Dolby Vision RPU coding", "-dolbyvision") {
		t.Fatal("option not found")
	}
	if hasEncoderOption("  -x265-params       <dictionary> mentions -dolbyvision in text", "-dolbyvision") {
		t.Fatal("option matched inside description")
	}
	caps := transcode.WorkerCapabilities{HDR10Transcode: true, Encoders: map[string]bool{"libx265": true}, Filters: map[string]bool{"lut3d": true}}
	if caps.SupportsHDR10Transcode() {
		t.Fatal("HDR10 needs the verified native 10-bit quality path")
	}
}

func TestBenchmarkMeasurementColorInfo(t *testing.T) {
	rep := &mediainspect.DetailedReport{Video: []mediainspect.DetailedStream{{ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc", PixelFormat: "yuv420p10le", BitDepth: 10}}}
	plan := testHDR10Plan()
	toneMapped := &BenchmarkExecutionEvidence{SourceBitDepth: 10, ToneMap: &transcode.HDRToneMapEvidence{Method: transcode.HDRToneMapMethod, AppliedTo: "reference_and_candidate"}}
	if c := benchmarkMeasurementColorInfo(&BenchmarkRecord{HDR10: plan}, toneMapped, rep); optimization.IsHDR(c) {
		t.Fatalf("tone-mapped HDR10 scores are SDR measurements: %+v", c)
	}
	for name, tc := range map[string]struct {
		record   *BenchmarkRecord
		evidence *BenchmarkExecutionEvidence
	}{
		"no plan":        {&BenchmarkRecord{}, toneMapped},
		"no tone map":    {&BenchmarkRecord{HDR10: plan}, &BenchmarkExecutionEvidence{SourceBitDepth: 10}},
		"other method":   {&BenchmarkRecord{HDR10: plan}, &BenchmarkExecutionEvidence{ToneMap: &transcode.HDRToneMapEvidence{Method: "other", AppliedTo: "reference_and_candidate"}}},
		"candidate only": {&BenchmarkRecord{HDR10: plan}, &BenchmarkExecutionEvidence{ToneMap: &transcode.HDRToneMapEvidence{Method: transcode.HDRToneMapMethod, AppliedTo: "candidate"}}},
	} {
		if c := benchmarkMeasurementColorInfo(tc.record, tc.evidence, rep); !optimization.IsHDR(c) {
			t.Fatalf("%s: HDR scored as SDR without the shared tone map: %+v", name, c)
		}
	}
}
