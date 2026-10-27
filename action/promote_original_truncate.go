package action

import (
	"log"
	"os"
	"strings"
)

// originalEmptied reports whether an earlier run emptied the original and
// stopped before unlinking it. The replacement and the recovery copy were
// verified before that run recorded its intent, and the step verifies them
// again before it gets here, so a resumed removal only finishes the unlink;
// hashing the empty original would fail the promotion for good. Only the
// recorded intent plus a zero size count: a short original without the intent
// is still a failed verification.
func (p *promotionState) originalEmptied(info os.FileInfo) bool {
	return p.OriginalEmptyStarted && p.OriginalBytes > 0 && info != nil && info.Mode().IsRegular() && info.Size() == 0
}

// originalEmptyBlocker says why the original must keep its bytes until it is
// unlinked, "" when they are provably safe elsewhere: the replacement is in
// place at replacement and verified (CandidateMediaVerified is set by the full
// read against CandidateSHA that precedes every removal), and a recovery copy
// made by a full read of the original sits in its recovery directory.
//
// Only recovery_mode "copy" qualifies. move_aside never unlinks the original
// in place (it becomes the recovery file, which cleanup empties), and a mode
// this code does not know keeps the plain unlink. A promotion that proves the
// original by size and modification time never read the recovery copy in
// full, only sampled ranges of it, so those bytes are not proved elsewhere
// either. The checks are stat-only and cheap, and are made again right
// before the file is opened.
func (p *promotionState) originalEmptyBlocker(original os.FileInfo, replacement string) string {
	switch {
	case original == nil || !original.Mode().IsRegular() || p.OriginalBytes <= 0 || original.Size() != p.OriginalBytes:
		return "the original is not the recorded file"
	case p.RecoveryMode != promotionRecoveryCopy:
		return "recovery mode " + promotionOr(p.RecoveryMode, "unknown") + " does not keep a verified copy"
	case p.originalByMetadata():
		return "the recovery copy was verified by sampled ranges, not a full read"
	case !p.BackupVerified || !p.RecoveryVerifiedBeforeReplacement || p.BackupPath == "":
		return "no verified recovery copy"
	case p.CandidateSHA == "" || !p.CandidateMediaVerified || replacement == "":
		return "the replacement is not verified"
	}
	// Each of these must be a different file from the original, with its own
	// recorded size: a recovery copy or replacement that is the original under
	// another name proves nothing about its bytes.
	for _, other := range []struct {
		path string
		size int64
		what string
	}{{p.BackupPath, p.OriginalBytes, "recovery copy"}, {replacement, p.CandidateBytes, "replacement"}} {
		info, err := os.Lstat(other.path)
		switch {
		case err != nil || !info.Mode().IsRegular() || other.size <= 0 || info.Size() != other.size:
			return "the " + other.what + " is not in place"
		case os.SameFile(original, info):
			return "the " + other.what + " is the original file"
		}
	}
	return ""
}

// planOriginalEmptying decides, before the original is unlinked, whether it
// may be emptied first, and records the intent when it may. The caller
// persists it with the removal intent. With the setting off, or an original
// with no bytes, nothing is tried and the skip is empty; otherwise a skip says
// why the original keeps its bytes.
func (e *Engine) planOriginalEmptying(p *promotionState, info os.FileInfo, replacement string) (empty bool, skip string) {
	if !e.truncateBeforeDelete() || info == nil || info.Size() == 0 {
		return false, ""
	}
	if why := p.originalEmptyBlocker(info, replacement); why != "" {
		return false, truncateSkipped + why
	}
	p.OriginalEmptyStarted = true
	return true, ""
}

// removeOriginal unlinks the original a promotion replaced. When planOriginalEmptying
// allowed it, the original is emptied first (see removeDisposable: only when
// this name is its last link, and only if it is still the file info describes,
// which the caller took before it verified the original), so a share that keeps
// SMB deletes in a recycle bin keeps an empty file, not the original's bytes.
// The first result says what emptying did, "" when it was not tried. Emptying
// never fails the removal: whatever stops it leaves the plain unlink.
func (e *Engine) removeOriginal(p *promotionState, path, replacement string, info os.FileInfo, empty bool, skip string) (string, error) {
	if empty {
		skip = ""
		if why := p.originalEmptyBlocker(info, replacement); why != "" {
			skip = truncateSkipped + why
		}
	}
	if !empty || skip != "" {
		if skip != "" {
			log.Printf("original removed without emptying it first: %s", strings.TrimPrefix(skip, truncateSkipped))
		}
		return skip, os.Remove(path)
	}
	truncate, err := e.removeDisposable(path, info)
	if strings.HasPrefix(truncate, truncateSkipped) {
		log.Printf("original removed without emptying it first: %s", strings.TrimPrefix(truncate, truncateSkipped))
	}
	return truncate, err
}

// noteOriginalTruncate keeps what emptying the original did, so a finished
// promotion shows it. An empty outcome (not tried) keeps what an earlier run
// recorded.
func (p *promotionState) noteOriginalTruncate(truncate string) {
	if truncate != "" {
		p.OriginalTruncate = truncate
	}
}

// truncateUnconfirmed is the outcome of a removal whose run stopped after the
// intent to empty the original and before the outcome was recorded; the
// original is gone, and whether it was emptied first is unknown.
const truncateUnconfirmed = "unconfirmed: the run stopped before the outcome was recorded"

// unconfirmedOriginalTruncate is the outcome to record when a resumed removal
// finds the original already gone: the intent says emptying was planned, and
// no recorded outcome says how it ended. Empty when emptying was never planned.
func (p *promotionState) unconfirmedOriginalTruncate() string {
	if p.OriginalEmptyStarted && p.OriginalTruncate == "" {
		return truncateUnconfirmed
	}
	return ""
}

// originalTruncateOutputs is the step output that shows the outcome, nil when
// emptying was not tried.
func originalTruncateOutputs(truncate string) map[string]any {
	if truncate == "" {
		return nil
	}
	return map[string]any{"original_truncate": truncate}
}
