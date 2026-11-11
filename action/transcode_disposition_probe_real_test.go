package action

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/transcode"
)

// The coordinator holds the candidate's track flags to the source's report as
// the job's state keeps it. A track with no flag set must still come back
// with its flags known (each one 0): a report that dropped the zeros would
// read as "flags unknown", and a default the conversion added would pass.
// The real ffprobe reports every flag, and the state's JSON keeps them.
func TestRealProbeKeepsUnsetTrackFlags(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	srt := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,500 --> 00:00:01,500\nHello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) string {
		t.Helper()
		out := filepath.Join(dir, name)
		if b, err := exec.CommandContext(ctx, ffmpeg, append(append([]string{"-nostdin", "-v", "error", "-y"}, args...), out)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v\n%s", err, b)
		}
		return out
	}
	src := run("src.mkv", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=duration=2", "-f", "lavfi", "-i", "sine=duration=2", "-i", srt,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3", "-c:v", "ffv1", "-c:a", "flac", "-c:s", "srt", "-disposition", "0")
	// The conversion marks the second audio track default and writes no
	// other flag: nothing ffmpeg infers.
	added := run("added.mkv", "-i", src, "-map", "0", "-c", "copy", "-disposition:a:1", "default")

	rep, err := mediainspect.InspectDetailed(ctx, ffprobe, src)
	if err != nil || !rep.Probed {
		t.Fatalf("inspect source: %v", err)
	}
	// The job's state is stored as JSON and read back.
	state, err := json.Marshal(map[string]any{"audio": rep.Audio, "subtitles": rep.Subtitles})
	if err != nil {
		t.Fatal(err)
	}
	var orig map[string]any
	if err := json.Unmarshal(state, &orig); err != nil {
		t.Fatal(err)
	}
	audio, subs := getStreamsList(orig, "audio"), getStreamsList(orig, "subtitles")
	if len(audio) != 2 || len(subs) != 1 {
		t.Fatalf("%d audio and %d subtitle tracks read back, want 2 and 1", len(audio), len(subs))
	}
	for _, s := range append(audio, subs...) {
		for _, name := range transcode.MatroskaDispositions {
			if v, ok := s.Disposition[name]; !ok || v != 0 {
				t.Errorf("stream %d: flag %q read back as %d (present %v), want a known 0", s.Index, name, v, ok)
			}
		}
	}

	out, err := mediainspect.InspectDetailed(ctx, ffprobe, added)
	if err != nil || !out.Probed || len(out.Audio) != 2 {
		t.Fatalf("inspect candidate: %v", err)
	}
	planned := []map[string]int{audio[0].Disposition, audio[1].Disposition}
	if d := transcode.DispositionChange(audio[1].Disposition, out.Audio[1].Disposition, transcode.InferredDefault(1, planned, true)); d != "default" {
		t.Fatalf("a default added to the second audio track gives %q, want it refused as \"default\"", d)
	}
}
