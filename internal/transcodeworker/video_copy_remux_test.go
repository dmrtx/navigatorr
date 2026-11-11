package transcodeworker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVideoCopyRemuxFromTransportAndMP4 remuxes MPEG-TS and MP4 sources into
// Matroska with the worker's own audio-only arguments, then runs the two
// checks the worker runs on the candidate: the copied-video proof and the
// packet-for-packet completeness. A good remux must pass both whatever the
// container's packaging of the parameter sets (Annex-B or length-prefixed).
func TestVideoCopyRemuxFromTransportAndMP4(t *testing.T) {
	ffmpeg, ffprobe, ctx := videoCopyTools(t)
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx264"] || !available["ac3"] {
		t.Skip("libx264/AC-3 unavailable")
	}
	dir := t.TempDir()
	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	plan := sizeAudioOnlyTestPlan(t, 8)
	for _, tc := range []struct {
		name, file, codec, ext, audio, rate string
	}{
		{"h264 ts", "h264-ts", "libx264", "ts", "ac3", "24"},
		{"h264 mp4", "h264-mp4", "libx264", "mp4", "aac", "24"},
		{"hevc ts", "hevc-ts", "libx265", "ts", "ac3", "24"},
		{"hevc mp4", "hevc-mp4", "libx265", "mp4", "aac", "24"},
		{"h264 ts 29.97", "h264-ts-ntsc", "libx264", "ts", "ac3", "30000/1001"},
		{"hevc ts 25", "hevc-ts-pal", "libx265", "ts", "ac3", "25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(dir, "source-"+tc.file+"."+tc.ext)
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=" + tc.rate + ":duration=5", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=5",
				"-map", "0:v", "-map", "1:a", "-c:v", tc.codec, "-preset", "ultrafast", "-g", "24", "-bf", "2", "-pix_fmt", "yuv420p", "-c:a", tc.audio, "-b:a", "128k"}
			if tc.codec == "libx265" {
				args = append(args, "-x265-params", "log-level=error")
			}
			if b, err := exec.CommandContext(ctx, ffmpeg, append(args, source)...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, b)
			}
			probe, err := ProbeSourceDetails(ctx, ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			ep, err := BuildExecutionPlan(plan, probe.Streams, probe.DurationSec)
			if err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(dir, "candidate-"+tc.file+".mkv")
			if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(dir, "progress"), filepath.Join(dir, "ffmpeg.log")); err != nil {
				log, _ := os.ReadFile(filepath.Join(dir, "ffmpeg.log"))
				t.Fatalf("remux: %v\n%s", err, log)
			}
			evidence, err := w.validateVideoCopy(ctx, source, candidate, plan, "sha256:c", mustSize(t, candidate))
			if err != nil || evidence.Verdict != "pass" {
				t.Errorf("validateVideoCopy refused a good remux: %v", err)
			}
			if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, prescanSource(t, w, ep.Plan, probe, source), 0); err != nil {
				t.Errorf("completeness refused a good remux: %v", err)
			}
			reject := func(what, bad, wantCode string) {
				t.Helper()
				evidence, err := w.validateVideoCopy(ctx, source, bad, plan, "sha256:bad", mustSize(t, bad))
				if err == nil || evidence == nil || evidence.Verdict == "pass" || len(evidence.VideoCopy.ReasonCodes) != 1 || (wantCode != "" && evidence.VideoCopy.ReasonCodes[0] != wantCode) {
					t.Errorf("%s passed the copy check: %v %v", what, evidence.VideoCopy.ReasonCodes, err)
				}
			}
			// Frames lost in the remux.
			short := filepath.Join(dir, "short-"+tc.file+".mkv")
			if b, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", candidate, "-map", "0", "-c", "copy", "-t", "3", short).CombinedOutput(); err != nil {
				t.Fatalf("short fixture: %v %s", err, b)
			}
			reject("remux without its last frames", short, "video_copy_packets_changed")
			// The same stream with one byte of a picture's payload changed: the
			// parameters and the packet count are the source's, a payload is not.
			corrupt := filepath.Join(dir, "corrupt-"+tc.file+".mkv")
			listing, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pos,size", "-of", "csv=p=0", candidate).Output()
			if err != nil {
				t.Fatal(err)
			}
			var pos, size int64
			if _, err := fmt.Sscanf(strings.Split(strings.TrimSpace(string(listing)), "\n")[3], "%d,%d", &pos, &size); err != nil {
				t.Fatalf("packet listing %q: %v", listing, err)
			}
			data, err := os.ReadFile(candidate)
			if err != nil {
				t.Fatal(err)
			}
			data[pos+size/2] ^= 0x55
			if err := os.WriteFile(corrupt, data, 0o600); err != nil {
				t.Fatal(err)
			}
			reject("a changed picture payload", corrupt, "video_copy_packets_changed")
			// A re-encode to another profile.
			if tc.codec == "libx264" {
				profile := filepath.Join(dir, "profile-"+tc.file+".mkv")
				if b, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-i", candidate, "-map", "0:v", "-c:v", "libx264", "-preset", "ultrafast", "-profile:v", "high", "-g", "24", profile).CombinedOutput(); err != nil {
					t.Fatalf("profile fixture: %v %s", err, b)
				}
				reject("another profile", profile, "video_copy_parameters_changed")
			}
		})
	}
}
