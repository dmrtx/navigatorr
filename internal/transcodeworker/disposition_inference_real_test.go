package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// ffmpeg marks a track default on its own, and transcode.InferredDefault
// must say exactly where: only when the command sets no output track's
// flags at all (any -disposition, audio or subtitle, stops it for every
// kind), and then only on the first track of a kind that has two or more
// tracks none of which is default. A single track of a kind is never marked.
// Each case runs the worker's argument shape (copy, -disposition:<kind>:<n>
// written with the builder's dispositionFlags) on sources with no default
// flag and checks the read-back flags against the rule both ways: a flag
// where the rule allows none, or none where it predicts one, fails.
func TestRealDefaultFlagInference(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe unavailable")
	}
	ctx := context.Background()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-v", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	srt := filepath.Join(dir, "sub.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,500 --> 00:00:01,500\nHello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// source makes a file with the given numbers of audio and subtitle
	// tracks, every flag cleared.
	source := func(name string, audio, subs int) string {
		args := []string{"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=2"}
		for i := 0; i < audio; i++ {
			args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=2")
		}
		for i := 0; i < subs; i++ {
			args = append(args, "-i", srt)
		}
		args = append(args, "-map", "0:v")
		for i := 0; i < audio+subs; i++ {
			args = append(args, "-map", fmt.Sprint(i+1))
		}
		args = append(args, "-c:v", "ffv1", "-c:a", "flac", "-c:s", "srt", "-disposition", "0")
		path := filepath.Join(dir, name+".mkv")
		run(append(args, path)...)
		return path
	}
	two, one := source("two", 2, 2), source("one", 1, 1)

	// Known flags, as the probe reports a track with none set: every track
	// is compared (an empty map would say nothing was measured).
	none := map[string]int{"default": 0, "forced": 0, "hearing_impaired": 0, "visual_impaired": 0, "original": 0, "comment": 0}
	pair := []map[string]int{none, none}
	cases := []struct {
		name          string
		src           string
		maps          []string
		audio, subs   []map[string]int // the flags each output track must carry, in output order
		written       map[string]map[string]int
		wantInference int // tracks the rule lets ffmpeg mark default
	}{
		{name: "no flags written", src: two, audio: pair, subs: pair, wantInference: 2},
		{name: "audio flags written", src: two, audio: pair, subs: pair,
			written: map[string]map[string]int{"a:0": none, "a:1": none}},
		{name: "only a subtitle default written", src: two, audio: pair, subs: []map[string]int{none, {"default": 1, "forced": 0}},
			written: map[string]map[string]int{"s:1": {"default": 1}}},
		{name: "one track of each kind", src: one, audio: []map[string]int{none}, subs: []map[string]int{none}},
		{name: "one track of each kind, flags written", src: one, audio: []map[string]int{none}, subs: []map[string]int{none},
			written: map[string]map[string]int{"a:0": none}},
		{name: "one audio and two subtitles kept", src: two, maps: []string{"0:v", "0:a:0", "0:s:0", "0:s:1"},
			audio: []map[string]int{none}, subs: pair, wantInference: 1},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"-i", tc.src}
			maps := tc.maps
			if maps == nil {
				maps = []string{"0"}
			}
			for _, m := range maps {
				args = append(args, "-map", m)
			}
			args = append(args, "-c", "copy")
			for spec, flags := range tc.written {
				args = append(args, "-disposition:"+spec, dispositionFlags(flags))
			}
			out := filepath.Join(dir, fmt.Sprintf("out%d.mkv", i))
			run(append(args, out)...)
			streams, _, err := ProbeSourceStreams(ctx, ffprobe, out)
			if err != nil {
				t.Fatal(err)
			}
			written, inferences := len(tc.written) > 0, 0
			for kind, planned := range map[string][]map[string]int{"audio": tc.audio, "subtitle": tc.subs} {
				got := streamsOfKind(streams, kind)
				if len(got) != len(planned) {
					t.Fatalf("%d %s tracks, want %d", len(got), kind, len(planned))
				}
				for n, s := range got {
					inferred := transcode.InferredDefault(n, planned, written)
					if inferred {
						inferences++
					}
					if d := transcode.DispositionChange(planned[n], s.Disposition, inferred); d != "" {
						t.Errorf("%s %d: flag %s changed (got %v, inferred %v): the rule refuses ffmpeg's own output", kind, n, d, s.Disposition, inferred)
					}
					if inferred && s.Disposition["default"] == 0 {
						t.Errorf("%s %d: the rule allows a default ffmpeg did not set", kind, n)
					}
				}
			}
			if inferences != tc.wantInference {
				t.Errorf("the rule allows %d inferred defaults, want %d", inferences, tc.wantInference)
			}
		})
	}
}
