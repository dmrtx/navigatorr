package transcodeworker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/optimization"
)

func sizeAudioOnlyTestPlan(t *testing.T, bitDepth int) *transcode.Plan {
	t.Helper()
	p, err := transcode.NewSizeAudioOnlyPlan(sizeV3TestPlan(true), bitDepth)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSizeAudioOnlyPlanShape(t *testing.T) {
	p := sizeAudioOnlyTestPlan(t, 8)
	if err := ValidatePlan(p); err != nil {
		t.Fatalf("audio-only plan rejected: %v", err)
	}
	if args, err := BuildVideoEncoderArgs(p); err != nil || !reflect.DeepEqual(args, []string{"-c:v", "copy"}) {
		t.Fatalf("video args %v %v", args, err)
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"perceptual plan":   func(p *transcode.Plan) { p.QualityValidation = sizeV3TestPlan(false).QualityValidation },
		"crf":               func(p *transcode.Plan) { p.Quality = 24 },
		"preset":            func(p *transcode.Plan) { p.Preset = "medium" },
		"pixel format":      func(p *transcode.Plan) { p.PixelFormat = "p010le" },
		"x265 settings":     func(p *transcode.Plan) { p.X265 = &transcode.X265Settings{} },
		"v2 policy":         func(p *transcode.Plan) { p.ModePolicyVersion = optimization.ModePolicyV2 },
		"lossy audio mode":  func(p *transcode.Plan) { p.AudioMode = "compact" },
		"quality mode":      func(p *transcode.Plan) { p.Mode = "quality" },
		"unknown bit depth": func(p *transcode.Plan) { p.ExpectedBitDepth = 0 },
	} {
		m := *p
		mutate(&m)
		m.PlanDigest, _ = transcode.DigestPlan(&m)
		if err := ValidatePlan(&m); err == nil {
			t.Errorf("%s: malformed audio-only plan accepted", name)
		}
	}
	// A re-encoding size plan still needs its frozen perceptual policy.
	encode := sizeV3TestPlan(false)
	encode.QualityValidation = nil
	encode.PlanDigest, _ = transcode.DigestPlan(encode)
	if err := ValidatePlan(encode); err == nil {
		t.Fatal("encode plan without quality validation accepted")
	}
	if _, err := transcode.NewSizeAudioOnlyPlan(sizeV3TestPlan(false), 12); err == nil {
		t.Fatal("12-bit source accepted")
	}
}

func TestVideoCopyCheckpointNeedsEvidence(t *testing.T) {
	p := sizeAudioOnlyTestPlan(t, 8)
	job := &JobRecord{Plan: p, SourceSizeBytes: 1000, CandidateSizeBytes: 500, CandidateSHA256: "sha256:c", JobTelemetry: transcode.JobTelemetry{ValidationFinishedAt: time.Now()}}
	if err := verifyCheckpointCandidate(context.Background(), job, filepath.Join(t.TempDir(), "missing.mkv")); err == nil || !errors.Is(err, ErrSourceInvalid) {
		t.Fatalf("checkpoint without video copy evidence: %v", err)
	}
	job.QualityEvidence = &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: "sha256:c", CandidateSizeBytes: 500, PlanDigest: p.PlanDigest, VideoCopy: &transcode.VideoCopyEvidence{Method: transcode.VideoCopyMethod, Verdict: "pass", SourceHash: "sha256:a", CandidateHash: "sha256:b"}}
	if err := verifyCheckpointCandidate(context.Background(), job, filepath.Join(t.TempDir(), "missing.mkv")); err == nil || !strings.Contains(err.Error(), "video copy attestation") {
		t.Fatalf("mismatched hashes passed the checkpoint: %v", err)
	}
}

// TestSizeAudioOnlyRealCopyWithLosslessAudio runs the real FFmpeg path on a
// small HEVC source with 24-bit PCM 5.1 and FLAC stereo next to an AC-3
// track: the video is copied and proven bit-identical, the lossless tracks
// become AAC with their layout, language and disposition, AC-3 is copied.
func TestSizeAudioOnlyRealCopyWithLosslessAudio(t *testing.T) {
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
	if err != nil || !available["libx265"] || !available["aac"] || !available["ac3"] || !available["flac"] || !probeStreamhashMuxer(ctx, ffmpeg) {
		t.Skip("libx265/AAC/AC-3/FLAC/streamhash unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	args := []string{"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=8",
		"-f", "lavfi", "-i", "aevalsrc=sin(440*2*PI*t)|sin(550*2*PI*t)|sin(660*2*PI*t)|sin(110*2*PI*t)|sin(770*2*PI*t)|sin(880*2*PI*t):s=48000:c=5.1:d=8",
		"-f", "lavfi", "-i", "aevalsrc=sin(300*2*PI*t)|sin(500*2*PI*t):s=48000:c=stereo:d=8",
		"-f", "lavfi", "-i", "sine=frequency=330:sample_rate=48000:duration=8",
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:a",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "-crf", "28", "-pix_fmt", "yuv420p",
		"-c:a:0", "pcm_s24le", "-c:a:1", "flac", "-c:a:2", "ac3", "-ac:a:2", "2", "-b:a:2", "192k",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:0", "title=LPCM 24-bit 5.1", "-metadata:s:a:1", "language=spa", "-metadata:s:a:2", "language=jpn",
		"-disposition:a:0", "default", "-disposition:a:1", "0", "-disposition:a:2", "0", source}
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	plan := sizeAudioOnlyTestPlan(t, 8)
	if err := ValidateEncoderCapabilities(ctx, ffmpeg, plan); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	if err := w.validateModeSourceQuality(ctx, source, plan); err != nil {
		t.Fatalf("audio-only source preflight: %v", err)
	}
	ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
	if err != nil {
		t.Fatal(err)
	}
	if len(ep.Conversions) != 2 || ep.Conversions[0].FromCodec != "pcm_s24le" || ep.Conversions[1].FromCodec != "flac" {
		t.Fatalf("conversions %+v", ep.Conversions)
	}
	jobDir := filepath.Join(dir, "job")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(jobDir, "candidate.mkv")
	if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
		log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
		t.Fatalf("remux: %v\n%s", err, log)
	}
	candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	audio := streamsOfKind(candProbe.Streams, "audio")
	if len(audio) != 3 || audio[0].Codec != "aac" || audio[0].Channels != 6 || !transcode.SameAudioLayout("5.1", audio[0].Layout) || audio[0].Language != "eng" || audio[0].Disposition["default"] != 1 || audio[0].Title != "AAC 5.1" ||
		audio[1].Codec != "aac" || audio[1].Channels != 2 || audio[1].Language != "spa" || audio[2].Codec != "ac3" || audio[2].Language != "jpn" {
		t.Fatalf("audio policy mismatch: %+v", audio)
	}
	attest, err := w.validateEncodedCandidateFull(ctx, candidate, ep, srcProbe, 0)
	if err != nil {
		t.Fatalf("pre-publish validation: %v", err)
	}
	if err := transcode.ValidateFinalSize(mustSize(t, source), attest.SizeBytes, plan.SizePolicy); err != nil {
		t.Fatalf("lossless PCM and FLAC should clear the savings gate: %v", err)
	}
	evidence, err := w.validateVideoCopy(ctx, source, candidate, plan, attest.SHA256, attest.SizeBytes)
	if err != nil || evidence.Verdict != "pass" || evidence.VideoCopy.Check() != nil || evidence.VideoCopy.SourceCodec != "hevc" || evidence.PlanDigest != plan.PlanDigest {
		t.Fatalf("video copy validation: %+v %v", evidence, err)
	}
	job := &JobRecord{Plan: plan, SourceSizeBytes: mustSize(t, source), CandidateSizeBytes: attest.SizeBytes, CandidateSHA256: attest.SHA256, QualityEvidence: evidence, JobTelemetry: transcode.JobTelemetry{ValidationFinishedAt: time.Now()}}
	if err := verifyCheckpointCandidate(ctx, job, candidate); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// A re-encoded video, even at the same codec and resolution, is not a copy.
	reencoded := filepath.Join(jobDir, "reencoded.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", candidate, "-map", "0", "-c", "copy", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "-crf", "30", reencoded).CombinedOutput(); err != nil {
		t.Fatalf("re-encode fixture: %v %s", err, out)
	}
	evidence, err = w.validateVideoCopy(ctx, source, reencoded, plan, "sha256:other", mustSize(t, reencoded))
	var rejected *transcode.PolicyRejectionError
	if !errors.As(err, &rejected) || rejected.ReasonCode != "preservation_not_met" || evidence == nil || evidence.Verdict == "pass" || len(evidence.VideoCopy.ReasonCodes) != 1 || evidence.VideoCopy.ReasonCodes[0] != "video_copy_packets_changed" {
		t.Fatalf("re-encoded video passed the copy check: %+v %v", evidence, err)
	}
}

func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
