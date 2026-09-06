package maint

import (
	"math"
	"strings"
)

// ReleaseCandidate is one Prowlarr/Sonarr/Radarr search result to rank.
type ReleaseCandidate struct {
	GUID         string
	Title        string
	ReleaseGroup string
	Size         int64 // bytes
	Seeders      int
	VideoCodec   string // e.g. "hevc", "h264"
	Resolution   string // e.g. "1080p"
	BitDepth     int
	AudioLangs   []string
	SubLangs     []string
	DualAudio    bool
	MultiSubs    bool
	Dangerous    []string // dangerous files inside the torrent, if known
}

// RankPrefs carries the applicable preferences into the scorer.
type RankPrefs struct {
	PreferredGroups  []string
	PreferredRes     string // e.g. "1080p"
	PreferHEVC       bool
	Prefer10Bit      bool
	PreferDualAudio  bool
	PreferMultiSubs  bool
	RequireSubs      bool // subtitles required when audio is not eng/spa
	MaxSizeBytes     int64
	MinSeeders       int
	CompactBias      bool   // a smaller healthy release beats a heavier one
	CurrentSizeBytes int64
	Objective        string // "size_optimization" (default) or "accessibility_repair"
}

// ScoredRelease is the deterministic output of Score.
type ScoredRelease struct {
	GUID    string   `json:"guid"`
	Title   string   `json:"title"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

// DefaultAnimePrefs returns the initial anime defaults from the spec.
func DefaultAnimePrefs() RankPrefs {
	return RankPrefs{
		PreferredGroups: []string{"Judas", "EMBER", "ASW"},
		PreferredRes:    "1080p",
		PreferHEVC:      true,
		Prefer10Bit:     true,
		PreferDualAudio: true,
		PreferMultiSubs: true,
		RequireSubs:     true,
		MinSeeders:      1,
		CompactBias:     true,
	}
}

// DefaultMoviePrefs returns the initial movie defaults.
func DefaultMoviePrefs() RankPrefs {
	return RankPrefs{
		PreferredRes: "1080p",
		PreferHEVC:   true,
		RequireSubs:  true,
		MinSeeders:   1,
	}
}

// Score ranks one candidate deterministically. The LLM presents and applies
// the result; it must not invent the points.
func Score(c ReleaseCandidate, prefs RankPrefs) ScoredRelease {
	var score float64
	var reasons []string
	bonus := func(points float64, reason string) {
		score += points
		reasons = append(reasons, reason)
	}
	penalty := func(points float64, reason string) {
		score -= points
		reasons = append(reasons, reason)
	}
	title := strings.ToLower(c.Title)
	group := strings.ToLower(c.ReleaseGroup)

	// Hard rejects first.
	if len(c.Dangerous) > 0 {
		return ScoredRelease{GUID: c.GUID, Title: c.Title, Score: -1000,
			Reasons: []string{"dangerous_files"}}
	}
	if c.Seeders <= 0 {
		penalty(25, "no_seeders")
	} else if c.Seeders >= 20 {
		bonus(8, "healthy_seed_count")
	} else if c.Seeders >= 5 {
		bonus(4, "ok_seed_count")
	} else {
		penalty(6, "low_seed_count")
	}

	// Codec / depth.
	codec := strings.ToLower(c.VideoCodec)
	if strings.Contains(codec, "hevc") || strings.Contains(codec, "265") ||
		strings.Contains(title, "x265") || strings.Contains(title, "hevc") {
		if prefs.PreferHEVC {
			bonus(12, "hevc_10bit_preference")
		} else {
			bonus(6, "hevc")
		}
	}
	if c.BitDepth == 10 || strings.Contains(title, "10bit") || strings.Contains(title, "10-bit") {
		if prefs.Prefer10Bit {
			bonus(6, "10bit")
		} else {
			bonus(3, "10bit")
		}
	}

	// Resolution.
	res := strings.ToLower(c.Resolution)
	want := strings.ToLower(prefs.PreferredRes)
	if want == "" {
		want = "1080p"
	}
	if res == "" {
		res = guessResolution(title)
	}
	switch {
	case res == want:
		bonus(8, "preferred_resolution")
	case res == "2160p" && want == "1080p":
		penalty(4, "heavier_than_preferred_resolution")
	case res == "720p" || res == "480p":
		penalty(10, "below_preferred_resolution")
	}

	// Release group.
	for i, g := range prefs.PreferredGroups {
		if g != "" && (strings.Contains(group, strings.ToLower(g)) ||
			strings.Contains(title, strings.ToLower(g))) {
			// Earlier groups rank higher (Judas > EMBER > ASW by default).
			bonus(12-float64(i)*3, "preferred_release_group")
			break
		}
	}

	// Audio / subtitles.
	if c.DualAudio || prefs.PreferDualAudio && hasDualAudio(c) {
		if prefs.PreferDualAudio {
			bonus(6, "dual_audio")
		}
	}
	subs := NormalizeLangs(c.SubLangs)
	hasEng, hasSpa := false, false
	for _, s := range subs {
		if s == "eng" {
			hasEng = true
		}
		if s == "spa" {
			hasSpa = true
		}
	}
	if c.MultiSubs || len(subs) >= 3 {
		bonus(8, "multi_subs")
	} else if hasEng {
		bonus(5, "eng_subs")
	}
	if hasSpa {
		bonus(4, "spa_subs")
	}
	audio := NormalizeLangs(c.AudioLangs)
	needsSubs := NeedsAccessibleSubtitles(audio, subs)
	if needsSubs && prefs.RequireSubs {
		penalty(30, "missing_required_subs")
	}

	// Size: reasonable beats both bloated and (suspiciously) tiny.
	if prefs.MaxSizeBytes > 0 && c.Size > prefs.MaxSizeBytes {
		penalty(10, "excessive_size")
	}
	if prefs.CurrentSizeBytes > 0 && c.Size > 0 {
		ratio := float64(c.Size) / float64(prefs.CurrentSizeBytes)
		if prefs.Objective == "accessibility_repair" {
			// For accessibility repairs, a larger release is acceptable if it brings accessible audio/subs
			if !needsSubs {
				bonus(8, "accessible_for_repair")
			}
			if ratio > 2.5 {
				penalty(8, "excessive_size_increase")
			}
		} else {
			switch {
			case ratio <= 0.35:
				bonus(10, "large_size_reduction")
			case ratio <= 0.6:
				bonus(6, "size_reduction")
			case ratio > 1.5:
				penalty(8, "larger_than_current")
			}
			if prefs.CompactBias && ratio > 1.0 {
				penalty(6, "against_compact_preference")
			}
		}
	}

	if reasons == nil {
		reasons = []string{"no_signals"}
	}
	return ScoredRelease{GUID: c.GUID, Title: c.Title,
		Score: math.Round(score*10) / 10, Reasons: reasons}
}

// RankRelease orders candidates best-first. Ties break by smaller size, then
// by more seeders: a small healthy release beats a heavier one.
func RankRelease(cands []ReleaseCandidate, prefs RankPrefs) []ScoredRelease {
	scored := make([]ScoredRelease, 0, len(cands))
	sizes := map[string]int64{}
	seeds := map[string]int{}
	for _, c := range cands {
		scored = append(scored, Score(c, prefs))
		sizes[c.GUID+c.Title] = c.Size
		seeds[c.GUID+c.Title] = c.Seeders
	}
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0; j-- {
			a, b := scored[j-1], scored[j]
			swap := a.Score < b.Score
			if a.Score == b.Score {
				sa, sb := sizes[a.GUID+a.Title], sizes[b.GUID+b.Title]
				if sa == sb {
					swap = seeds[a.GUID+a.Title] < seeds[b.GUID+b.Title]
				} else {
					swap = sa > sb
				}
			}
			if !swap {
				break
			}
			scored[j-1], scored[j] = scored[j], scored[j-1]
		}
	}
	return scored
}

func guessResolution(title string) string {
	for _, r := range []string{"2160p", "1080p", "720p", "480p"} {
		if strings.Contains(title, r) {
			return r
		}
	}
	return ""
}

func hasDualAudio(c ReleaseCandidate) bool {
	audio := NormalizeLangs(c.AudioLangs)
	hasEng, hasJpn := false, false
	for _, a := range audio {
		if a == "eng" {
			hasEng = true
		}
		if a == "jpn" || a == "kor" || a == "chi" {
			hasJpn = true
		}
	}
	return hasEng && hasJpn
}
