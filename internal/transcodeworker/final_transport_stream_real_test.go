package transcodeworker

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// A good encode of a constant-rate source with no keyframe index (an MPEG
// transport stream, an M2TS, an MPEG program stream) passes the final gate
// end to end, every grid and gated fresh window measured. Their demuxers seek
// to a packet near the time asked, inside a group of pictures, and a decoder
// shows nothing until the next keyframe, so an extract seeking to the
// window's start held a later part of the window than the encode's, or
// nothing: without the source's packet scan (no keyframe times) the same
// encode of a source whose groups of pictures are longer than a window (250
// or 300 pictures) is misaligned. Short groups with B-pictures are measured
// too (there a seek may land on a keyframe by itself, so only the scanned
// gate is held). So is a transport stream cut from a longer one inside a
// group of pictures (its first packets are not a keyframe and show nothing),
// and an AVI whose only keyframe is its first picture: every window's
// keyframe is the file's first, and an AVI cannot seek to or before its
// first packet (ffmpeg then reads on from wherever its probe stopped, and an
// H.264 extract holds nothing), so those extracts decode from the file's start (its index seeks to the
// keyframe by itself, so only the scanned gate is held).
func TestRealTransportStreamFinalGate(t *testing.T) {
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
	if err != nil || !available["libx265"] || !available["libx264"] || !available["mpeg2video"] || !available["aac"] || !available["ac3"] {
		t.Skip("libx265/libx264/MPEG-2/AAC/AC-3 unavailable")
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		video []string
		audio string
		out   []string
		// seeks reach a keyframe by themselves (groups of pictures
		// shorter than a window, or a keyframe index)
		seeksKeyed bool
		cutHead    bool // the file starts inside a group of pictures
	}{
		{name: "source.ts", video: []string{"-c:v", "libx264", "-preset", "veryfast"}, audio: "aac"},
		{name: "short-gop.ts", video: []string{"-c:v", "libx264", "-preset", "veryfast", "-g", "15", "-bf", "3"}, audio: "aac", seeksKeyed: true},
		{name: "cut.ts", video: []string{"-c:v", "libx264", "-preset", "veryfast", "-bf", "3"}, audio: "aac", cutHead: true},
		{name: "source.avi", video: []string{"-c:v", "libx264", "-preset", "veryfast", "-g", "1800", "-bf", "3"}, audio: "ac3", seeksKeyed: true},
		{name: "source.m2ts", video: []string{"-c:v", "libx264", "-preset", "veryfast"}, audio: "aac", out: []string{"-f", "mpegts", "-mpegts_m2ts_mode", "1"}},
		{name: "source.mpg", video: []string{"-c:v", "mpeg2video", "-q:v", "3", "-g", "300", "-bf", "2"}, audio: "ac3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := filepath.Join(dir, tc.name)
			args := append([]string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30:duration=60", "-f", "lavfi", "-i", "sine=duration=60", "-pix_fmt", "yuv420p"}, tc.video...)
			fixture := source
			if tc.cutHead {
				fixture = source + ".whole.ts"
			}
			args = append(append(append(args, "-c:a", tc.audio), tc.out...), fixture)
			if out, err := exec.CommandContext(ctx, ffmpeg, args...).CombinedOutput(); err != nil {
				t.Fatalf("source fixture: %v %s", err, out)
			}
			if tc.cutHead {
				// Drop the first 3 s of 188-byte packets: the file starts
				// past its first keyframe.
				whole, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				cut := len(whole) / 20 / 188 * 188
				if err := os.WriteFile(source, whole[cut:], 0o600); err != nil {
					t.Fatal(err)
				}
			}
			srcProbe, err := ProbeSourceDetails(ctx, ffprobe, source)
			if err != nil {
				t.Fatal(err)
			}
			plan := sizeV3TestPlan(false)
			plan.Preset = "ultrafast"
			grid := transcode.PlanFinalValidationWindows(srcProbe.DurationSec, 4, 600, 5, 12)
			fresh := transcode.FreshValidationWindows(grid, srcProbe.DurationSec, 4, 2)
			if len(grid) != 5 || len(fresh) != 2 {
				t.Fatalf("windows %+v %+v", grid, fresh)
			}
			plan.QualityValidation = &transcode.QualityValidationPlan{Metric: "ssim", Samples: grid, FreshSamples: fresh, GateFreshSamples: true, Quality: transcode.BenchmarkQualityConfig{FinalValidation: &transcode.BenchmarkFinalValidationConfig{Mode: "sampled"}}, BenchmarkRequestDigest: "sha256:" + strings.Repeat("2", 64)}
			plan.PlanDigest, _ = transcode.DigestPlan(plan)
			ep, err := BuildExecutionPlan(plan, srcProbe.Streams, srcProbe.DurationSec)
			if err != nil {
				t.Fatal(err)
			}
			jobDir := filepath.Join(dir, tc.name+"-job")
			for _, sub := range []string{"scanned", "unscanned"} {
				if err := os.MkdirAll(filepath.Join(jobDir, sub), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			candidate := filepath.Join(jobDir, "candidate.mkv")
			if err := RunFFmpegPaths(ctx, ffmpeg, ep, source, candidate, filepath.Join(jobDir, "progress"), filepath.Join(jobDir, "ffmpeg.log")); err != nil {
				log, _ := os.ReadFile(filepath.Join(jobDir, "ffmpeg.log"))
				t.Fatalf("encode: %v\n%s", err, log)
			}
			w := NewWorker(&WorkerConfig{StateDir: filepath.Join(dir, tc.name+"-state"), FFmpeg: ffmpeg, FFprobe: ffprobe})
			scanned := prescanSource(t, w, ep.Plan, srcProbe, source)
			if scanned.videoScan == nil || indexedContainer(scanned.videoScan.format) {
				t.Fatalf("scan %+v: want one of a container with no keyframe index", scanned.videoScan)
			}
			if s := scanned.videoScan; tc.cutHead && (s.LeadingNonKey == 0 || s.Discontinuous || s.timesOverflow) {
				t.Fatalf("cut scan: %d packets before the first keyframe, discontinuous %v, overflow %v: want a cut inside a group of pictures, timed throughout", s.LeadingNonKey, s.Discontinuous, s.timesOverflow)
			}
			if _, err := w.validateEncodedCandidateFull(ctx, candidate, ep, scanned, 0); err != nil {
				t.Fatalf("pre-publish validation: %v", err)
			}
			e, err := w.validateFinalPerceptualScanned(ctx, filepath.Join(jobDir, "scanned"), source, candidate, plan, "sha256:candidate", 1, scanned.videoScan)
			if err != nil || e.Verdict != "pass" || len(e.FreshUnmeasured) != 0 {
				t.Fatalf("a good encode failed the final gate: %+v %v", e, err)
			}
			if tc.seeksKeyed {
				return
			}
			if e, err := w.validateFinalPerceptual(ctx, filepath.Join(jobDir, "unscanned"), source, candidate, plan, "sha256:candidate", 1); err == nil || !strings.Contains(err.Error(), "quality_final_sample_misaligned") {
				t.Fatalf("without keyframe times the seek at the window's start measured aligned windows: %+v %v", e, err)
			}
		})
	}
}

// An MPEG program stream joined from two recordings whose clock restarts 40
// seconds lower than it began (100 s, then 60 s): the scan reads a restarted
// clock and keeps no keyframe times, so a window 5 s into the video would
// seek 20 s before it, to 85 s. No packet before the window has that time,
// and the demuxer's seek finds the restarted part, whose decode never
// reaches the window: an empty extract. A seek before the first packet's
// time is not made; the extract decodes from the file's start and holds the
// window's pictures.
func TestRealWindowSeekClockRestartedLower(t *testing.T) {
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
	dir := t.TempDir()
	var joined []byte
	for _, part := range []struct{ name, src, offset string }{{"a.mpg", "testsrc", "100"}, {"b.mpg", "testsrc2", "60"}} {
		path := filepath.Join(dir, part.name)
		if out, err := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", part.src+"=d=30:s=320x240:r=25",
			"-c:v", "mpeg2video", "-g", "12", "-bf", "2", "-output_ts_offset", part.offset, "-f", "vob", path).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, data...)
	}
	source := filepath.Join(dir, "joined.mpg")
	if err := os.WriteFile(source, joined, 0o600); err != nil {
		t.Fatal(err)
	}
	packets, err := scanFilePackets(ctx, ffprobe, source, packetScanRequest{video: 0})
	if err != nil {
		t.Fatal(err)
	}
	scan := &packets.video
	// The video's first picture is the first packet's, at 100 s; the
	// restarted part holds the earliest times.
	first := scan.firstTime
	if !scan.Discontinuous || scan.earliest > first-20.5 {
		t.Fatalf("fixture: restarted %v, earliest %.3f, first packet %.3f", scan.Discontinuous, scan.earliest, first)
	}
	from, to := 5.0, 6.0
	seek, keyed := scan.windowSeek(first + from)
	if !math.IsInf(seek, -1) || !keyed {
		t.Fatalf("seek %.3f keyed %v, want the file's start", seek, keyed)
	}
	out := filepath.Join(dir, "reference.mkv")
	if b, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error"}, buildExtractionArgs(source, out, 0, first, from, to, seek)...)...).CombinedOutput(); err != nil {
		t.Fatalf("extract: %v %s", err, b)
	}
	pts, err := probeSamplePTS(ctx, ffprobe, out)
	if err != nil || len(pts) != 25 {
		t.Fatalf("extract holds %d pictures (%v), want the window's 25", len(pts), err)
	}
}
