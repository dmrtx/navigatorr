package transcode

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxFileNameBytes is the longest single path component the NAS filesystem
// accepts (NAME_MAX). A longer name is refused by SMB with "The object name is
// invalid", which strands a finished encode before it can be published.
const MaxFileNameBytes = 255

// partialSuffix is what the publisher appends to the candidate name while it
// uploads: ".partial.<job id>". The candidate and its partial are both
// created, so the candidate name must leave room for this suffix.
func partialSuffix(jobID string) string { return ".partial." + jobID }

// CandidateFileName returns "<stem>.<jobID><ext>", shortening only the stem
// when the candidate, or its ".partial.<jobID>" upload file, would not fit in
// MaxFileNameBytes. The stem is cut on a rune boundary, and the job id and
// extension are kept so the name stays unique and recognisable by the code that
// matches an attempt's own files by name. A name that already fits is returned
// unchanged.
func CandidateFileName(stem, jobID, ext string) string {
	tail := "." + jobID + ext
	budget := MaxFileNameBytes - len(tail) - len(partialSuffix(jobID))
	return truncateOnRune(stem, budget) + tail
}

// ShortenCandidatePath applies CandidateFileName to a candidate path built the
// older way, without a length limit: "<dir>/<stem>.<jobID><ext>". A path whose
// name already fits is returned unchanged, so it is safe to call on every
// candidate. A name that does not carry ".<jobID>" is cut before its extension.
func ShortenCandidatePath(candidate, jobID string) string {
	dir, base := filepath.Split(candidate)
	if len(base)+len(partialSuffix(jobID)) <= MaxFileNameBytes {
		return candidate
	}
	if i := strings.LastIndex(base, "."+jobID); i >= 0 {
		return dir + CandidateFileName(base[:i], jobID, base[i+1+len(jobID):])
	}
	ext := filepath.Ext(base)
	budget := MaxFileNameBytes - len(ext) - len(partialSuffix(jobID))
	return dir + truncateOnRune(strings.TrimSuffix(base, ext), budget) + ext
}

// truncateOnRune cuts s to at most max bytes without splitting a UTF-8 rune.
func truncateOnRune(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
