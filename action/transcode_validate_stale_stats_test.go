package action

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// levelTwoEAC3Source is a source of eight E-AC-3 5.1 640 kbps tracks, which
// audio level 2 turns into HE-AAC 160 kbps each.
func levelTwoEAC3Source() []mediainspect.DetailedStream {
	langs := []string{"eng", "spa", "spa", "fre", "ger", "ita", "jpn", "por"}
	src := make([]mediainspect.DetailedStream, len(langs))
	for i, lang := range langs {
		src[i] = mediainspect.DetailedStream{Index: i + 1, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", Language: lang, BitRate: 640_000}
	}
	return src
}

// A level-2 candidate whose HE-AAC tracks still carry the source's Matroska
// statistics ("BPS-eng: 640000"): Matroska stores no bitrate for AAC, so
// ffprobe reports the stale tag as the track's bitrate. Promotion used to
// read it and refuse a candidate the worker had measured, by its packets, at
// 160 kbps. It now measures the packets, as the worker does.
func TestPromotionValidationIgnoresCopiedStatisticsTags(t *testing.T) {
	src := levelTwoEAC3Source()
	const duration = 1420
	for _, tc := range []struct {
		name       string
		packetKbps int
		want       StepStatus
		reason     string
	}{
		{"packets at the planned bitrate", 158, StepCompleted, ""},
		{"packets really at the source bitrate", 640, StepWaitingDecision, "Audio bitrate mismatch at stream 0: planned 160 kbps, output is 640 kbps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			candidate := filepath.Join(dir, "candidate.mkv")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			streams := `{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p10le", "width": 1920, "height": 1080, "bits_per_raw_sample": "10"}`
			packets := ""
			for i, a := range src {
				// What ffprobe prints for HE-AAC in Matroska: no bit_rate, and
				// the source's statistics under a tag language.
				streams += fmt.Sprintf(`,{"index": %d, "codec_type": "audio", "codec_name": "aac", "profile": "HE-AAC", "channels": 6, "channel_layout": "5.1", "tags": {"language": %q, "BPS-eng": "640000", "NUMBER_OF_BYTES-eng": "113600000", "DURATION-eng": "00:23:40.000000000", "DURATION": "00:23:40.021000000"}}`, i+1, a.Language)
				half := int64(tc.packetKbps) * 1000 * duration / 16
				packets += fmt.Sprintf("stream_index=%d|pts_time=0.000000|duration_time=1.000000|size=%d\nstream_index=%d|pts_time=%d.000000|duration_time=1.000000|size=%d\n", i+1, half, i+1, duration-1, half)
			}
			probe := fmt.Sprintf(`{"streams": [%s], "format": {"format_name": "matroska,webm", "duration": "%d"}}`, streams, duration)
			ffprobe := filepath.Join(dir, "ffprobe")
			script := "#!/bin/sh\ncase \"$*\" in *-show_packets*) cat <<'PACKETS'\n" + packets + "PACKETS\nexit 0;; esac\ncat <<'JSON'\n" + probe + "\nJSON\n"
			if err := os.WriteFile(ffprobe, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			// The stale evidence the old check read.
			rep, err := mediainspect.InspectDetailed(context.Background(), ffprobe, candidate)
			if err != nil || rep.Audio[0].BitRate != 640_000 {
				t.Fatalf("ffprobe bitrate %d: %v", rep.Audio[0].BitRate, err)
			}
			e := NewEngine(EngineDeps{Ffprobe: ffprobe})
			ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
				"candidate_path": candidate,
				"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: transcode.AudioModeSizeHEAAC, ModePolicyVersion: transcode.ModePolicyV5, ExpectedBitDepth: 10},
				"original": map[string]any{"size_bytes": int64(8 << 30), "duration_sec": float64(duration),
					"video": []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "hevc", PixelFormat: "yuv420p10le", Width: 1920, Height: 1080, BitDepth: 10}},
					"audio": src},
			}}
			result, err := e.validateCandidateDetailed(context.Background(), ec)
			if err != nil || result.Status != tc.want {
				t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
			}
			if tc.reason != "" && result.WaitingReason != tc.reason {
				t.Fatalf("reason %q, want %q", result.WaitingReason, tc.reason)
			}
			if tc.want == StepCompleted {
				if v, _ := ec.State["validation"].(map[string]any); v["audio_bitrate"] != "packets" {
					t.Fatalf("validation %v", v)
				}
			}
		})
	}
}

// The production case end to end on real files: a candidate built the way
// workers built level-2 candidates before the statistics fix (HE-AAC 160 via
// aac_at, metadata preserved, only the exact statistics names cleared) keeps
// the source's "BPS-eng: 640000", and promotion now passes it without a new
// encode because its packets are at the planned bitrate.
func TestPromotionValidationPassesAlreadyProducedLevelTwoCandidate(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	encoders, err := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil || !strings.Contains(string(encoders), " aac_at ") || !strings.Contains(string(encoders), " eac3 ") || !strings.Contains(string(encoders), " libx264 ") {
		t.Skip("AudioToolbox AAC, E-AC-3 or libx264 unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	args := []string{"-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=14",
		"-f", "lavfi", "-i", "aevalsrc=sin(440*2*PI*t)|sin(550*2*PI*t)|sin(660*2*PI*t)|sin(110*2*PI*t)|sin(770*2*PI*t)|sin(880*2*PI*t):s=48000:c=5.1:d=14",
		"-map", "0:v", "-map", "1:a", "-map", "1:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "eac3", "-b:a", "640k"}
	for i, lang := range []string{"spa", "spa"} {
		s := fmt.Sprintf("-metadata:s:a:%d", i)
		args = append(args, s, "language="+lang, s, "BPS-eng=640000", s, "NUMBER_OF_BYTES-eng=1120000", s, "NUMBER_OF_FRAMES-eng=438", s, "DURATION-eng=00:00:14.000000000")
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, append(args, source)...).CombinedOutput(); err != nil {
		t.Fatalf("source: %v %s", err, out)
	}
	srcRep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dir, "candidate.mkv")
	args = []string{"-nostdin", "-v", "error", "-i", source, "-map", "0:v?", "-map", "0:a?", "-map", "0:s?", "-map", "0:t?", "-map_metadata", "0", "-c:v", "copy", "-c:a", "copy"}
	for i := range srcRep.Audio {
		args = append(args, fmt.Sprintf("-profile:a:%d", i), "4", fmt.Sprintf("-c:a:%d", i), "aac_at", fmt.Sprintf("-b:a:%d", i), "160k")
		for _, tag := range []string{"BPS", "NUMBER_OF_BYTES", "NUMBER_OF_FRAMES", "DURATION", "_STATISTICS_WRITING_APP", "_STATISTICS_WRITING_DATE_UTC", "_STATISTICS_TAGS"} {
			args = append(args, fmt.Sprintf("-metadata:s:a:%d", i), tag+"=")
		}
	}
	if out, err := exec.CommandContext(ctx, ffmpeg, append(args, candidate)...).CombinedOutput(); err != nil {
		t.Fatalf("candidate: %v %s", err, out)
	}
	candRep, err := mediainspect.InspectDetailed(ctx, ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	// The reproduction: HE-AAC 160 whose stated bitrate is the source's.
	if a := candRep.Audio[0]; a.Profile != "HE-AAC" || a.Tags["BPS-eng"] != "640000" || a.BitRate != 640_000 {
		t.Fatalf("candidate track %+v", a)
	}
	plan := &transcode.Plan{VideoCodec: "h264_videotoolbox", AudioMode: transcode.AudioModeSizeHEAAC, ModePolicyVersion: transcode.ModePolicyV5}
	original := map[string]any{"size_bytes": srcRep.SizeBytes, "duration_sec": srcRep.DurationSec, "video": srcRep.Video, "audio": srcRep.Audio}
	e := NewEngine(EngineDeps{Ffprobe: ffprobe})
	ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{"candidate_path": candidate, "plan": plan, "original": original}}
	result, err := e.validateCandidateDetailed(ctx, ec)
	if err != nil || result.Status != StepCompleted {
		t.Fatalf("status %s (%s%s): %v", result.Status, result.Error, result.WaitingReason, err)
	}
	if v, _ := ec.State["validation"].(map[string]any); v["audio_bitrate"] != "packets" {
		t.Fatalf("validation %v", v)
	}
	// What passed it is the packets, which say 160 kbps, not the tag's 640.
	spans, err := mediainspect.MeasureAudioPackets(ctx, ffprobe, candidate)
	if err != nil {
		t.Fatal(err)
	}
	for index, s := range spans {
		if kbps := s.BitsPerSecond() / 1000; kbps < 100 || kbps > float64(transcode.AudioBitrateCeilingBps(160))/1000 {
			t.Fatalf("stream %d measured %.0f kbps", index, kbps)
		}
	}
}
