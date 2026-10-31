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
	if args, err := BuildVideoEncoderArgs(p); err != nil || !reflect.DeepEqual(args, []string{"-c:v", "copy", "-copyinkf:v"}) {
		t.Fatalf("video args %v %v", args, err)
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"perceptual plan":   func(p *transcode.Plan) { p.QualityValidation = sizeV3TestPlan(false).QualityValidation },
		"crf":               func(p *transcode.Plan) { p.Quality = 24 },
		"preset":            func(p *transcode.Plan) { p.Preset = "medium" },
		"pixel format":      func(p *transcode.Plan) { p.PixelFormat = "p010le" },
		"x265 settings":     func(p *transcode.Plan) { p.X265 = &transcode.X265Settings{} },
		"v2 policy":         func(p *transcode.Plan) { p.ModePolicyVersion = transcode.ModePolicyV2 },
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
	job.QualityEvidence = &transcode.FinalQualityEvidence{Verdict: "pass", CandidateSHA256: "sha256:c", CandidateSizeBytes: 500, PlanDigest: p.PlanDigest, VideoCopy: &transcode.VideoCopyEvidence{Method: transcode.VideoCopyMethod, Verdict: "pass", SourceHash: "sha256:a", CandidateHash: "sha256:b", SourcePackets: 10, CandidatePackets: 10}}
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
	if err != nil || !available["libx265"] || !available["aac"] || !available["ac3"] || !available["flac"] {
		t.Skip("libx265/AAC/AC-3/FLAC unavailable")
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
	if err != nil || evidence.Verdict != "pass" || evidence.VideoCopy.Check() != nil || evidence.VideoCopy.SourceCodec != "hevc" || evidence.VideoCopy.SourcePackets != 192 || evidence.PlanDigest != plan.PlanDigest {
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
	if !errors.As(err, &rejected) || rejected.ReasonCode != "preservation_not_met" || evidence == nil || evidence.Verdict == "pass" || len(evidence.VideoCopy.ReasonCodes) != 1 || !strings.HasPrefix(evidence.VideoCopy.ReasonCodes[0], "video_copy_") {
		t.Fatalf("re-encoded video passed the copy check: %+v %v", evidence, err)
	}
}

// A level rewrite in the parameter sets changes no slice payload, but it is
// not the source stream: the stream fields catch it.
func TestVideoCopyRejectsParameterSetEdit(t *testing.T) {
	ffmpeg, ffprobe, ctx := videoCopyTools(t)
	dir := t.TempDir()
	source := videoCopyFixture(t, ctx, ffmpeg, dir, "source.mkv", "-g", "24")
	edited := filepath.Join(dir, "edited.mkv")
	if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", source, "-map", "0", "-c", "copy", "-bsf:v", "hevc_metadata=level=6.2", edited).CombinedOutput(); err != nil {
		t.Skipf("hevc_metadata unavailable: %v %s", err, out)
	}
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	evidence, err := w.validateVideoCopy(ctx, source, edited, sizeAudioOnlyTestPlan(t, 8), "sha256:e", mustSize(t, edited))
	if err == nil || evidence == nil || evidence.VideoCopy.Verdict == "pass" {
		t.Fatalf("level edit passed the copy check: %+v %v", evidence, err)
	}
}

// FFmpeg's stream copy drops packets before the first flagged keyframe
// unless told otherwise. The audio-only remux keeps them (-copyinkf), and
// the proof lists packets at the demuxer, so a candidate that lost them,
// or lost every packet of a source with no flagged keyframe, is rejected.
func TestVideoCopyKeepsAndChecksLeadingNonKeyPackets(t *testing.T) {
	ffmpeg, ffprobe, ctx := videoCopyTools(t)
	dir := t.TempDir()
	full := videoCopyFixture(t, ctx, ffmpeg, dir, "full.mkv", "-g", "24")
	cut := func(name string, args ...string) string {
		out := filepath.Join(dir, name)
		cmd := append([]string{"-nostdin", "-v", "error", "-i", full}, args...)
		if b, err := exec.CommandContext(ctx, ffmpeg, append(cmd, "-map", "0", "-c", "copy", "-copyinkf", out)...).CombinedOutput(); err != nil {
			t.Fatalf("cut %s: %v %s", name, err, b)
		}
		return out
	}
	leading := cut("leading.mkv", "-ss", "0.5")          // starts mid-GOP, keyframe at 1 s
	noKey := cut("nokey.mkv", "-ss", "0.3", "-t", "0.4") // inside one GOP: no keyframe
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	plan := sizeAudioOnlyTestPlan(t, 8)
	for _, source := range []string{leading, noKey} {
		name := filepath.Base(source)
		packets, err := probeVideoPackets(ctx, ffprobe, source)
		if err != nil || len(packets) == 0 || packets[0].key {
			t.Fatalf("%s fixture does not start with a non-key packet: %d %v", name, len(packets), err)
		}
		if source == noKey {
			for _, p := range packets {
				if p.key {
					t.Fatalf("%s fixture has a keyframe", name)
				}
			}
		}
		// The worker's own remux keeps every packet and passes.
		probe, err := ProbeSourceDetails(ctx, ffprobe, source)
		if err != nil {
			t.Fatal(err)
		}
		ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(dir, "candidate-"+name)
		if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
			log, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.log"))
			t.Fatalf("%s remux: %v\n%s", name, err, log)
		}
		evidence, err := w.validateVideoCopy(ctx, source, candidate, plan, "sha256:c", mustSize(t, candidate))
		if err != nil || evidence.VideoCopy.Check() != nil || evidence.VideoCopy.CandidatePackets != len(packets) {
			t.Fatalf("%s: worker remux rejected: %+v %v", name, evidence, err)
		}
		// A plain stream copy drops the leading non-key packets.
		dropped := filepath.Join(dir, "dropped-"+name)
		if b, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", source, "-map", "0", "-c", "copy", dropped).CombinedOutput(); err != nil {
			t.Fatalf("%s drop fixture: %v %s", name, err, b)
		}
		evidence, err = w.validateVideoCopy(ctx, source, dropped, plan, "sha256:d", mustSize(t, dropped))
		var rejected *transcode.PolicyRejectionError
		want := map[string]string{"leading.mkv": "video_copy_packets_changed", "nokey.mkv": "video_copy_stream_empty"}[name]
		if !errors.As(err, &rejected) || evidence == nil || len(evidence.VideoCopy.ReasonCodes) != 1 || evidence.VideoCopy.ReasonCodes[0] != want {
			t.Fatalf("%s: candidate without its leading packets passed: %+v %v", name, evidence, err)
		}
	}
}

func TestCompareVideoPackets(t *testing.T) {
	h := strings.Repeat("a", 64)
	src := []videoPacket{{pts: 0.0417, hasPTS: true, size: 10, hash: h}, {pts: 0, hasPTS: true, size: 20, key: true, hash: h}, {pts: 0.0834, hasPTS: true, size: 5, hash: h}}
	remux := []videoPacket{{pts: 1.042, hasPTS: true, size: 10, hash: h}, {pts: 1.0, hasPTS: true, size: 20, key: true, hash: h}, {pts: 1.083, hasPTS: true, size: 5, hash: h}}
	if code, err := compareVideoPackets(src, remux); code != "" {
		t.Fatalf("millisecond remux rejected: %s %v", code, err)
	}
	for name, tc := range map[string]struct {
		cand []videoPacket
		code string
	}{
		"empty candidate": {nil, "video_copy_stream_empty"},
		"dropped packet":  {remux[1:], "video_copy_packets_changed"},
		"key flag":        {[]videoPacket{remux[0], {pts: 1.0, hasPTS: true, size: 20, hash: h}, remux[2]}, "video_copy_packets_changed"},
		"payload":         {[]videoPacket{remux[0], remux[1], {pts: 1.083, hasPTS: true, size: 5, hash: strings.Repeat("b", 64)}}, "video_copy_packets_changed"},
		"timestamp":       {[]videoPacket{remux[0], remux[1], {pts: 1.1, hasPTS: true, size: 5, hash: h}}, "video_copy_timestamps_changed"},
	} {
		if code, _ := compareVideoPackets(src, tc.cand); code != tc.code {
			t.Errorf("%s: %q, want %q", name, code, tc.code)
		}
	}
	if code, _ := compareVideoPackets(nil, nil); code != "video_copy_stream_empty" {
		t.Fatalf("two empty streams compared equal: %q", code)
	}
	empty := transcode.VideoCopyEvidence{Method: transcode.VideoCopyMethod, Verdict: "pass", SourceHash: videoPacketDigest(nil), CandidateHash: videoPacketDigest(nil)}
	if empty.Check() == nil {
		t.Fatal("empty packet digest accepted as proof")
	}
}

func videoCopyTools(t *testing.T) (string, string, context.Context) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	if available, err := ProbeAvailableEncoders(ctx, ffmpeg); err != nil || !available["libx265"] || !available["aac"] {
		t.Skip("libx265/AAC unavailable")
	}
	return ffmpeg, ffprobe, ctx
}

// videoCopyFixture is a small HEVC clip with a lossless PCM stereo track.
func videoCopyFixture(t *testing.T, ctx context.Context, ffmpeg, dir, name string, videoArgs ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	args := append([]string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=3",
		"-f", "lavfi", "-i", "aevalsrc=sin(440*2*PI*t)|sin(550*2*PI*t):s=48000:c=stereo:d=3",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p"}, videoArgs...)
	if b, err := exec.CommandContext(ctx, ffmpeg, append(args, "-c:a", "pcm_s24le", out)...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, b)
	}
	return out
}

func mustSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// sizeAudioOnlyGoalTestPlan is a plan of the audio-only goal (policy v7): the
// video copied, a bed level, no minimum savings, and the selection if any.
func sizeAudioOnlyGoalTestPlan(t *testing.T, sel *transcode.TrackSelection) *transcode.Plan {
	t.Helper()
	base := sizeV3TestPlan(false)
	base.ModePolicyVersion, base.AudioMode, base.SizePolicy = transcode.ModePolicyV7, transcode.AudioModeSizeAACBed, &transcode.SizePolicy{}
	p, err := transcode.NewSizeAudioOnlyPlan(base, 8)
	if err != nil {
		t.Fatal(err)
	}
	p.Tracks = sel
	p.PlanDigest = ""
	p.PlanDigest, _ = transcode.DigestPlan(p)
	return p
}

func TestSizeAudioOnlyGoalPlanVersion(t *testing.T) {
	for _, sel := range []*transcode.TrackSelection{nil, {KeepAudio: []int{1}}} {
		if err := ValidatePlan(sizeAudioOnlyGoalTestPlan(t, sel)); err != nil {
			t.Fatalf("v7 plan (selection %v) rejected: %v", sel, err)
		}
	}
	if m, ok := transcode.ModeMinimumSavings("size", transcode.ModePolicyV7); !ok || m != 0 {
		t.Fatalf("v7 size minimum = %d, %v", m, ok)
	}
	if _, ok := transcode.ModeMinimumSavings("compact", transcode.ModePolicyV7); ok {
		t.Fatal("v7 admits compact")
	}
	for name, mutate := range map[string]func(*transcode.Plan){
		"the size goal's minimum": func(p *transcode.Plan) { p.SizePolicy = &transcode.SizePolicy{MinSavingsPercent: 15} },
		"lossless compaction":     func(p *transcode.Plan) { p.AudioMode = transcode.AudioModeCompactLossless },
		"audio copied":            func(p *transcode.Plan) { p.AudioMode = "copy" },
		"compact mode":            func(p *transcode.Plan) { p.Mode = "compact" },
		"a video encode": func(p *transcode.Plan) {
			e := sizeV3TestPlan(false)
			p.VideoCodec, p.Quality, p.Preset, p.VideoProfile, p.PixelFormat, p.QualityValidation = e.VideoCodec, e.Quality, e.Preset, e.VideoProfile, e.PixelFormat, e.QualityValidation
		},
	} {
		p := sizeAudioOnlyGoalTestPlan(t, nil)
		mutate(p)
		p.PlanDigest, _ = transcode.DigestPlan(p)
		if err := ValidatePlan(p); err == nil {
			t.Errorf("v7 plan with %s accepted", name)
		}
	}
	// v6 still requires its selection, and only v6 and v7 carry one.
	v6 := sizeAudioOnlyGoalTestPlan(t, nil)
	v6.ModePolicyVersion, v6.SizePolicy = transcode.ModePolicyV6, &transcode.SizePolicy{MinSavingsPercent: 15}
	v6.PlanDigest, _ = transcode.DigestPlan(v6)
	if err := ValidatePlan(v6); err == nil {
		t.Fatal("v6 plan without a track selection accepted")
	}
	v5 := sizeAudioOnlyGoalTestPlan(t, &transcode.TrackSelection{KeepAudio: []int{1}})
	v5.ModePolicyVersion, v5.SizePolicy = transcode.ModePolicyV5, &transcode.SizePolicy{MinSavingsPercent: 15}
	v5.PlanDigest, _ = transcode.DigestPlan(v5)
	if err := ValidatePlan(v5); err == nil {
		t.Fatal("v5 plan with a track selection accepted")
	}
}
