package maint

import (
	"strings"
	"testing"
)

// Acceptance: Judas (small, healthy, preferred group) beats SubsPlease
// (heavy) for a 21GB original under a compact preference.
func TestJudasBeatsSubsPlease(t *testing.T) {
	prefs := DefaultAnimePrefs()
	prefs.CurrentSizeBytes = 21 << 30

	judas := ReleaseCandidate{
		GUID: "judas-guid", Title: "[Judas] Fate/strange Fake - 1080p HEVC x265 10bit Dual Audio Multi-Subs",
		ReleaseGroup: "Judas", Size: 6 << 30, Seeders: 100,
		VideoCodec: "hevc", Resolution: "1080p", BitDepth: 10,
		AudioLangs: []string{"jpn", "eng"}, SubLangs: []string{"eng", "spa", "ara"},
		DualAudio: true, MultiSubs: true,
	}
	subsPlease := ReleaseCandidate{
		GUID: "sp-guid", Title: "[SubsPlease] Fate/strange Fake - 1080p HEVC",
		ReleaseGroup: "SubsPlease", Size: 35 << 30, Seeders: 200,
		VideoCodec: "hevc", Resolution: "1080p", BitDepth: 10,
		AudioLangs: []string{"jpn", "eng"}, SubLangs: []string{"eng"},
	}
	ranked := RankRelease([]ReleaseCandidate{subsPlease, judas}, prefs)
	if len(ranked) != 2 {
		t.Fatalf("got %d results", len(ranked))
	}
	if !strings.Contains(ranked[0].Title, "Judas") {
		t.Errorf("winner is %q, want the Judas release", ranked[0].Title)
	}
	if len(ranked[0].Reasons) == 0 {
		t.Error("winner carries no score reasons")
	}
}

// A dangerous torrent scores below everything and is labeled.
func TestDangerousScoresWorst(t *testing.T) {
	prefs := DefaultAnimePrefs()
	bad := ReleaseCandidate{Title: "Show S01 1080p", Size: 1 << 30, Seeders: 500,
		Dangerous: []string{"Episode.mkv.exe"}}
	good := ReleaseCandidate{Title: "Show S01 1080p HEVC", Size: 2 << 30, Seeders: 2,
		VideoCodec: "hevc"}
	ranked := RankRelease([]ReleaseCandidate{bad, good}, prefs)
	if ranked[1].Score >= 0 || ranked[1].Reasons[0] != "dangerous_files" {
		t.Errorf("dangerous release not sunk: %+v", ranked[1])
	}
}

func TestDangerousFilenames(t *testing.T) {
	for _, n := range []string{
		"Dark.Matter.S02E03.1080p.mkv.exe",
		"movie.exe", "setup.scr", "run.bat", "a.cmd", "x.com", "i.msi", "s.ps1",
		"Season 2/Episode.mkv.EXE",
	} {
		if !IsDangerousFilename(n) {
			t.Errorf("%q should be dangerous", n)
		}
	}
	for _, n := range []string{
		"Show.S01E01.1080p.HEVC.x265-Judas.mkv",
		"Movie.2024.1080p.BluRay.mp4",
		"subs/Show.eng.srt",
	} {
		if IsDangerousFilename(n) {
			t.Errorf("%q should be safe", n)
		}
	}
	if got := ScanFilenames([]string{"a.mkv", "b.mkv.exe"}); len(got) != 1 || got[0] != "b.mkv.exe" {
		t.Errorf("scan: %v", got)
	}
}

// Acceptance: Korean audio with no subs is a problem; Japanese audio with
// English subs is not. originalLanguage alone is never consulted.
func TestAccessibleLanguage(t *testing.T) {
	if !NeedsAccessibleSubtitles([]string{"Korean"}, nil) {
		t.Error("korean-only audio without subs should need subs")
	}
	if !NeedsAccessibleSubtitles([]string{"kor"}, []string{"chi"}) {
		t.Error("korean audio with only chinese subs should need eng/spa subs")
	}
	if NeedsAccessibleSubtitles([]string{"Japanese"}, []string{"English"}) {
		t.Error("japanese audio with english subs should be fine")
	}
	if NeedsAccessibleSubtitles([]string{"English"}, nil) {
		t.Error("english audio should be fine")
	}
	if NeedsAccessibleSubtitles([]string{"Spanish"}, nil) {
		t.Error("spanish audio should be fine")
	}
	if NeedsAccessibleSubtitles(nil, nil) {
		t.Error("no audio data should not raise a language issue (incomplete inspection)")
	}
}

func TestOversizedPerEpisode(t *testing.T) {
	// Vanitas: 40.2GB / 24 episodes ~= 1.68GB per episode -> oversized.
	if !IsOversizedEpisode(40200000000, 24, 0) {
		t.Error("vanitas should be oversized")
	}
	// A 150-episode series with 100GB total is only ~0.67GB/episode.
	if IsOversizedEpisode(100000000000, 150, 0) {
		t.Error("long series should not be oversized on total size")
	}
	if IsOversizedEpisode(0, 0, 0) {
		t.Error("empty stats should not flag")
	}
}

func TestPossibleMismatch(t *testing.T) {
	if !PossibleMismatch("The Tank", "/movies/The Tiger (2023)/The.Tiger.2023.mkv") {
		t.Error("tank/tiger mismatch not flagged")
	}
	if PossibleMismatch("The Tank", "/movies/The Tank (2023)/The.Tank.2023.mkv") {
		t.Error("matching title flagged")
	}
}
