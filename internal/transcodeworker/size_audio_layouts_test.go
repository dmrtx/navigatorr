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

// layoutTrack is one audio track of the layout fixture: a source of noise in
// the named layout (empty for PCM, which Matroska stores without one).
type layoutTrack struct {
	name     string
	layout   string // the layout asked of ffmpeg
	channels int
	codec    string // flac, pcm_s16le, ac3 or eac3
	kbps     string // lossy bitrate
}

func layoutTracks() []layoutTrack {
	var tracks []layoutTrack
	for _, l := range []struct {
		layout   string
		channels int
	}{
		{"mono", 1}, {"stereo", 2}, {"2.1", 3}, {"3.0", 3}, {"4.0", 4}, {"quad", 4}, {"quad(side)", 4},
		{"5.0", 5}, {"5.0(side)", 5}, {"5.1", 6}, {"5.1(side)", 6}, {"hexagonal", 6}, {"6.1(back)", 7},
		{"7.0", 7}, {"7.1", 8}, {"octagonal", 8},
	} {
		tracks = append(tracks, layoutTrack{name: "flac " + l.layout, layout: l.layout, channels: l.channels, codec: "flac"})
	}
	for n := 2; n <= 8; n++ {
		tracks = append(tracks, layoutTrack{name: fmt.Sprintf("pcm %d channels", n), channels: n, codec: "pcm_s16le"})
	}
	tracks = append(tracks,
		layoutTrack{name: "ac3 mono", layout: "mono", channels: 1, codec: "ac3", kbps: "192k"},
		layoutTrack{name: "ac3 3.0", layout: "3.0", channels: 3, codec: "ac3", kbps: "448k"},
		layoutTrack{name: "ac3 5.1", layout: "5.1", channels: 6, codec: "ac3", kbps: "640k"},
		layoutTrack{name: "eac3 stereo", layout: "stereo", channels: 2, codec: "eac3", kbps: "384k"},
	)
	return tracks
}

func layoutNoise(n int, seconds int) string {
	if n == 1 {
		return fmt.Sprintf("anoisesrc=color=pink:amplitude=0.05:sample_rate=48000:duration=%d", seconds)
	}
	var parts, ins []string
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("anoisesrc=color=pink:amplitude=0.05:sample_rate=48000:duration=%d:seed=%d[c%d]", seconds, i+1, i))
		ins = append(ins, fmt.Sprintf("[c%d]", i))
	}
	return strings.Join(parts, ";") + ";" + strings.Join(ins, "") + fmt.Sprintf("amerge=inputs=%d", n)
}

// The size goal's levels are checked with the encoder itself, for every layout
// the policy has a rule for: mono to 7.1 and octagonal, lossless and lossy, and
// PCM of an unspecified layout. Each is planned by the worker's own path (probe,
// plan, ffmpeg, probe, pre-publish validation), which refuses a candidate whose
// channels, layout, codec or profile differ from the plan. Every track must
// validate or be planned as a copy. At level 2 only stereo, quad and 5.1 tracks
// become HE-AAC (the layouts AudioToolbox writes correctly as HE-AAC); every
// other layout keeps its level 1 output. Needs AudioToolbox, so it runs on macOS
// and skips where aac_at is absent.
func TestSizeAudioLevelsRealEncodeLayouts(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	defer cancel()
	available, err := ProbeAvailableEncoders(ctx, ffmpeg)
	if err != nil || !available["libx265"] || !available["libx264"] || !available["aac"] || !available["aac_at"] || !available["eac3"] || !available["ac3"] || !available["flac"] {
		t.Skip("libx265/libx264/AAC/AudioToolbox AAC/AC-3/E-AC-3/FLAC unavailable")
	}
	const seconds = 14
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mkv")
	tracks := layoutTracks()
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=320x180:rate=24:duration=%d", seconds)}
	for _, tr := range tracks {
		args = append(args, "-f", "lavfi", "-i", layoutNoise(tr.channels, seconds))
	}
	args = append(args, "-map", "0:v", "-c:v", "libx264", "-preset", "ultrafast", "-crf", "18", "-pix_fmt", "yuv420p")
	for i, tr := range tracks {
		args = append(args, "-map", fmt.Sprintf("%d:a", i+1), fmt.Sprintf("-c:a:%d", i), tr.codec)
		if tr.layout != "" {
			args = append(args, fmt.Sprintf("-filter:a:%d", i), "aformat=channel_layouts="+tr.layout)
		}
		if tr.kbps != "" {
			args = append(args, fmt.Sprintf("-b:a:%d", i), tr.kbps)
		}
	}
	raw := filepath.Join(dir, "raw.mkv")
	args = append(args, raw)
	if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("source fixture: %v %s", err, out)
	}
	// ffmpeg's Matroska muxer writes no per-stream statistics, which mkvmerge does
	// and which lossless tracks (no bit_rate in the header) are measured from.
	// Tag the fixture the way a real rip is tagged.
	packets, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "packet=stream_index,size", "-of", "csv=p=0", raw).Output()
	if err != nil {
		t.Fatal(err)
	}
	bytesBy := map[int]int64{}
	for _, line := range strings.Split(strings.TrimSpace(string(packets)), "\n") {
		var idx int
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "%d,%d", &idx, &n); err == nil {
			bytesBy[idx] += n
		}
	}
	tagArgs := []string{"-nostdin", "-v", "error", "-y", "-i", raw, "-map", "0", "-c", "copy"}
	for i := range tracks {
		tagArgs = append(tagArgs, fmt.Sprintf("-metadata:s:a:%d", i), fmt.Sprintf("NUMBER_OF_BYTES=%d", bytesBy[i+1]))
	}
	tagArgs = append(tagArgs, source)
	if out, err := exec.CommandContext(ctx, ffmpeg, tagArgs...).CombinedOutput(); err != nil {
		t.Fatalf("tagging the fixture: %v %s", err, out)
	}
	srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
	if err != nil {
		t.Fatal(err)
	}
	srcAudio := streamsOfKind(srcProbe.Streams, "audio")
	if len(srcAudio) != len(tracks) {
		t.Fatalf("%d audio tracks in the fixture, want %d", len(srcAudio), len(tracks))
	}
	for i, s := range srcAudio {
		if s.BitrateKbps <= 0 {
			t.Fatalf("%s: the probe states no bitrate (%+v)", tracks[i].name, s)
		}
		if tracks[i].codec == "pcm_s16le" && s.Layout != "" {
			t.Logf("%s: the fixture came back with layout %q", tracks[i].name, s.Layout)
		}
	}

	w := NewWorker(&WorkerConfig{StateDir: dir, FFmpeg: ffmpeg, FFprobe: ffprobe})
	for _, mode := range transcode.AudioLevelModes {
		plan := sizeV5TestPlan(mode)
		plan.Preset = "ultrafast"
		windows := transcode.PlanFinalValidationWindows(srcProbe.DurationSec, 1, 2.4, 5, 12)
		plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: windows, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64)}
		plan.PlanDigest, _ = transcode.DigestPlan(plan)
		ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
		if err != nil {
			t.Fatal(err)
		}
		jobDir := filepath.Join(dir, mode)
		if err := os.MkdirAll(jobDir, 0o700); err != nil {
			t.Fatal(err)
		}
		candidate := filepath.Join(jobDir, "candidate.mkv")
		if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
			log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
			t.Fatalf("%s encode: %v\n%s", mode, err, log)
		}
		candProbe, err := ProbeSourceDetails(ctx, ffprobe, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, srcProbe, 0); err != nil {
			t.Fatalf("%s: pre-publish validation: %v", mode, err)
		}
		candAudio := streamsOfKind(candProbe.Streams, "audio")
		converted, heaac := 0, 0
		for i, s := range srcAudio {
			out := transcode.AudioOutputFor(mode, s.AudioTrack())
			c := candAudio[i]
			switch {
			case out.Codec == "copy":
				if c.Codec != s.Codec {
					t.Errorf("%s %s: copied track came back as %s", mode, tracks[i].name, c.Codec)
				}
			case out.HEAAC:
				converted++
				heaac++
				if c.Codec != "aac" || c.Profile != "HE-AAC" || c.Channels != s.Channels {
					t.Errorf("%s %s: HE-AAC track came back as %+v", mode, tracks[i].name, c)
				}
			default:
				converted++
				if c.Codec != "aac" || c.Profile != "LC" || c.Channels != s.Channels {
					t.Errorf("%s %s: AAC-LC track came back as %+v", mode, tracks[i].name, c)
				}
			}
			t.Logf("%s %-16s src %-11s %4d kbps ch %d -> %s (encoder aac_at %v, he %v, %d kbps) out %s/%s/%s", mode, tracks[i].name, s.Layout, s.BitrateKbps, s.Channels, out.Codec, out.AudioToolbox, out.HEAAC, out.Kbps, c.Codec, c.Profile, c.Layout)
		}
		if converted < 15 {
			t.Errorf("%s converted only %d of %d tracks; the fixture is not testing the policy", mode, converted, len(srcAudio))
		}
		if mode == transcode.AudioModeSizeAAC && heaac != 0 {
			t.Errorf("level 1 planned %d HE-AAC tracks", heaac)
		}
		if mode == transcode.AudioModeSizeHEAAC && heaac == 0 {
			t.Error("level 2 planned no HE-AAC track")
		}
	}
}
