package action

import (
	"log"
	"os"
	"strings"
)

// recoveryEmptied reports whether an earlier run emptied the recovery copy
// and stopped before unlinking it. The copy was verified before that run
// recorded its intent to empty it, so a resumed cleanup has nothing left to
// verify and only finishes the unlink; hashing the empty file would fail the
// promotion for good. Only the recorded intent plus a zero size count: a
// short file without the intent is still a failed verification.
func (p *promotionState) recoveryEmptied(info os.FileInfo) bool {
	return p.RecoveryEmptyStarted && p.OriginalBytes > 0 && info != nil && info.Mode().IsRegular() && info.Size() == 0
}

// planRecoveryEmptying records, before the recovery copy is unlinked, that it
// may be emptied first. The caller persists it together with the cleanup
// intent. A copy that stays whole (a link kept it) simply verifies again.
func (e *Engine) planRecoveryEmptying(p *promotionState, info os.FileInfo) {
	if e.truncateBeforeDelete() && info != nil && info.Size() > 0 {
		p.RecoveryEmptyStarted = true
	}
}

// removeRecoveryFile unlinks a recovery copy or its partial after the caller
// verified it. With cleanup.truncate_before_delete it is emptied first (see
// removeDisposable: only when this name is its last link), so a share that
// keeps SMB deletes in a recycle bin keeps an empty file, not the original's
// bytes. The first result says what emptying did, "" when it was not tried.
// Emptying never fails the removal: whatever stops it leaves the plain unlink.
func (e *Engine) removeRecoveryFile(path string) (string, error) {
	if !e.truncateBeforeDelete() {
		return "", os.Remove(path)
	}
	info, _ := os.Lstat(path) // a missing file is reported by the unlink
	truncate, err := e.removeDisposable(path, info)
	if strings.HasPrefix(truncate, truncateSkipped) {
		log.Printf("recovery copy removed without emptying it first: %s", strings.TrimPrefix(truncate, truncateSkipped))
	}
	return truncate, err
}

// otherCopyInPlace reports whether the library holds the bytes the recovery
// copy stands in for: the replacement at its final path, or the original still
// at its own. A user discard of a recovery copy that is the only original
// (the library deleted it and the import never finished) must leave the bytes
// to the share's recycle bin, so it only empties the copy when this holds.
func (p *promotionState) otherCopyInPlace() bool {
	same := func(path string, size int64) bool {
		info, err := os.Lstat(path)
		return path != "" && size > 0 && err == nil && info.Mode().IsRegular() && info.Size() == size
	}
	return same(p.NewPath, p.CandidateBytes) || same(p.OriginalPath, p.OriginalBytes)
}

// noteRecoveryTruncate keeps what emptying the recovery copy did, so a
// finished promotion shows it. An empty outcome (not tried) keeps what an
// earlier run recorded.
func (p *promotionState) noteRecoveryTruncate(truncate string) {
	if truncate != "" {
		p.RecoveryTruncate = truncate
	}
}
