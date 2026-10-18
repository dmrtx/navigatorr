// Package mediafile holds the filename-safety and language-label rules that
// both the server and the transcode worker need. It is a leaf package: it
// imports only the standard library, so either side can depend on it
// without pulling in the other's code.
package mediafile

import (
	"path/filepath"
	"strings"
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
	// Strip BCP-47 locale/subtag (e.g. "en-us" -> "en", "es-419" -> "es", "en_us" -> "en")
	if idx := strings.IndexAny(l, "-_"); idx > 0 {
		base := l[:idx]
		if code, ok := langAliases[base]; ok {
			return code
		}
	}
	return l
}
