// Package maint holds the deterministic media-maintenance logic: release
// ranking, filename safety, language accessibility and library-issue
// heuristics. Everything here is a pure function of its inputs so the LLM
// never becomes the sole owner of a score or a safety verdict.
package maint

import (
	"path/filepath"
	"strings"
)

// Issue types produced by scans and accepted by the maintenance queue.
const (
	IssueOversized        = "oversized"
	IssueMissingLanguage  = "missing_accessible_language"
	IssueDangerousMedia   = "dangerous_media"
	IssuePossibleMismatch = "possible_media_mismatch"
	IssueNeedsInspection  = "needs_inspection"
)

// BlockedExtensions are never acceptable inside a media download.
var BlockedExtensions = map[string]bool{
	"exe": true, "scr": true, "bat": true, "cmd": true, "com": true,
	"msi": true, "ps1": true, "jar": true, "vbs": true, "js": true,
	"lnk": true, "dll": true, "reg": true,
}

// VideoExtensions are the containers a media torrent is expected to hold.
var VideoExtensions = map[string]bool{
	"mkv": true, "mp4": true, "avi": true, "ts": true, "m2ts": true,
	"webm": true, "mov": true,
}

// SidecarSubExtensions are external subtitle files worth detecting.
var SidecarSubExtensions = map[string]bool{
	"srt": true, "ass": true, "ssa": true, "vtt": true, "sub": true, "idx": true,
}

// IsDangerousFilename reports whether a filename must reject a release:
// a blocked extension, or a double extension hiding an executable behind a
// video suffix (Episode.mkv.exe).
func IsDangerousFilename(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(base)), ".")
	if BlockedExtensions[ext] {
		return true
	}
	// Double extension: strip the outer suffix and inspect the inner one.
	// A video inner suffix with a blocked outer suffix (mkv.exe) is the
	// classic disguised-malware shape.
	inner := strings.TrimSuffix(base, "."+ext)
	innerExt := strings.TrimPrefix(strings.ToLower(filepath.Ext(inner)), ".")
	if innerExt != "" && VideoExtensions[innerExt] && BlockedExtensions[ext] {
		return true
	}
	// Any blocked extension anywhere in a multi-suffix name is suspicious.
	for _, part := range strings.Split(base, ".")[1:] {
		if BlockedExtensions[strings.ToLower(part)] {
			return true
		}
	}
	return false
}

// ScanFilenames returns the subset of names that are dangerous.
func ScanFilenames(names []string) []string {
	var out []string
	for _, n := range names {
		if IsDangerousFilename(n) {
			out = append(out, n)
		}
	}
	return out
}

// langAliases normalizes free-form language labels to ISO 639-2 codes.
var langAliases = map[string]string{
	"english": "eng", "eng": "eng", "en": "eng",
	"spanish": "spa", "espanol": "spa", "español": "spa", "spa": "spa", "es": "spa",
	"castilian": "spa",
	"japanese":  "jpn", "jpn": "jpn", "ja": "jpn", "jap": "jpn",
	"korean": "kor", "kor": "kor", "ko": "kor",
	"chinese": "chi", "chi": "chi", "zho": "chi", "zh": "chi", "mandarin": "chi",
	"cantonese": "yue",
	"french":    "fre", "fre": "fre", "fra": "fre", "fr": "fre",
	"german": "ger", "ger": "ger", "deu": "ger", "de": "ger",
	"italian": "ita", "portuguese": "por", "russian": "rus", "hindi": "hin",
	"und": "und", "unknown": "und",
}

// NormalizeLang maps a language label to a canonical code.
func NormalizeLang(label string) string {
	l := strings.ToLower(strings.TrimSpace(label))
	if code, ok := langAliases[l]; ok {
		return code
	}
	return l
}

// NormalizeLangs normalizes a list, dropping empties and unknowns.
func NormalizeLangs(labels []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range labels {
		c := NormalizeLang(l)
		if c == "" || c == "und" {
			continue
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// AccessibleAudio reports the acceptable audio/subtitle languages.
func AccessibleAudio() []string { return []string{"eng", "spa"} }

// HasUnknownLang reports whether any language label in the slice is unknown, und, or empty.
// An empty slice returns false (no unknown labels present).
func HasUnknownLang(labels []string) bool {
	for _, l := range labels {
		c := NormalizeLang(l)
		if c == "" || c == "und" || c == "unknown" {
			return true
		}
	}
	return false
}

// EvaluateLanguageAccessibility returns the accessibility verdict according to policy:
// - Audio has eng or spa => "accessible"
// - Subs have eng or spa => "accessible"
// - Audio is empty or contains und/unknown => IssueNeedsInspection
// - Audio is known foreign:
//   - Subs contain und/unknown => IssueNeedsInspection
//   - Subs empty or known foreign only => IssueMissingLanguage
func EvaluateLanguageAccessibility(audioLangs, subtitleLangs []string) string {
	audio := NormalizeLangs(audioLangs)
	subs := NormalizeLangs(subtitleLangs)

	for _, a := range audio {
		if a == "eng" || a == "spa" {
			return "accessible"
		}
	}
	for _, s := range subs {
		if s == "eng" || s == "spa" {
			return "accessible"
		}
	}

	if len(audio) == 0 || HasUnknownLang(audioLangs) {
		return IssueNeedsInspection
	}
	if HasUnknownLang(subtitleLangs) {
		return IssueNeedsInspection
	}

	return IssueMissingLanguage
}

// NeedsAccessibleSubtitles reports true when none of the audio streams is in
// an accessible language AND none of the subtitle streams is either. Real
// stream data is required: originalLanguage alone is not enough.
func NeedsAccessibleSubtitles(audioLangs, subtitleLangs []string) bool {
	return EvaluateLanguageAccessibility(audioLangs, subtitleLangs) == IssueMissingLanguage
}

// OversizedThresholdBytes is the default per-episode size flag for anime.
const OversizedThresholdBytes = int64(900 << 20) // ~900MB per episode

// IsOversizedEpisode reports whether sizeOnDisk/episodeFileCount exceeds the
// per-episode threshold. Per-episode math matters: a 150-episode series is
// naturally large in total.
func IsOversizedEpisode(sizeOnDisk int64, episodeFileCount int, thresholdBytes int64) bool {
	if episodeFileCount <= 0 || sizeOnDisk <= 0 {
		return false
	}
	if thresholdBytes <= 0 {
		thresholdBytes = OversizedThresholdBytes
	}
	return sizeOnDisk/int64(episodeFileCount) > thresholdBytes
}

// PossibleMismatch heuristically flags a library entry whose file path looks
// unrelated to its title (e.g. title "The Tank", file "The Tiger"). It is a
// weak signal meant to open an investigation, not to prove anything.
func PossibleMismatch(title, filePath string) bool {
	tokens := meaningfulTokens(title)
	if len(tokens) == 0 {
		return false
	}
	lower := strings.ToLower(filePath)
	for _, tok := range tokens {
		if strings.Contains(lower, tok) {
			return false
		}
	}
	return true
}

func meaningfulTokens(title string) []string {
	stop := map[string]bool{"the": true, "a": true, "an": true, "of": true, "and": true}
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(w) >= 4 && !stop[w] {
			out = append(out, w)
		}
	}
	return out
}
