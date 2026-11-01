package transcodeworker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

const testHDRMasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,50)"

// compactKeepHDRPlan is compactPQPlan frozen for the keep-HDR chain.
func compactKeepHDRPlan() *transcode.CompactPlan {
	p := compactPQPlan()
	p.ChainVersion = transcode.CompactHDRChainVersion
	p.HDR = &transcode.CompactHDROutput{Transfer: transcode.CompactHDRTransferPQ, MasterDisplay: testHDRMasterDisplay, MaxCLL: 1000, MaxFALL: 400}
	return p
}

func TestCompactKeepHDRChainArgsAndEvidence(t *testing.T) {
	useTempCompactLUTDir(t)
	sideData := ",sidedata=mode=delete:type=MASTERING_DISPLAY_METADATA,sidedata=mode=delete:type=CONTENT_LIGHT_LEVEL,sidedata=mode=delete:type=DYNAMIC_HDR_PLUS,sidedata=mode=delete:type=DYNAMIC_HDR_VIVID,sidedata=mode=delete:type=DOVI_RPU_BUFFER,sidedata=mode=delete:type=DOVI_METADATA,sidedata=mode=delete:type=AMBIENT_VIEWING_ENVIRONMENT"
	tag := "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv"
	p := compactKeepHDRPlan()
	got, err := compactVideoFilter(p, "L")
	if want := tag + ",scale=w=1920:h=1080:flags=lanczos+accurate_rnd+full_chroma_int,format=yuv420p10le," + tag + sideData; err != nil || got != want {
		t.Fatalf("keep-HDR chain\n got %s %v\nwant %s", got, err, want)
	}
	// At the source size nothing is rescaled; HLG keeps its transfer.
	same := compactKeepHDRPlan()
	same.MaxHeight, same.TargetWidth, same.TargetHeight = 2160, 3840, 2160
	if got, err := compactVideoFilter(same, ""); err != nil || got != tag+",format=yuv420p10le,"+tag+sideData {
		t.Fatalf("4K chain %s %v", got, err)
	}
	hlg := compactHLGPlan()
	hlg.ChainVersion, hlg.HDR = transcode.CompactHDRChainVersion, &transcode.CompactHDROutput{Transfer: transcode.CompactHDRTransferHLG}
	if got, err := compactVideoFilter(hlg, ""); err != nil || !strings.HasPrefix(got, "setparams=color_primaries=bt2020:color_trc=arib-std-b67:") || strings.Contains(got, "lut3d") {
		t.Fatalf("HLG chain %s %v", got, err)
	}

	for _, x265 := range []*transcode.X265Settings{nil, {BitrateKbps: 8000, TwoPass: true, Lookahead: 20, BFrames: 4, RefFrames: 3, AQMode: 2, AQStrength: 1, PsyRD: 1, PsyRDOQ: 1}} {
		plan := &transcode.Plan{VideoCodec: transcode.VideoCodecLibX265, Quality: 22, Preset: "medium", VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, Compact: p, X265: x265}
		if x265 != nil {
			plan.Quality = 0
		}
		args, err := BuildVideoEncoderArgs(plan)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		for _, want := range []string{"-filter:v:0 " + tag, "-color_primaries:v:0 bt2020", "-color_trc:v:0 smpte2084", "-colorspace:v:0 bt2020nc", "-color_range:v:0 tv", "-dolbyvision:v:0 0", "-pix_fmt yuv420p10le", "-profile:v main10",
			"repeat-headers=1:hdr10=1:hdr10-opt=1:master-display=" + testHDRMasterDisplay + ":max-cll=1000,400"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("missing %q in %s", want, joined)
			}
		}
		// FFmpeg keeps only the last -x265-params: there must be one.
		if strings.Count(joined, "-x265-params") != 1 || strings.Contains(joined, "lut3d") || strings.Contains(joined, "bt709") {
			t.Fatalf("keep-HDR args %s", joined)
		}
		if x265 != nil && !strings.Contains(joined, "rdoq-level=2:repeat-headers=1") {
			t.Fatalf("HDR params not merged into the encoder's: %s", joined)
		}
	}
	vt := &transcode.Plan{VideoCodec: "hevc_videotoolbox", Quality: 60, VideoProfile: "main10", PixelFormat: "p010le", ExpectedBitDepth: 10, Compact: p}
	if _, err := BuildVideoEncoderArgs(vt); err == nil {
		t.Fatal("keep-HDR VideoToolbox encode accepted")
	}
	if got := compactX265Params(hlg.HDR); got != "repeat-headers=1" {
		t.Fatalf("HLG params %s", got)
	}
	if got := compactX265Params(&transcode.CompactHDROutput{Transfer: transcode.CompactHDRTransferPQ}); got != "repeat-headers=1:hdr10=1:hdr10-opt=1" {
		t.Fatalf("PQ10 params %s", got)
	}

	// Measurement: the reference through the SDR chain of the source, the
	// candidate through the same LUT at its own size.
	ref, cand, err := compactMeasurementChains(p)
	if err != nil {
		t.Fatal(err)
	}
	sdrRef, err := compactFilterForRun(compactPQPlan())
	if err != nil || ref != sdrRef {
		t.Fatalf("reference chain %s, want %s (%v)", ref, sdrRef, err)
	}
	lut := "lut3d=file=" + escapeFFmpegFilterPath(compactLUTPath(p)) + ":interp"
	if !strings.Contains(cand, lut) || !strings.Contains(cand, "scale=w=1920:h=1080:in_color_matrix=bt2020") || !strings.HasSuffix(cand, "range=tv"+sideData) {
		t.Fatalf("candidate chain %s", cand)
	}
	if ref, cand, err := compactMeasurementChains(compactPQPlan()); err != nil || ref != sdrRef || cand != "" {
		t.Fatalf("SDR plan measures its candidate as encoded: %q %q %v", ref, cand, err)
	}

	e, err := compactChainEvidence(p, "sha256:abc")
	if err != nil || e.ChainVersion != transcode.CompactHDRChainVersion || e.AppliedTo != "reference_and_candidate" || e.MeasurementSpace != compactHDRMeasurementSpace || e.ToneMapMethod != transcode.CompactToneMapPQ {
		t.Fatalf("evidence %+v %v", e, err)
	}
	if !strings.Contains(e.ReferenceChain, "lut3d=file=compact.cube:") || !strings.Contains(e.CandidateChain, "lut3d=file=compact.cube:") || strings.Contains(e.CandidateChain+e.ReferenceChain, compactLUTDir()) {
		t.Fatalf("evidence chains %s / %s", e.ReferenceChain, e.CandidateChain)
	}
	if e, _ := compactChainEvidence(compactPQPlan(), "sha256:abc"); e.CandidateChain != "" || e.AppliedTo != "encoder_input_and_reference" {
		t.Fatalf("SDR evidence changed: %+v", e)
	}
}

func TestCheckCompactKeepHDROutput(t *testing.T) {
	p := compactKeepHDRPlan()
	md := &mediainspect.MasteringDisplayMetadata{RedX: "34000/50000", RedY: "16000/50000", GreenX: "13250/50000", GreenY: "34500/50000", BlueX: "7500/50000", BlueY: "3000/50000", WhitePointX: "15635/50000", WhitePointY: "16450/50000", MinLuminance: "50/10000", MaxLuminance: "10000000/10000"}
	clean := mediainspect.DetailedStream{Kind: "video", Width: 1920, Height: 1080, BitDepth: 10, ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc", ColorRange: "tv"}
	frames := mediainspect.FrameSignals{Probed: true, Frames: 8, ColorPrimaries: "bt2020", ColorTransfer: "smpte2084", ColorSpace: "bt2020nc", ColorRange: "tv",
		MasteringDisplay: md, ContentLightLevel: &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 400}}
	if e := checkCompactOutput(clean, frames, p); e.Verdict != "pass" || e.MasterDisplay != testHDRMasterDisplay || e.MaxCLL != 1000 || e.MaxFALL != 400 {
		t.Fatalf("clean HDR output rejected: %+v", e)
	}
	// A container copy of the metadata is fine when it matches.
	withStream := clean
	withStream.MasteringDisplay, withStream.ContentLightLevel = md, frames.ContentLightLevel
	if e := checkCompactOutput(withStream, frames, p); e.Verdict != "pass" {
		t.Fatalf("matching container metadata rejected: %+v", e)
	}
	brighter := *md
	brighter.MaxLuminance = "40000000/10000"
	cases := map[string]struct {
		mutate func(*mediainspect.DetailedStream, *mediainspect.FrameSignals)
		reason string
	}{
		"size":          {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.Height = 2160 }, "compact_output_size_mismatch"},
		"8-bit":         {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.BitDepth = 8 }, "compact_output_hdr_not_10bit"},
		"sdr stream":    {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.ColorTransfer = "bt709" }, "compact_output_stream_not_hdr"},
		"hlg frames":    {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.ColorTransfer = "arib-std-b67" }, "compact_output_frames_not_hdr"},
		"untagged":      {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.ColorPrimaries = "" }, "compact_output_frames_not_hdr"},
		"no mastering":  {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.MasteringDisplay = nil }, "compact_output_master_display_mismatch"},
		"other master":  {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.MasteringDisplay = &brighter }, "compact_output_master_display_mismatch"},
		"stream master": {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) { v.MasteringDisplay = &brighter }, "compact_output_master_display_mismatch"},
		"no cll":        {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.ContentLightLevel = nil }, "compact_output_content_light_level_mismatch"},
		"other cll": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) {
			f.ContentLightLevel = &mediainspect.ContentLightLevelMetadata{MaxCLL: 1000, MaxFALL: 300}
		}, "compact_output_content_light_level_mismatch"},
		"hdr10plus": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.HDR10Plus = true }, "compact_output_dynamic_hdr_present"},
		"dovi rpu":  {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) { f.DolbyVisionRPU = true }, "compact_output_dynamic_hdr_present"},
		"dovi config rec": {func(v *mediainspect.DetailedStream, _ *mediainspect.FrameSignals) {
			v.SideData = []mediainspect.SideDataRecord{dovi8Record()}
		}, "compact_output_dynamic_hdr_present"},
		"vivid": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) {
			f.DynamicMetadata = []string{"HDR Vivid"}
		}, "compact_output_dynamic_hdr_present"},
		"inconsistent": {func(_ *mediainspect.DetailedStream, f *mediainspect.FrameSignals) {
			f.Inconsistent = []string{"mastering_display"}
		}, "compact_output_frames_inconsistent"},
	}
	for name, c := range cases {
		v, f := clean, frames
		c.mutate(&v, &f)
		if e := checkCompactOutput(v, f, p); e.Verdict != "fail" || !containsString(e.ReasonCodes, c.reason) {
			t.Errorf("%s: %+v", name, e)
		}
	}
	// Without frozen static metadata the output must carry none; 0,0 light
	// levels mean none.
	bare := compactKeepHDRPlan()
	bare.SourceClass, bare.HDR.MasterDisplay, bare.HDR.MaxCLL, bare.HDR.MaxFALL = mediainspect.DynamicRangePQ10, "", 0, 0
	unknown := frames
	unknown.MasteringDisplay, unknown.ContentLightLevel = nil, &mediainspect.ContentLightLevelMetadata{}
	if e := checkCompactOutput(clean, unknown, bare); e.Verdict != "pass" {
		t.Fatalf("bare PQ output rejected: %+v", e)
	}
	if e := checkCompactOutput(clean, frames, bare); e.Verdict != "fail" {
		t.Fatal("metadata the plan did not freeze was accepted")
	}
	// An SDR output never satisfies a keep-HDR plan, nor HDR an SDR plan.
	sdrOut := mediainspect.DetailedStream{Kind: "video", Width: 1920, Height: 1080, BitDepth: 10, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorSpace: "bt709", ColorRange: "tv"}
	sdrFrames := mediainspect.FrameSignals{Probed: true, Frames: 8, ColorPrimaries: "bt709", ColorTransfer: "bt709", ColorSpace: "bt709", ColorRange: "tv"}
	if e := checkCompactOutput(sdrOut, sdrFrames, p); e.Verdict != "fail" {
		t.Fatal("SDR output passed a keep-HDR plan")
	}
	if e := checkCompactOutput(clean, frames, compactPQPlan()); e.Verdict != "fail" {
		t.Fatal("HDR output passed an SDR plan")
	}
}

func TestValidatePlanCompactKeepHDR(t *testing.T) {
	if err := ValidatePlan(compactModePlan(t, compactKeepHDRPlan())); err != nil {
		t.Fatal(err)
	}
	resign := func(p *transcode.Plan) *transcode.Plan { p.PlanDigest, _ = transcode.DigestPlan(p); return p }
	vt := compactModePlan(t, compactKeepHDRPlan())
	vt.VideoCodec, vt.Quality, vt.Preset = "hevc_videotoolbox", 60, ""
	main8 := compactModePlan(t, compactKeepHDRPlan())
	main8.VideoProfile, main8.PixelFormat, main8.ExpectedBitDepth = "main", "yuv420p", 8
	unknown := compactKeepHDRPlan()
	unknown.ChainVersion = "compact-hdr-v2"
	for name, p := range map[string]*transcode.Plan{"videotoolbox": resign(vt), "main": resign(main8), "unknown chain": compactModePlan(t, unknown)} {
		if err := ValidatePlan(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestRealCompactKeepHDREncode encodes synthetic HDR10 (4K to 1080p and at
// 4K) and Dolby Vision 8.1 sources with the production keep-HDR arguments.
// The output must reclassify as HDR10 with the source's static metadata and
// no Dolby Vision, and pass the output check.
func TestRealCompactKeepHDREncode(t *testing.T) {
	ctx, ffmpeg, ffprobe := compactRealEnv(t)
	if ok, err := libX265HasDolbyVisionOption(ctx, ffmpeg); err != nil || !ok {
		t.Skipf("libx265 without -dolbyvision: %v", err)
	}
	if err := probeCompactHDRChain(ctx, ffmpeg); err != nil {
		t.Fatalf("keep-HDR probe: %v", err)
	}
	encode := func(t *testing.T, dir, source string, maxHeight int) (*transcode.CompactPlan, mediainspect.DynamicRange, string, mediainspect.DetailedReport) {
		rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
		if err != nil {
			t.Fatal(err)
		}
		d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
		if err != nil {
			t.Fatal(err)
		}
		c, err := transcode.NewCompactPlanForChain(transcode.CompactHDRChainVersion, d, rep.MainVideo()[0], maxHeight)
		if err != nil {
			t.Fatalf("plan for %+v: %v", d, err)
		}
		plan := compactModePlanFor(t, c, rep.MainVideo()[0].BitDepth)
		if err := ValidatePlan(plan); err != nil {
			t.Fatal(err)
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
		candidate := filepath.Join(dir, "candidate.mkv")
		args, err := BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
		if err != nil {
			t.Fatal(err)
		}
		runFFmpeg(t, ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...)
		out, err := mediainspect.InspectDetailed(ctx, ffprobe, candidate)
		if err != nil {
			t.Fatal(err)
		}
		return c, d, candidate, out
	}
	check := func(t *testing.T, c *transcode.CompactPlan, candidate string, out mediainspect.DetailedReport, w, h int) {
		e := validateCompactOutput(ctx, ffprobe, candidate, &out, c)
		if e.Verdict != "pass" || e.Width != w || e.Height != h || e.MasterDisplay != testHDRMasterDisplay || e.MaxCLL != 1000 || e.MaxFALL != 400 {
			t.Fatalf("output check: %+v", e)
		}
		v := out.MainVideo()[0]
		if v.Codec != "hevc" || v.BitDepth != 10 || mediainspect.HasDolbyVision(v) || v.Tags["title"] != "" {
			t.Fatalf("output video %+v", v)
		}
		od, err := classifySourceDynamicRange(ctx, ffprobe, candidate, &out)
		if err != nil || od.Class != mediainspect.DynamicRangeHDR10 || od.DolbyVision != nil || od.HDR10Plus {
			t.Fatalf("output classified %+v %v", od, err)
		}
		// The output is itself a source the keep-HDR chain accepts, with the
		// same static metadata.
		if again, err := transcode.NewCompactPlanForChain(transcode.CompactHDRChainVersion, od, v, c.MaxHeight); err != nil || *again.HDR != *c.HDR {
			t.Fatalf("output replans as %+v %v", again, err)
		}
	}

	t.Run("hdr10 to 1080p", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "source.mkv")
		compactRealSource(t, ctx, ffmpeg, source, true)
		c, _, candidate, out := encode(t, dir, source, 1080)
		check(t, c, candidate, out, 1920, 1080)
		if len(out.Audio) != 2 || len(out.Subtitles) != 1 {
			t.Fatalf("tracks lost: %d audio, %d subtitles", len(out.Audio), len(out.Subtitles))
		}
	})
	t.Run("hdr10 at 4K", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "source.mkv")
		compactRealSource(t, ctx, ffmpeg, source, true)
		c, _, candidate, out := encode(t, dir, source, 2160)
		check(t, c, candidate, out, 3840, 2160)
	})
	t.Run("dolby vision 8.1 becomes hdr10", func(t *testing.T) {
		dir := t.TempDir()
		base := filepath.Join(dir, "base.mp4")
		runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=1",
			"-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
			"-c:v", "libx265", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p10le", "-tag:v", "hvc1",
			"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:repeat-headers=1:master-display="+testHDRMasterDisplay+":max-cll=1000,400:log-level=error",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv",
			"-metadata:s:v:0", "title=HEVC Dolby Vision", base)
		raw, err := os.ReadFile(base)
		if err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(dir, "source.mp4")
		if err := os.WriteFile(source, withDolbyVisionConfig(t, raw), 0o600); err != nil {
			t.Fatal(err)
		}
		c, d, candidate, out := encode(t, dir, source, 1080)
		if d.Class != mediainspect.DynamicRangeDolbyVision || c.DolbyVisionProfile != 8 {
			t.Fatalf("source %+v plan %+v", d, c)
		}
		check(t, c, candidate, out, 1920, 1080)
	})
	t.Run("hlg", func(t *testing.T) {
		dir := t.TempDir()
		source := filepath.Join(dir, "source.mkv")
		runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=size=3840x2160:rate=24:duration=1",
			"-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=arib-std-b67:colorspace=bt2020nc:range=tv",
			"-c:v", "libx265", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error",
			"-color_primaries", "bt2020", "-color_trc", "arib-std-b67", "-colorspace", "bt2020nc", "-color_range", "tv", source)
		c, _, candidate, out := encode(t, dir, source, 1080)
		if e := validateCompactOutput(ctx, ffprobe, candidate, &out, c); e.Verdict != "pass" || e.MasterDisplay != "" {
			t.Fatalf("HLG output check: %+v", e)
		}
		if od, err := classifySourceDynamicRange(ctx, ffprobe, candidate, &out); err != nil || od.Class != mediainspect.DynamicRangeHLG {
			t.Fatalf("output classified %+v %v", od, err)
		}
	})
}

// TestCompactKeepHDRWorkerQualityEndToEnd runs final validation with libvmaf
// on a keep-HDR plan: both the HDR10 source and the HDR10 candidate pass
// through the same tone map before scoring, and the capability probe
// advertises the chain.
func TestCompactKeepHDRWorkerQualityEndToEnd(t *testing.T) {
	ffmpeg := requireLibvmaf(t)
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	model := hfrTestModel(t, ctx, ffmpeg)
	shim := qualityShim(t, ffmpeg, model, false)
	useTempCompactLUTDir(t)
	dir := t.TempDir()
	caps, err := ProbeWorkerCapabilitiesWithScratch(ctx, shim, dir)
	if err != nil || !caps.SupportsHFRQuality() || !caps.Encoders["libx265"] || !caps.SupportsCompactTranscode() {
		t.Skipf("decimated VMAF, libx265 or the compact chain unavailable: %v", err)
	}
	if ok, _ := libX265HasDolbyVisionOption(ctx, ffmpeg); ok != caps.CompactKeepHDR {
		t.Fatalf("compact_keep_hdr=%v with libx265 -dolbyvision=%v", caps.CompactKeepHDR, ok)
	}
	if !caps.CompactKeepHDR {
		t.Skip("libx265 cannot run the keep-HDR chain")
	}
	plain := caps
	plain.CompactKeepHDR = false
	if !caps.SupportsCompactPlan(compactKeepHDRPlan()) || plain.SupportsCompactPlan(compactKeepHDRPlan()) || !plain.SupportsCompactPlan(compactPQPlan()) {
		t.Fatal("SupportsCompactPlan does not follow compact_keep_hdr")
	}
	if caps.Fingerprint() != plain.Fingerprint() {
		t.Fatal("compact_keep_hdr changed the capability fingerprint")
	}

	source := filepath.Join(dir, "source.mkv")
	runFFmpeg(t, ctx, ffmpeg, "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=50:duration=2",
		"-vf", "format=yuv420p10le,setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "12", "-pix_fmt", "yuv420p10le",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited:hdr10=1:repeat-headers=1:master-display="+testHDRMasterDisplay+":max-cll=1000,400:log-level=error",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv", source)
	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := classifySourceDynamicRange(ctx, ffprobe, source, &rep)
	if err != nil || d.Class != mediainspect.DynamicRangeHDR10 {
		t.Fatalf("source classified %+v %v", d, err)
	}
	c, err := transcode.NewCompactPlanForChain(transcode.CompactHDRChainVersion, d, rep.MainVideo()[0], 720)
	if err != nil {
		t.Fatal(err)
	}
	threshold, limit := 50.0, 100
	policy := transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h", Minimum: 60, Target: 70, WorstWindowSeconds: 0.5, FrameThreshold: &threshold, MaxFramesBelowThreshold: &limit, GuardrailEnforcement: "reject"}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}
	plan := compactModePlanFor(t, c, rep.MainVideo()[0].BitDepth)
	plan.Quality = 18
	plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "vmaf", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 0.5, DurationSeconds: 1}}, Quality: policy, BenchmarkRequestDigest: "sha256:" + strings.Repeat("1", 64)}
	plan.PlanDigest = ""
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	worker := func(name string) *Worker {
		return NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, name), AllowedRoots: []string{dir}, FFmpeg: shim, FFprobe: ffprobe, MaxParallelJobs: 1})
	}
	if err := worker("pre").validateModeSourceQuality(ctx, source, plan); err != nil {
		t.Fatalf("keep-HDR source refused: %v", err)
	}

	probe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.mkv")
	args, err := BuildFFmpegArgs(ep, source, candidate, filepath.Join(dir, "progress"))
	if err != nil {
		t.Fatal(err)
	}
	runFFmpeg(t, ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...)
	jobDir := filepath.Join(dir, "final")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := worker("final").validateFinalPerceptual(ctx, jobDir, source, candidate, plan, "sha256:candidate", 1)
	if err != nil || e == nil || e.Verdict != "pass" || e.VMAF == nil || e.VMAF.Mean < 60 {
		t.Fatalf("final keep-HDR validation: %+v %v", e, err)
	}
	if o := e.CompactOutput; o == nil || o.Verdict != "pass" || o.MasterDisplay != testHDRMasterDisplay || o.MaxCLL != 1000 {
		t.Fatalf("final output evidence %+v", o)
	}
	if ch := e.CompactChain; ch == nil || ch.ChainVersion != transcode.CompactHDRChainVersion || ch.AppliedTo != "reference_and_candidate" || ch.CandidateChain == "" {
		t.Fatalf("final chain evidence %+v", ch)
	}

	// An SDR candidate does not satisfy a keep-HDR plan, whatever it scores.
	sdrPlan := compactModePlanFor(t, c.MeasurementPlan(), rep.MainVideo()[0].BitDepth)
	sdrPlan.Quality = 18
	sdrPlan.PlanDigest, _ = transcode.DigestPlan(sdrPlan)
	sdrEP, err := BuildExecutionPlan(sdrPlan, probe.Streams, probe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	sdrCandidate := filepath.Join(dir, "sdr.mkv")
	if args, err = BuildFFmpegArgs(sdrEP, source, sdrCandidate, filepath.Join(dir, "progress-sdr")); err != nil {
		t.Fatal(err)
	}
	runFFmpeg(t, ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, args...)...)
	if e, err := worker("final-sdr").validateFinalPerceptual(ctx, filepath.Join(dir, "final-sdr-job"), source, sdrCandidate, plan, "sha256:sdr", 1); err == nil && e != nil && e.Verdict == "pass" {
		t.Fatal("an SDR candidate passed a keep-HDR plan")
	}
}
