package transcodeworker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/quality"
)

func realFFmpeg(t *testing.T, needs ...string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("encodes synthetic clips with real ffmpeg")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, need := range needs {
		kind, name, _ := strings.Cut(need, ":")
		flag := "-encoders"
		if kind == "filter" {
			flag = "-filters"
		}
		out, err := exec.Command(ffmpeg, "-hide_banner", flag).Output()
		if err != nil || !strings.Contains(string(out), name) {
			t.Skip(name, "unavailable")
		}
	}
	return ffmpeg
}

// Every partition a video split can hand an encode must produce the very same
// frames as the unpartitioned default. 12 logical CPUs with four video slots
// yields two candidates (pools of 6 and 4 threads); the check encodes each
// against the default and compares framemd5.
func TestRealX265EveryCandidateIsFrameIdentical(t *testing.T) {
	ffmpeg := realFFmpeg(t, "encoder:libx265")
	if runtime.NumCPU() < 6 {
		t.Skip("fewer CPUs than the largest candidate pool")
	}
	withHostCPUs(t, 12)
	w := NewWorker(&WorkerConfig{FFmpeg: ffmpeg, StateDir: t.TempDir(), MaxParallelJobs: 5})
	// Saving a split normally proves its pools in the background; this test
	// runs the check itself.
	w.ffmpegPath = ""
	if _, err := w.SetSlotSplit(transcode.SlotSplit{VideoSlots: 4, PodcastSlots: 1}); err != nil {
		t.Fatal(err)
	}
	w.ffmpegPath = ffmpeg
	want := "pools=6:frame-threads=3,pools=4:frame-threads=3"
	if key := w.x265PartitionKey(); key != want {
		t.Fatalf("candidates %q, want %q", key, want)
	}
	rec := w.RunX265PartitionCheck(context.Background())
	if rec.Status != x265CheckPassed || rec.Params != want {
		t.Fatalf("real check over every candidate: %+v", rec)
	}
}

// libvmaf's n_threads only schedules work: a score never depends on how many
// threads measured it. The worker widens the metric threads when a video job
// runs alone, so this must hold frame by frame.
func TestRealVMAFScoresDoNotDependOnThreads(t *testing.T) {
	ffmpeg := realFFmpeg(t, "encoder:libx264", "filter:libvmaf")
	dir := t.TempDir()
	reference := filepath.Join(dir, "ref.mkv")
	distorted := filepath.Join(dir, "dist.mkv")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	run("-f", "lavfi", "-i", "testsrc2=size=640x360:rate=24:duration=2", "-pix_fmt", "yuv420p10le", "-c:v", "ffv1", reference)
	run("-i", reference, "-c:v", "libx264", "-crf", "34", "-preset", "veryfast", "-pix_fmt", "yuv420p", distorted)
	model, err := quality.Model(quality.ProbeModelID)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "vmaf.json")
	score := func(argsFor func(threads int) []string, threads int) (any, any, bool) {
		t.Helper()
		_ = os.Remove(logPath)
		out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error"}, argsFor(threads)...)...).CombinedOutput()
		if err != nil {
			t.Logf("libvmaf threads=%d: %v\n%s", threads, err, out)
			return nil, nil, false
		}
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["frames"] == nil || doc["pooled_metrics"] == nil {
			t.Fatalf("no scores in %s", data)
		}
		return doc["frames"], doc["pooled_metrics"], true
	}
	invariant := func(t *testing.T, argsFor func(threads int) []string) {
		t.Helper()
		baseFrames, basePooled, ok := score(argsFor, 1)
		if !ok {
			t.Skip("this libvmaf build cannot run the measurement")
		}
		for _, threads := range []int{2, 4, 8} {
			frames, pooled, ok := score(argsFor, threads)
			if !ok {
				t.Fatalf("libvmaf failed with %d threads", threads)
			}
			if !reflect.DeepEqual(frames, baseFrames) || !reflect.DeepEqual(pooled, basePooled) {
				t.Fatalf("scores changed with %d threads", threads)
			}
		}
	}
	t.Run("default model", func(t *testing.T) {
		invariant(t, func(threads int) []string {
			return []string{"-nostdin", "-nostats", "-i", distorted, "-i", reference, "-filter_complex",
				"[0:v][1:v]libvmaf=log_fmt=json:log_path=" + escapeFFmpegFilterPath(logPath) + libvmafThreadsOption(threads), "-f", "null", "-"}
		})
	})
	t.Run("production model", func(t *testing.T) {
		invariant(t, func(threads int) []string {
			args, err := BuildVMAFModelArgs(distorted, reference, logPath, model, 8, 1, threads)
			if err != nil {
				t.Fatal(err)
			}
			return args
		})
	})
	t.Run("cambi", func(t *testing.T) {
		invariant(t, func(threads int) []string {
			return BuildCAMBIArgs(distorted, reference, logPath, 8, threads)
		})
	})
}
