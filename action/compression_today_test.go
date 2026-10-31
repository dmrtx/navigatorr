package action

import (
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/transcode"
)

// The page describes each goal from the policy a new job freezes today.
func TestCompressionPolicyTodayDescribesEachGoal(t *testing.T) {
	today, err := CompressionPolicyToday()
	if err != nil {
		t.Fatal(err)
	}
	byGoal := map[string]CompressionGoal{}
	for _, g := range today.Goals {
		byGoal[g.Goal] = g
		if g.Version != ModePolicyV4 || len(g.Digest) != 12 || strings.Contains(g.Digest, ":") || g.Picture == "" || g.Video == "" || g.Audio == "" || g.Keeps == "" {
			t.Fatalf("%s: %+v", g.Goal, g)
		}
	}
	if len(byGoal) != 5 {
		t.Fatalf("goals %v", today.Goals)
	}
	size := byGoal["size"]
	for _, want := range []string{"Apple media engine at quality 60, then 65, then 70", "x265 (medium preset) at CRF 22, then 19, then 16"} {
		if !strings.Contains(size.Video, want) {
			t.Fatalf("size video %q lacks %q", size.Video, want)
		}
	}
	if !strings.Contains(size.Quality, "10-second windows, one per 10 minutes of the file (5 to 12): VMAF at least 88 on average and in every window, 84 at the 5th percentile, 85 in the worst 5 seconds; banding (CAMBI) at most 6 on average and 16 at its peak") {
		t.Fatalf("size quality %q", size.Quality)
	}
	if !strings.Contains(size.Audio, "AAC-LC (mono 128, stereo 192, 5.1 384, 7.1 512 kbps) when it is at least 1.25 times that target") || !strings.Contains(size.Audio, "keeps its channel bed") {
		t.Fatalf("size audio %q", size.Audio)
	}
	if !strings.Contains(size.Video, "only the audio is converted, if that still saves 15%") || !strings.Contains(size.Video, "x265 is offered as a next step") {
		t.Fatalf("size video %q", size.Video)
	}
	// Other goals are checked on the five default windows, never the whole file.
	if q := byGoal["quality"].Quality; !strings.Contains(q, "five windows of up to 10 seconds") || strings.Contains(q, "whole file") {
		t.Fatalf("quality check %q", q)
	}
	if !strings.Contains(size.Keeps, "at least 15% smaller") {
		t.Fatalf("size keeps %q", size.Keeps)
	}
	// Shrink converts lossless audio to the same AAC-LC targets and copies lossy
	// Atmos; it never drops a track.
	if c := byGoal["compact"]; !strings.Contains(c.Picture, "at most 1080p") || !strings.Contains(c.Keeps, "at least 30% smaller") || !strings.Contains(c.Audio, "AAC-LC (mono 128, stereo 192, 5.1 384, 7.1 512 kbps)") || !strings.Contains(c.Audio, "lossy Atmos included, is copied") {
		t.Fatalf("compact %+v", c)
	}
	if a := byGoal[AudioOnlyGoal]; a.Quality != "" || !strings.Contains(a.Video, "not re-encoded") || !strings.Contains(a.Keeps, "smaller at all") {
		t.Fatalf("audio only %+v", a)
	}
	if q := byGoal["quality"]; q.Tracks != "" || !strings.Contains(q.Keeps, "does not grow") || !strings.Contains(q.Audio, "copied") {
		t.Fatalf("quality %+v", q)
	}
}

// Every policy version a job can freeze says in the changelog what it changed,
// and the changelog is newest first.
func TestCompressionChangesNameEveryPolicyVersion(t *testing.T) {
	named := map[string]bool{}
	for i, c := range compressionChanges {
		named[c.Version] = true
		if i > 0 && c.Date > compressionChanges[i-1].Date {
			t.Fatalf("%q is out of order", c.Text)
		}
	}
	for _, v := range []string{transcode.ModePolicyV2, transcode.ModePolicyV3, ModePolicyV4, transcode.ModePolicyV5, transcode.ModePolicyV6, transcode.ModePolicyV7} {
		if !named[v] {
			t.Fatalf("no change names %s", v)
		}
	}
}

// HE-AAC covers stereo, quad and 5.1 only; mono and 7.1 stay AAC-LC.
func TestCompressionAudioNamesHEAACLayouts(t *testing.T) {
	got := describeAudio(transcode.AudioModeSizeHEAAC)
	if !strings.Contains(got, "HE-AAC for stereo, quad and 5.1 (stereo 64, 5.1 160 kbps), else AAC-LC (mono 128, stereo 192, 5.1 384, 7.1 512 kbps)") {
		t.Fatalf("HE-AAC audio %q", got)
	}
}
