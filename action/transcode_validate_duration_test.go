package action

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// probeScriptWithPackets is a fake ffprobe that prints probe for a stream
// listing and, for a packet scan, one packet per audio stream that spans the
// whole container duration: a candidate whose audio is as long as its video.
func probeScriptWithPackets(t *testing.T, probe string) string {
	t.Helper()
	var parsed struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal([]byte(probe), &parsed); err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.ParseFloat(parsed.Format.Duration, 64)
	if err != nil {
		t.Fatal(err)
	}
	packets := ""
	for _, s := range parsed.Streams {
		if s.CodecType == "audio" {
			packets += fmt.Sprintf("stream_index=%d|pts_time=0.000000|duration_time=%.6f|size=1000\n", s.Index, seconds)
		}
	}
	return "#!/bin/sh\ncase \"$*\" in *-show_packets*) cat <<'PACKETS'\n" + packets + "PACKETS\nexit 0;; esac\ncat <<'JSON'\n" + probe + "\nJSON\n"
}

// A kept audio track must run as long as the source. The container's own
// duration is the longest stream's, so a candidate whose audio stops early
// passes the container check behind a full-length video; only the track's
// packets show it.
func TestPromotionValidationRefusesTruncatedAudio(t *testing.T) {
	restore := mediainspect.SetAudioPacketStallTimeoutForTest(300 * time.Millisecond)
	defer restore()
	const duration = 1420
	for _, tc := range []struct {
		name    string
		packets string // the scan's packets for the two audio streams
		want    StepStatus
		reason  string
	}{
		{"every track full length", packetLine(1, 0, duration) + packetLine(2, 0, duration), StepCompleted, ""},
		{"within the container tolerance", packetLine(1, 0, duration) + packetLine(2, 0, duration-20), StepCompleted, ""},
		{"second track stops early", packetLine(1, 0, duration) + packetLine(2, 0, 600), StepWaitingDecision, "Audio truncated at stream 1: original runs 1420.0s, the track's packets cover 600.0s (820.0s short)"},
		{"first track stops early", packetLine(1, 0, 900) + packetLine(2, 0, duration), StepWaitingDecision, "Audio truncated at stream 0: original runs 1420.0s, the track's packets cover 900.0s (520.0s short)"},
		{"a track with no packets", packetLine(1, 0, duration), StepWaitingDecision, "Audio stream 1 has no measurable packets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, ec := validateWithPackets(t, tc.packets, "")
			if result.Status != tc.want {
				t.Fatalf("status %s (%s%s)", result.Status, result.Error, result.WaitingReason)
			}
			if tc.reason != "" && result.WaitingReason != tc.reason {
				t.Fatalf("reason %q, want %q", result.WaitingReason, tc.reason)
			}
			if tc.want == StepWaitingDecision {
				if len(result.WaitingOptions) == 0 {
					t.Fatal("a refusal must be decidable")
				}
				return
			}
			if v, _ := ec.State["validation"].(map[string]any); v["audio_duration"] != "ok" {
				t.Fatalf("validation %v", v)
			}
		})
	}
}

// A copied track is measured too, and a scan that cannot read the file never
// passes: the length is unknown, not full.
func TestPromotionValidationRefusesAudioWhoseLengthIsUnmeasurable(t *testing.T) {
	restore := mediainspect.SetAudioPacketStallTimeoutForTest(300 * time.Millisecond)
	defer restore()
	for _, tc := range []struct {
		name, packets, reason string
	}{
		{"ffprobe exits non-zero", "echo 'read error' >&2; exit 1", "ffprobe tool_exit"},
		{"packet scan stalls", "exec sleep 30", "no audio packet read for 300ms"},
		{"packets without timing", "echo 'stream_index=1|pts_time=N/A|duration_time=N/A|size=10'", "audio packet timing/size unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := validateWithPackets(t, "", tc.packets)
			if result.Status != StepWaitingDecision || !strings.HasPrefix(result.WaitingReason, "Audio duration not measured at stream 0: ") || !strings.Contains(result.WaitingReason, tc.reason) {
				t.Fatalf("status %s reason %q (%s)", result.Status, result.WaitingReason, result.Error)
			}
		})
	}
}

func packetLine(stream int, start, length float64) string {
	return fmt.Sprintf("stream_index=%d|pts_time=%.6f|duration_time=%.6f|size=1000\n", stream, start, length)
}

// validateWithPackets validates a two-track copied-audio candidate whose fake
// ffprobe prints packets, or runs packetScript in place of the scan.
func validateWithPackets(t *testing.T, packets, packetScript string) (StepResult, *ExecutionContext) {
	t.Helper()
	const duration = 1420
	dir := t.TempDir()
	candidate := filepath.Join(dir, "candidate.mkv")
	if err := os.WriteFile(candidate, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := fmt.Sprintf(`{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "pix_fmt": "yuv420p", "width": 1920, "height": 1080, "bits_per_raw_sample": "8"},
{"index": 1, "codec_type": "audio", "codec_name": "eac3", "channels": 6, "channel_layout": "5.1(side)", "tags": {"language": "eng"}},
{"index": 2, "codec_type": "audio", "codec_name": "ac3", "channels": 2, "channel_layout": "stereo", "tags": {"language": "spa"}}],
"format": {"format_name": "matroska,webm", "duration": "%d"}}`, duration)
	body := "cat <<'PACKETS'\n" + packets + "PACKETS\nexit 0"
	if packetScript != "" {
		body = packetScript
	}
	ffprobe := filepath.Join(dir, "ffprobe")
	script := "#!/bin/sh\ncase \"$*\" in *-show_packets*) " + body + ";; esac\ncat <<'JSON'\n" + probe + "\nJSON\n"
	if err := os.WriteFile(ffprobe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(EngineDeps{Ffprobe: ffprobe})
	ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
		"candidate_path": candidate,
		"plan":           &transcode.Plan{VideoCodec: "libx265", AudioMode: "copy"},
		"original": map[string]any{"size_bytes": int64(1 << 30), "duration_sec": float64(duration),
			"video": []mediainspect.DetailedStream{{Index: 0, Kind: "video", Codec: "h264", PixelFormat: "yuv420p", Width: 1920, Height: 1080, BitDepth: 8}},
			"audio": []mediainspect.DetailedStream{
				{Index: 1, Kind: "audio", Codec: "eac3", Channels: 6, ChannelLayout: "5.1(side)", Language: "eng"},
				{Index: 2, Kind: "audio", Codec: "ac3", Channels: 2, ChannelLayout: "stereo", Language: "spa"}}},
	}}
	result, err := e.validateCandidateDetailed(context.Background(), ec)
	if err != nil {
		t.Fatal(err)
	}
	return result, ec
}

// The real case end to end: a candidate whose audio was cut short while its
// video runs the full length, validated against the full-length source with
// the real ffmpeg and ffprobe.
func TestPromotionValidationRefusesRealTruncatedAudio(t *testing.T) {
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
	if err != nil || !strings.Contains(string(encoders), " libx264 ") || !strings.Contains(string(encoders), " ac3 ") {
		t.Skip("libx264 or AC-3 unavailable")
	}
	dir := t.TempDir()
	run := func(name string, args ...string) string {
		out := filepath.Join(dir, name)
		if b, err := exec.CommandContext(ctx, ffmpeg, append(append([]string{"-nostdin", "-v", "error"}, args...), out)...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, b)
		}
		return out
	}
	video := []string{"-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24:duration=60"}
	source := run("source.mkv", append(append(video, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=60"),
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "ac3", "-b:a", "192k", "-metadata:s:a:0", "language=eng")...)
	// The video runs the full 60 s, so the container's duration looks right;
	// only 20 s of audio follow it.
	short := run("short.mkv", append(append(video, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=20"),
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "ac3", "-b:a", "192k", "-metadata:s:a:0", "language=eng")...)
	full := run("full.mkv", "-i", source, "-map", "0", "-c", "copy")
	srcRep, err := mediainspect.InspectDetailed(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	validate := func(candidate string) StepResult {
		e := NewEngine(EngineDeps{Ffprobe: ffprobe})
		ec := &ExecutionContext{Inputs: map[string]any{}, State: map[string]any{
			"candidate_path": candidate,
			"plan":           &transcode.Plan{VideoCodec: "h264_videotoolbox", AudioMode: "copy"},
			"original":       map[string]any{"size_bytes": srcRep.SizeBytes * 2, "duration_sec": srcRep.DurationSec, "video": srcRep.Video, "audio": srcRep.Audio},
		}}
		result, err := e.validateCandidateDetailed(ctx, ec)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if r := validate(full); r.Status != StepCompleted {
		t.Fatalf("full-length candidate: %s (%s%s)", r.Status, r.Error, r.WaitingReason)
	}
	r := validate(short)
	if r.Status != StepWaitingDecision || !strings.HasPrefix(r.WaitingReason, "Audio truncated at stream 0: ") {
		t.Fatalf("truncated candidate: %s (%s%s)", r.Status, r.Error, r.WaitingReason)
	}
}
