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
	cover := filepath.Join(dir, "cover.png")
	run("-f", "lavfi", "-i", "color=c=red:size=32x32", "-frames:v", "1", cover)
	// source makes a file with an audio track per entry of channels and the
	// given number of subtitle tracks, every flag cleared, then extra (flags
	// to set, a cover to attach).
	source := func(name string, channels []int, subs int, extra ...string) string {
		args := []string{"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=2"}
		for range channels {
			args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=2")
		}
		for i := 0; i < subs; i++ {
			args = append(args, "-i", srt)
		}
		args = append(args, "-map", "0:v")
		for i := 0; i < len(channels)+subs; i++ {
			args = append(args, "-map", fmt.Sprint(i+1))
		}
		args = append(args, "-c:v", "ffv1", "-c:a", "flac", "-c:s", "srt")
		for i, n := range channels {
			args = append(args, fmt.Sprintf("-ac:a:%d", i), fmt.Sprint(n))
		}
		args = append(args, "-disposition", "0")
		path := filepath.Join(dir, name+".mkv")
		run(append(append(args, extra...), path)...)
		return path
	}
	two, one := source("two", []int{1, 1}, 2), source("one", []int{1}, 1)
	// The second audio track has more channels: ffmpeg still marks the
	// first (its "most channels" rule picks streams when no -map is given,
	// not the default flag).
	wider := source("wider", []int{1, 6}, 2)
	// A cover attachment, as the builder re-attaches it (-attach with its
	// type and name); attachments carry no flags.
	covered := source("covered", []int{1, 1}, 2, "-attach", cover, "-metadata:s:t:0", "mimetype=image/png", "-metadata:s:t:0", "filename=cover.png")
	// A default first subtitle, and a forced default one.
	subDefault := source("subdefault", []int{1}, 3, "-disposition:s:0", "default")
	subForced := source("subforced", []int{1}, 3, "-disposition:s:0", "default+forced")
	coverArgs := []string{"-attach", cover, "-metadata:s:t:0", "mimetype=image/png", "-metadata:s:t:0", "filename=cover.png"}

	// Known flags, as the probe reports a track with none set: every track
	// is compared (an empty map would say nothing was measured).
	none := map[string]int{"default": 0, "forced": 0, "hearing_impaired": 0, "visual_impaired": 0, "original": 0, "comment": 0}
	pair := []map[string]int{none, none}
	cases := []struct {
		name        string
		src         string
		maps        []string
		audio, subs []map[string]int // the flags each output track must carry, in output order
		written     map[string]map[string]int
		extra       []string // output arguments after the flags
		// keep, when set, is the plan's track selection: the maps, the
		// flags each output track must carry and the flags written come
		// from transcode's Partition and WrittenDisposition, as the
		// worker's plan computes them, instead of the fields above.
		keep          *transcode.TrackSelection
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
		{name: "second audio track has more channels", src: wider, audio: pair, subs: pair, wantInference: 2},
		{name: "cover attachment in and out", src: covered, maps: []string{"0:V?", "0:a?", "0:s?", "0:t?"},
			audio: pair, subs: pair, extra: coverArgs, wantInference: 2},
		{name: "cover attachment, audio flags written", src: covered, maps: []string{"0:V?", "0:a?", "0:s?", "0:t?"},
			audio: pair, subs: pair, extra: coverArgs, written: map[string]map[string]int{"a:0": none, "a:1": none}},
		{name: "default subtitle kept with the others", src: subDefault,
			keep: &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{2, 3, 4}}},
		{name: "default subtitle dropped, the default moves", src: subDefault,
			keep: &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{3, 4}}},
		{name: "forced default subtitle dropped, no default", src: subForced,
			keep: &transcode.TrackSelection{KeepAudio: []int{1}, KeepSubtitles: []int{3, 4}}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.keep != nil {
				tc.maps, tc.audio, tc.subs, tc.written = planFlags(t, ctx, ffprobe, tc.src, tc.keep)
			}
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
			run(append(append(args, tc.extra...), out)...)
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

// planFlags is what the worker's plan does with a track selection on src:
// the maps (video, then the kept audio and subtitles in source order), the
// flags each kept track must carry after Partition moved a dropped default,
// and the -disposition values WrittenDisposition writes.
func planFlags(t *testing.T, ctx context.Context, ffprobe, src string, keep *transcode.TrackSelection) (maps []string, audio, subs []map[string]int, written map[string]map[string]int) {
	t.Helper()
	streams, _, err := ProbeSourceStreams(ctx, ffprobe, src)
	if err != nil {
		t.Fatal(err)
	}
	var tracks []transcode.TrackInfo
	source := map[int]map[string]int{}
	for _, s := range streams {
		if s.Kind == "audio" || s.Kind == "subtitle" {
			tracks = append(tracks, transcode.TrackInfo{Index: s.Index, Kind: s.Kind, Disposition: s.Disposition})
			source[s.Index] = s.Disposition
		}
	}
	keptAudio, keptSubs, _, _ := keep.Partition(tracks)
	maps = []string{"0:v"}
	written = map[string]map[string]int{}
	for kind, kept := range map[string][]transcode.TrackInfo{"audio": keptAudio, "subtitle": keptSubs} {
		for n, k := range kept {
			if d := transcode.WrittenDisposition(kind, k.Disposition, source[k.Index]); d != nil {
				written[fmt.Sprintf("%s:%d", kind[:1], n)] = d
			}
		}
	}
	for _, k := range keptAudio {
		maps = append(maps, fmt.Sprintf("0:%d", k.Index))
		audio = append(audio, k.Disposition)
	}
	for _, k := range keptSubs {
		maps = append(maps, fmt.Sprintf("0:%d", k.Index))
		subs = append(subs, k.Disposition)
	}
	return maps, audio, subs, written
}
