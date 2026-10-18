package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func joyPlan(t *testing.T) *transcode.Plan {
	t.Helper()
	return joyPlanFromFile(t, "../../docs/transcode/examples/transcode-profile-joy.json")
}

func joyPlanFromFile(t *testing.T, path string) *transcode.Plan {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p recipe.Profile
	if err = json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	p, _, err = recipe.NormalizeAndDigestProfile("joy-1080p-experimental", p)
	if err != nil {
		t.Fatal(err)
	}
	s, err := recipe.Parse(recipe.EmbeddedBytes())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := recipe.Resolve(s, "joy-1080p-experimental", map[string]recipe.Profile{"joy-1080p-experimental": p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestJoyPlanKeepsExactSettingsAndIdentity(t *testing.T) {
	p := joyPlan(t)
	if err := ValidatePlan(p); err != nil {
		t.Fatal(err)
	}
	args, err := BuildVideoEncoderArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(args, " ")
	for _, want := range []string{"-b:v 1200k", "-pix_fmt yuv420p10le", "rc-lookahead=80", "bframes=6", "aq-strength=0.8", "psy-rd=0", "ssim-rd=1"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q: %s", want, s)
		}
	}
	if strings.Contains(s, "-crf") {
		t.Fatal("ABR accidentally became CRF")
	}
	old := p.PlanDigest
	p.X265.Lookahead = 79
	if digest, _ := transcode.DigestPlan(p); digest == old {
		t.Fatal("x265 settings missing from immutable identity")
	}
	p.X265.BitrateKbps = -1
	if _, err := BuildVideoEncoderArgs(p); err == nil {
		t.Fatal("invalid settings accepted")
	}
}

func TestJoyTwoPassFailureNeverStartsSecondPass(t *testing.T) {
	d := t.TempDir()
	calls := filepath.Join(d, "calls")
	stub := filepath.Join(d, "ffmpeg")
	script := "#!/bin/sh\necho invoked >> '" + calls + "'\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := joyPlan(t)
	out := filepath.Join(d, "output.mkv")
	args, err := BuildCandidateEncodeArgs(filepath.Join(d, "reference.mkv"), out, p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), stub, args...)
	if err := runEncoderCommand(context.Background(), cmd, args, p, out); err == nil {
		t.Fatal("first pass failure ignored")
	}
	b, _ := os.ReadFile(calls)
	if string(b) != "invoked\n" {
		t.Fatalf("unexpected calls: %s", b)
	}
	if files, _ := filepath.Glob(filepath.Join(d, ".x265-stats-*")); len(files) != 0 {
		t.Fatal("private stats leaked")
	}
}

func TestJoyCancellationBeforeFinalPassPreservesSource(t *testing.T) {
	d := t.TempDir()
	stub := filepath.Join(d, "ffmpeg")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	p := joyPlan(t)
	source := filepath.Join(d, "source.mkv")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(d, "candidate.mkv")
	args, err := BuildCandidateEncodeArgs(source, out, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var phases []string
	ctx = context.WithValue(ctx, encoderPhaseContextKey{}, func(phase string) error {
		phases = append(phases, phase)
		if phase == "encoding" {
			cancel()
			return context.Canceled
		}
		return nil
	})
	cmd := exec.CommandContext(ctx, stub, args...)
	if err := runEncoderCommand(ctx, cmd, args, p, out); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	if strings.Join(phases, ",") != "analysing_video,encoding" {
		t.Fatalf("incorrect phase order: %v", phases)
	}
	if b, _ := os.ReadFile(source); string(b) != "original" {
		t.Fatal("source changed")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("cancelled output exists")
	}
}

func TestJoyFullEncodeUsesHEAACAndPreservesTracks(t *testing.T) {
	p := joyPlan(t)
	ep, err := BuildExecutionPlan(p, []SourceStream{{Kind: "video", Codec: "h264", Index: 0}, {Kind: "audio", Codec: "eac3", Index: 1, Channels: 6, Disposition: map[string]int{"default": 1}}}, 20)
	if err != nil {
		t.Fatal(err)
	}
	args, err := BuildFFmpegArgs(ep, "/tmp/source.mkv", "/tmp/candidate.mkv", "/tmp/progress")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Join(args, " ")
	for _, want := range []string{"-map 0:a?", "-c:a:0 aac_at", "-profile:a:0 4", "-b:a:0 160k", "-disposition:a:0 default", "-map_metadata 0", "-map_chapters 0"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q: %s", want, s)
		}
	}
}

func TestJoyBenchmarkPipelineExecutesBothPasses(t *testing.T) {
	d := t.TempDir()
	ffmpeg, probe, calls := setupMockTools(t, d, sdr8BitProbeJSON, "")
	script, err := os.ReadFile(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), " V..... ffv1", " V..... libx265              HEVC\n V..... ffv1", 1))
	if err := os.WriteFile(ffmpeg, script, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(d, "source.mkv")
	if err := os.WriteFile(source, make([]byte, 100000), 0600); err != nil {
		t.Fatal(err)
	}
	p := joyPlan(t)
	w := NewWorker(&WorkerConfig{StateDir: filepath.Join(d, "state"), AllowedRoots: []string{d}, FFmpeg: ffmpeg, FFprobe: probe})
	record := &BenchmarkRecord{ID: "bench-joy-pipeline", Status: "running", Source: source, Metric: "ssim", Samples: []transcode.BenchmarkSampleWindow{{Index: 0, StartSeconds: 10, DurationSeconds: 5}}, Candidates: []transcode.BenchmarkCandidate{{ID: "joy1200", VideoCodec: "libx265", Preset: "slow", X265: p.X265.Clone(), VideoProfile: "main", PixelFormat: "yuv420p"}}}
	if err := (&ProductionBenchmarkRunner{}).RunBenchmark(context.Background(), w, record); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"pass=1:stats=x265.log", "pass=2:stats=x265.log"} {
		if !strings.Contains(string(b), pass) {
			t.Fatalf("benchmark lost %s: %s", pass, b)
		}
	}
	if record.Evidence == nil || len(record.Evidence.CandidateSamples) != 1 {
		t.Fatal("missing measured sample evidence")
	}
}

func TestJoyNativeTwoPassAndHEAAC(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	enc, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !enc["libx265"] {
		t.Skip("libx265 unavailable")
	}
	d := t.TempDir()
	ref := filepath.Join(d, "reference.mkv")
	out := filepath.Join(d, "candidate.mkv")
	if b, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=25:duration=2", "-c:v", "ffv1", ref).CombinedOutput(); err != nil {
		t.Fatalf("reference: %v %s", err, b)
	}
	p := joyPlan(t)
	args, err := BuildCandidateEncodeArgs(ref, out, p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	var log strings.Builder
	cmd.Stderr = &log
	if err := runEncoderCommand(ctx, cmd, args, p, out); err != nil {
		t.Fatalf("two passes: %v %s", err, log.String())
	}
	probe := ResolveToolPath("", "ffprobe")
	streams, _, err := ProbeSourceStreams(ctx, probe, out)
	if err != nil || len(streams) != 1 || streams[0].BitDepth != 10 {
		t.Fatalf("Main10 not verified: %+v %v", streams, err)
	}
	if !strings.Contains(log.String(), "stats-read") && !strings.Contains(log.String(), "kb/s") {
		t.Fatal("missing encoder evidence")
	}
	if !enc["aac_at"] {
		t.Log("HE-AAC unavailable on this platform")
		return
	}
	audio := filepath.Join(d, "audio.mka")
	b, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "anoisesrc=sample_rate=48000:duration=2:seed=7", "-ac", "6", "-c:a", "aac_at", "-profile:a", "4", "-b:a", "160k", audio).CombinedOutput()
	if err != nil {
		t.Fatalf("HE-AAC: %v %s", err, b)
	}
	streams, _, err = ProbeSourceStreams(ctx, probe, audio)
	if err != nil || len(streams) != 1 || streams[0].Channels != 6 || streams[0].Profile != "HE-AAC" {
		t.Fatalf("HE-AAC 5.1 not verified: %+v %v", streams, err)
	}
	if b, err = exec.CommandContext(ctx, ffmpeg, "-v", "error", "-i", audio, "-f", "null", "-").CombinedOutput(); err != nil {
		t.Fatalf("HE-AAC decode: %v %s", err, b)
	}
}
