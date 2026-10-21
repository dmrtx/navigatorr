package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// sizeV3TestPlan is a complete size v3 mode plan: frozen savings and quality
// policy, lossless audio compaction and, when main10, an 8-bit source
// encoded as Main10.
func sizeV3TestPlan(main10 bool) *transcode.Plan {
	p := &transcode.Plan{Container: "mkv", VideoCodec: "libx265", Quality: 24, Preset: "medium", VideoProfile: "main", PixelFormat: "yuv420p", ExpectedBitDepth: 8, AudioMode: transcode.AudioModeCompactLossless, SubtitleMode: "preserve", PreserveMetadata: true, PreserveChapters: true, PreserveAttachments: true, RecipeVersion: "test", RecipeDigest: "sha256:" + strings.Repeat("0", 64), Resilience: transcode.ResiliencePlan{MaxAttempts: 1},
		Mode: "size", ModePolicyVersion: transcode.ModePolicyV3, PolicyDigest: "sha256:" + strings.Repeat("1", 64), SizePolicy: &transcode.SizePolicy{MinSavingsPercent: 15},
		QualityValidation: &transcode.QualityValidationPlan{Metric: "vmaf", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 5, DurationSeconds: 10}}, Quality: transcode.BenchmarkQualityConfig{VMAF: &transcode.BenchmarkQualityThresholds{Model: "v1_1080p_3h"}, FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64)}}
	if main10 {
		p.VideoProfile, p.PixelFormat, p.ExpectedBitDepth = "main10", "p010le", 10
	}
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

func TestSizeV3PlanAudioPolicy(t *testing.T) {
	if err := ValidatePlan(sizeV3TestPlan(true)); err != nil {
		t.Fatalf("size v3 plan rejected: %v", err)
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"v3 size copying audio":    func(p *transcode.Plan) { p.AudioMode = "copy" },
		"v2 size compacting audio": func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV2 },
		"v3 quality mode":          func(p *transcode.Plan) { p.Mode = "quality" },
		"profile plan compacting lossless": func(p *transcode.Plan) {
			p.Mode, p.ModePolicyVersion, p.SizePolicy, p.PolicyDigest, p.QualityValidation = "", "", nil, "", nil
		},
	} {
		p := sizeV3TestPlan(false)
		mutate(p)
		p.PlanDigest, _ = transcode.DigestPlan(p)
		if err := ValidatePlan(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestModeCandidateBitDepth(t *testing.T) {
	for _, tc := range []struct {
		plan   *transcode.Plan
		source int
		want   int
	}{
		{sizeV3TestPlan(true), 8, 10},
		{sizeV3TestPlan(false), 8, 8},
		{sizeV3TestPlan(true), 10, 10},
		{&transcode.Plan{Mode: "size", ModePolicyVersion: transcode.ModePolicyV2, ExpectedBitDepth: 10}, 8, 8},
		{&transcode.Plan{Mode: "quality", ModePolicyVersion: transcode.ModePolicyV3, ExpectedBitDepth: 10}, 8, 8},
	} {
		if got := modeCandidateBitDepth(tc.plan, tc.source); got != tc.want {
			t.Errorf("%s/%s depth %d from %d-bit: %d, want %d", tc.plan.Mode, tc.plan.ModePolicyVersion, tc.plan.ExpectedBitDepth, tc.source, got, tc.want)
		}
	}
}

// A real size v3 encode: an 8-bit 1080p source with a 5.1 PCM track and an
// AC-3 track becomes Main10 HEVC, the PCM track AAC 5.1 and the AC-3 track a
// copy, and it passes pre-publish and sampled final validation.
func TestSizeV3RealEncodeMain10WithPCM(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] || !available["libx264"] || !available["aac"] || !available["ac3"] {
		t.Skip("libx265/libx264/AAC/AC-3 unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	args := []string{"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=12",
		"-f", "lavfi", "-i", "aevalsrc=sin(440*2*PI*t)|sin(550*2*PI*t)|sin(660*2*PI*t)|sin(110*2*PI*t)|sin(770*2*PI*t)|sin(880*2*PI*t):s=48000:c=5.1:d=12",
		"-f", "lavfi", "-i", "sine=frequency=330:sample_rate=48000:duration=12",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p",
		"-c:a:0", "pcm_s24le", "-c:a:1", "ac3", "-ac:a:1", "2", "-b:a:1", "192k",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:0", "title=LPCM 24-bit 5.1", "-metadata:s:a:1", "language=jpn",
		"-disposition:a:0", "default", "-disposition:a:1", "0", source}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := sizeV3TestPlan(true)
	plan.Preset = "ultrafast"
	windows := transcode.PlanFinalValidationWindows(srcProbe.DurationSec, 1, 2.4, 5, 12)
	if len(windows) != 5 {
		t.Fatalf("planned windows %+v", windows)
	}
	plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: windows, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64)}
	plan.PlanDigest, _ = transcode.DigestPlan(plan)
	ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Conversions) != 1 || ep.Conversions[0].FromCodec != "pcm_s24le" || ep.Conversions[0].ToCodec != "aac" {
		t.Fatalf("conversions %+v", ep.Conversions)
	}
	jobDir := filepath.Join(dir, "job")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(jobDir, "candidate.mkv")
	if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
		log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
		t.Fatalf("encode: %v\n%s", err, log)
	}
	candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	video := firstMainVideo(candProbe.Streams)
	if video == nil || video.Codec != "hevc" || video.BitDepth != 10 || video.Width != 1920 || video.Height != 1080 {
		t.Fatalf("video not 1080p Main10: %+v", video)
	}
	audio := streamsOfKind(candProbe.Streams, "audio")
	if len(audio) != 2 || audio[0].Codec != "aac" || audio[0].Channels != 6 || !transcode.SameAudioLayout("5.1", audio[0].Layout) || audio[0].Language != "eng" || audio[0].Disposition["default"] != 1 || audio[1].Codec != "ac3" || audio[1].Language != "jpn" {
		t.Fatalf("audio policy mismatch: %+v", audio)
	}
	if audio[0].Title != "AAC 5.1" {
		t.Fatalf("converted title %q", audio[0].Title)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, srcProbe, 0); err != nil {
		t.Fatalf("pre-publish validation: %v", err)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := w.validateFinalPerceptual(ctx, jobDir, source, candidate, plan, "sha256:candidate", info.Size())
	if err != nil || evidence == nil || evidence.Verdict != "pass" || evidence.SSIMMean == nil || evidence.SourceBitDepth != 8 || evidence.CandidateBitDepth != 10 {
		t.Fatalf("sampled final validation: %+v %v", evidence, err)
	}
	// Outside size v3 a Main10 candidate of an 8-bit source is a changed
	// measurement class and never publishes.
	v2 := *plan
	v2.ModePolicyVersion = transcode.ModePolicyV2
	if e, err := w.validateFinalPerceptual(ctx, jobDir, source, candidate, &v2, "sha256:candidate", info.Size()); err == nil || e == nil || e.Verdict == "pass" {
		t.Fatalf("v2 accepted an 8-bit to Main10 candidate: %+v %v", e, err)
	}
}

// Size mode converts lossless object audio to AAC on its channel bed, whether
// the profile, title or handler names it (TrueHD Atmos 7.1 at 512k, DTS:X
// 5.1 at 384k), and copies E-AC-3 Atmos. Language, dispositions and every
// track are kept; the converted titles and handlers no longer name Atmos or
// DTS:X. The validator agrees with the plan both ways.
func TestSizeV3ConvertsLosslessObjectAudio(t *testing.T) {
	plan := sizeV3TestPlan(true)
	src := []SourceStream{
		{Index: 0, Kind: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8},
		{Index: 1, TypeIndex: 0, Kind: "audio", Codec: "truehd", Profile: "Dolby TrueHD + Dolby Atmos", Title: "English - TrueHD Atmos 7.1", Channels: 8, Layout: "7.1", Language: "eng", Disposition: map[string]int{"default": 1}},
		{Index: 2, TypeIndex: 1, Kind: "audio", Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Title: "DD+ Atmos 5.1", Channels: 6, Layout: "5.1(side)", Language: "spa"},
		{Index: 3, TypeIndex: 2, Kind: "audio", Codec: "dts", Profile: "DTS-HD MA", Title: "Commentary", Handler: "DTS:X", Channels: 6, Layout: "5.1(side)", Language: "ita", Disposition: map[string]int{"comment": 1}},
	}
	ep, err := BuildExecutionPlan(plan, src, 600)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, s := range ep.Streams {
		if s.Kind == "audio" {
			targets = append(targets, fmt.Sprintf("%s/%d", s.TargetCodec, s.BitrateKbps))
		}
	}
	if strings.Join(targets, ",") != "aac/512,copy/0,aac/384" {
		t.Fatalf("audio targets %v", targets)
	}
	if len(ep.Conversions) != 2 {
		t.Fatalf("conversions %+v", ep.Conversions)
	}
	for _, c := range ep.Conversions {
		if !strings.Contains(c.Reason, "object and height metadata dropped") {
			t.Fatalf("conversion reason hides the object audio loss: %q", c.Reason)
		}
	}
	args, err := BuildFFmpegArgs(ep, "/media/movies/Movie/Movie.mkv", "/tmp/candidate.mkv", "/tmp/progress")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c:a:0 aac -b:a:0 512k", "-metadata:s:a:0 title=English AAC 7.1", "-disposition:a:0 default", "-c:a:2 aac -b:a:2 384k", "-metadata:s:a:2 handler_name=", "-disposition:a:2 comment"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	for _, unwanted := range []string{"-c:a:1 ", "-metadata:s:a:1 ", "-metadata:s:a:0 handler_name=", "-metadata:s:a:2 title="} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("unexpected %q in %s", unwanted, joined)
		}
	}
	converted := append([]SourceStream(nil), src...)
	converted[1].Codec, converted[1].Profile, converted[1].Title = "aac", "LC", "English AAC 7.1"
	converted[3].Codec, converted[3].Profile, converted[3].Handler = "aac", "LC", ""
	if err := validateAudioPreservation(plan, src, converted); err != nil {
		t.Fatalf("converted object audio rejected: %v", err)
	}
	copied := append([]SourceStream(nil), converted...)
	copied[1] = src[1]
	if err := validateAudioPreservation(plan, src, copied); err == nil {
		t.Fatal("copied TrueHD Atmos passed a plan that converts it")
	}
	flattened := append([]SourceStream(nil), converted...)
	flattened[2].Codec = "aac"
	if err := validateAudioPreservation(plan, src, flattened); err == nil {
		t.Fatal("converted E-AC-3 Atmos passed validation")
	}
}
