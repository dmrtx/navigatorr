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
		if "transcode-modes-"+g.Version != ModePolicyV4 || len(g.Digest) != 12 || strings.Contains(g.Digest, ":") || g.Picture == "" || g.Video == "" || g.Audio == "" || g.Keeps == "" {
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
	if !strings.Contains(size.Quality, "VMAF at least 88 on average, 84 at the 5th percentile and 85 in the worst 5-second window") {
		t.Fatalf("size quality %q", size.Quality)
	}
	if !strings.Contains(size.Audio, "AAC-LC (mono 128, stereo 192, 5.1 384, 7.1 512 kbps)") || !strings.Contains(size.Audio, "Atmos and DTS:X keep their channel bed") {
		t.Fatalf("size audio %q", size.Audio)
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
