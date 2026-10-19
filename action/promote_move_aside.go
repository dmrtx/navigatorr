package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Recovery modes. move_aside keeps the original in place, verified and
// reserved, until the moment it would be removed, then renames it into the
// promotion's private recovery directory: no bytes are copied, and the
// library never sees it missing while its episodes still use it. copy is the
// fallback when it cannot be renamed there, and the only mode before it.
const (
	promotionRecoveryCopy      = "copy"
	promotionRecoveryMoveAside = "move_aside"
)

// errPromotionMoveAsideUnsupported means the rename was refused and nothing
// moved; the caller falls back to a recovery copy.
var errPromotionMoveAsideUnsupported = errors.New("the original cannot be renamed into the recovery directory")

// errPromotionOriginalGone means the original vanished from its library path
// before this promotion recorded any intent to move it: nothing is left to
// move or copy, and only the caller knows whether that is safe.
var errPromotionOriginalGone = errors.New("the original disappeared before it was moved aside; nothing moved")

func (p *promotionState) movesAside() bool { return p.RecoveryMode == promotionRecoveryMoveAside }

// sameInode ignores ctime, which a rename changes.
func (fp promotionFingerprint) sameInode(o promotionFingerprint) bool {
	return fp.Dev == o.Dev && fp.Ino == o.Ino && fp.Size == o.Size && fp.MtimeNs == o.MtimeNs
}

// sameFileAfterRead reports whether fp is the file known describes, for a path
// the caller has just read in full and checked against its digest. A
// fingerprint from this process also pins the device. One from an earlier
// process does not: network mounts often get a new device number per mount, so
// after a restart the device is dropped and the inode, size and mtime still
// have to match. Only resume decisions use it; nothing is deleted or
// overwritten on this check.
func (fp promotionFingerprint) sameFileAfterRead(known *promotionFingerprint) bool {
	if known == nil {
		return false
	}
	if k, ok := known.fromThisProcess(); ok {
		return fp.sameInode(k)
	}
	return fp.Ino == known.Ino && fp.Size == known.Size && fp.MtimeNs == known.MtimeNs
}

// promotionKnownFingerprint is the fingerprint this execution's full hash of
// path to sha recorded, or nil when there was none or it was not stable.
func promotionKnownFingerprint(ctx context.Context, path, sha string) *promotionFingerprint {
	v := promotionVerified(ctx)
	if v == nil {
		return nil
	}
	known, ok := v.files[path]
	if !ok || known.sha != sha {
		return nil
	}
	return known.fp.persisted()
}

func (e *Engine) promotionCurrentFingerprint(path string) (promotionFingerprint, error) {
	info, err := e.promotionStatIdentity(path)
	if err != nil {
		return promotionFingerprint{}, err
	}
	fp, ok := promotionFileFingerprint(info)
	if !ok {
		return promotionFingerprint{}, fmt.Errorf("no stable file identity for %s", path)
	}
	return fp, nil
}

// promotionVerifyByIdentity re-proves after a wait that path still holds the
// bytes a full hash in this process recorded as known. It is used only where that cannot lose
// data: renaming the original aside (the rename keeps its bytes), a recovery
// copy in its private 0700 directory, and the library renaming the adopted
// file (finalization reads it in full before any recovery is removed).
// It compares the whole fingerprint, ctime included.
// A fingerprint from an earlier process, or any doubt, reads the whole file.
// It returns the fingerprint now proved, or nil when the platform has none.
func (e *Engine) promotionVerifyByIdentity(ctx context.Context, path, sha string, known *promotionFingerprint) (*promotionFingerprint, error) {
	if k, ok := known.fromThisProcess(); ok {
		fp, err := e.promotionCurrentFingerprint(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && fp == k {
			if v := promotionVerified(ctx); v != nil {
				v.files[path] = promotionVerifiedFile{sha: sha, fp: fp}
				v.record(path, promotionCheckIdentity, sha, fp)
			}
			return fp.persisted(), nil
		}
	}
	if err := e.verifyPromotionHashStable(ctx, path, sha); err != nil {
		return nil, err
	}
	return promotionKnownFingerprint(ctx, path, sha), nil
}

// promotionMoveAside renames the verified original into the recovery path
// right before the old library file is removed. Every state is durable:
//
//   - no intent: the original is at OriginalPath and nothing is at BackupPath;
//   - intent, not moved: either of those, or the original at BackupPath when
//     the rename finished before its checkpoint;
//   - moved: the original is at BackupPath with BackupFingerprint.
//
// Anything else (both present, neither present, a partial) stops with
// nothing moved. A refused rename returns errPromotionMoveAsideUnsupported
// with the original still in place.
func (e *Engine) promotionMoveAside(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	if !p.movesAside() {
		return fmt.Errorf("promotion does not move its original aside")
	}
	if p.MovedAside {
		return e.promotionVerifyRecovered(ctx, p)
	}
	if err := promotionBackupPathMatches(p); err != nil {
		return err
	}
	if p.BackupPath == "" || p.OriginalFingerprint == nil {
		return fmt.Errorf("verified original identity is missing; nothing moved")
	}
	partial := p.BackupPath + ".partial"
	for _, path := range []string{p.OriginalPath, p.BackupPath, partial} {
		if _, err := e.promotionPath(path, true); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(partial); !os.IsNotExist(err) {
		return fmt.Errorf("unexpected recovery partial before moving the original aside; nothing moved, recovery retained")
	}
	_, originalErr := os.Lstat(p.OriginalPath)
	_, backupErr := os.Lstat(p.BackupPath)
	renamed := false
	switch {
	case originalErr != nil && !os.IsNotExist(originalErr):
		return originalErr
	case backupErr != nil && !os.IsNotExist(backupErr):
		return backupErr
	case originalErr == nil && backupErr != nil:
		proved, err := e.promotionVerifyByIdentity(ctx, p.OriginalPath, p.OriginalSHA, p.OriginalFingerprint)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %v", errPromotionOriginalGone, err)
		}
		if err != nil {
			return err
		}
		if proved == nil {
			return fmt.Errorf("the original has no stable file identity to follow through a rename; nothing moved")
		}
		p.OriginalFingerprint = proved
		p.MoveAsideStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.promotionRenameAside(p.OriginalPath, p.BackupPath); err != nil {
			if !promotionCopyUnsupported(err) {
				return fmt.Errorf("move the original aside: %w; recovery retained", err)
			}
			// The rename was refused outright. Prove nothing moved before
			// the caller copies instead.
			if _, statErr := os.Lstat(p.OriginalPath); statErr != nil {
				return fmt.Errorf("move the original aside was refused (%v) and the original is no longer in place: %w", err, statErr)
			}
			if _, statErr := os.Lstat(p.BackupPath); !os.IsNotExist(statErr) {
				return fmt.Errorf("move the original aside was refused (%v) but the recovery path is occupied", err)
			}
			log.Printf("promotion %s: moving the original aside was refused (%v); making a recovery copy instead", ec.InstanceID, err)
			return fmt.Errorf("%w: %v", errPromotionMoveAsideUnsupported, err)
		}
		for _, dir := range []string{filepath.Dir(p.OriginalPath), filepath.Dir(p.BackupPath)} {
			if d, err := os.Open(dir); err == nil {
				_ = d.Sync()
				_ = d.Close()
			}
		}
		renamed = true
	case originalErr != nil && backupErr == nil && p.MoveAsideStartedAt != "":
		// The rename finished before its checkpoint; only a restart gets
		// here, so read the moved file in full.
		if err := e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA); err != nil {
			return err
		}
	case originalErr != nil && backupErr != nil && p.MoveAsideStartedAt == "":
		return errPromotionOriginalGone
	default:
		return fmt.Errorf("original and recovery locations are in an unexpected state (original present: %t, recovery present: %t); nothing moved, recovery retained", originalErr == nil, backupErr == nil)
	}
	fp, err := e.promotionCurrentFingerprint(p.BackupPath)
	if err != nil || !fp.sameFileAfterRead(p.OriginalFingerprint) {
		return fmt.Errorf("recovery location does not hold the verified original; recovery retained")
	}
	if renamed {
		e.promotionCarryRenamed(ctx, p.OriginalPath, p.BackupPath)
	}
	p.MovedAside, p.BackupVerified, p.BackupFingerprint = true, true, fp.persisted()
	p.RecoveryVerifiedBeforeReplacement = true
	return e.savePromotion(ctx, ec, p)
}

func (e *Engine) promotionRenameAside(from, to string) error {
	if e.promotionMoveAsideHook != nil {
		return e.promotionMoveAsideHook(from, to)
	}
	return renamePromotionNoReplace(from, to)
}

// promotionPreserveForRemoval keeps the original recoverable right before it
// leaves its library path: moved aside, or copied when it cannot be moved.
// The copy fallback switches the promotion to copy mode for good, and its
// caller then removes the original like any copy-mode promotion.
func (e *Engine) promotionPreserveForRemoval(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	err := e.promotionMoveAside(ctx, ec, p)
	if !errors.Is(err, errPromotionMoveAsideUnsupported) {
		return err
	}
	p.RecoveryMode, p.MoveAsideStartedAt, p.MoveAsideRefused = promotionRecoveryCopy, "", err.Error()
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	return e.promotionResumeFallbackCopy(ctx, ec, p)
}

// promotionResumeFallbackCopy makes, or finishes after an error or a crash,
// the recovery copy a refused move aside switched to. Until that copy is
// verified nothing has left the library path, so the original is read in
// full and copied (an exact finished partial is reused). Any other state is
// left to promotionVerifyRecovered, which fails closed.
func (e *Engine) promotionResumeFallbackCopy(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	if p.RecoveryMode != promotionRecoveryCopy || p.MoveAsideRefused == "" || p.BackupVerified || p.MovedAside || p.MoveAsideStartedAt != "" || p.DeleteSentAt != "" || p.OldRemoved {
		return nil
	}
	progress := &promotionPreserveProgress{e: e, ec: ec, p: p}
	defer delete(ec.State, "work_progress")
	ctx = context.WithValue(ctx, promotionHashProgressKey{}, progress.hashes(ctx))
	if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return err
	}
	return e.promotionMakeRecoveryCopy(ctx, ec, p, progress)
}

// promotionMoveBack renames a moved-aside original back to its empty library
// path when the candidate could not take that name, so a failed same-name
// publish never leaves the library path empty. It is a restoring mutation and
// runs even when the batch no longer approves the promotion. The intent is
// durable; promotionResumeMoveBack finishes it after a crash.
func (e *Engine) promotionMoveBack(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	ctx = context.WithoutCancel(ctx)
	if err := promotionBackupPathMatches(p); err != nil {
		return err
	}
	for _, path := range []string{p.OriginalPath, p.BackupPath} {
		if _, err := e.promotionPath(path, true); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(p.OriginalPath); !os.IsNotExist(err) {
		return fmt.Errorf("the library path is no longer empty; the original stays at its recovery path")
	}
	if p.MovedAside {
		if err := e.promotionVerifyRecovered(ctx, p); err != nil {
			return err
		}
	} else if err := e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA); err != nil {
		// The rename finished before its checkpoint.
		return err
	}
	p.MoveBackStartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	if err := renamePromotionNoReplace(p.BackupPath, p.OriginalPath); err != nil {
		return fmt.Errorf("move the original back: %w", err)
	}
	for _, dir := range []string{filepath.Dir(p.BackupPath), filepath.Dir(p.OriginalPath)} {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	e.promotionCarryRenamed(ctx, p.BackupPath, p.OriginalPath)
	return e.promotionFinishMoveBack(ctx, ec, p)
}

// promotionResumeMoveBack finishes a move back a crash interrupted: the
// original is back at its library path (read in full, since only a restart
// gets here), or still at its recovery path, as before the intent.
func (e *Engine) promotionResumeMoveBack(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	if p.MoveBackStartedAt == "" {
		return nil
	}
	_, originalErr := os.Lstat(p.OriginalPath)
	_, backupErr := os.Lstat(p.BackupPath)
	switch {
	case originalErr == nil && os.IsNotExist(backupErr):
		if err := e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
			return err
		}
		return e.promotionFinishMoveBack(ctx, ec, p)
	case os.IsNotExist(originalErr) && backupErr == nil:
		p.MoveBackStartedAt = ""
		return e.savePromotion(ctx, ec, p)
	default:
		return fmt.Errorf("original and recovery locations are in an unexpected state after moving the original back (original present: %t, recovery present: %t); nothing moved", originalErr == nil, backupErr == nil)
	}
}

func (e *Engine) promotionFinishMoveBack(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	fp, err := e.promotionCurrentFingerprint(p.OriginalPath)
	known := p.BackupFingerprint
	if known == nil {
		known = p.OriginalFingerprint
	}
	if err != nil || !fp.sameFileAfterRead(known) {
		return fmt.Errorf("the library path does not hold the moved-back original; nothing else moved")
	}
	p.OriginalFingerprint = fp.persisted()
	p.MovedAside, p.BackupVerified, p.BackupFingerprint = false, false, nil
	p.MoveAsideStartedAt, p.MoveBackStartedAt, p.RecoveryVerifiedBeforeReplacement = "", "", false
	return e.savePromotion(ctx, ec, p)
}

// promotionRecoveryIsOnlyOriginal refuses to discard, or list as removable,
// a recovery that is the moved-aside original while nothing replaced it: no
// file the candidate's size at its library path, nor at the replacement's
// path when that is a different name. Removing it would remove the only copy.
func promotionRecoveryIsOnlyOriginal(p *promotionState) error {
	if !p.MovedAside && p.MoveAsideStartedAt == "" && p.MoveBackStartedAt == "" {
		return nil
	}
	for _, path := range []string{p.OriginalPath, p.NewPath} {
		if path == "" {
			continue
		}
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && (p.CandidateBytes <= 0 || info.Size() == p.CandidateBytes) {
			return nil
		}
	}
	return fmt.Errorf("this recovery is the moved-aside original and the candidate did not replace it at its library path; it cannot be discarded")
}

// promotionConfirmAdopted re-proves after a wait that path is the candidate
// this promotion fully verified, for steps that cannot lose data, by its
// strict fingerprint, ctime included. A rename by the library usually
// changes ctime, and then, like any other change or a fingerprint from an
// earlier process, it is read in full. Finalization always reads it before
// recovery is removed.
func (e *Engine) promotionConfirmAdopted(ctx context.Context, p *promotionState, path string) error {
	if k, ok := p.CandidateFingerprint.fromThisProcess(); ok && p.CandidateMediaVerified {
		if _, err := e.promotionPath(path, false); err != nil {
			return err
		}
		if fp, err := e.promotionCurrentFingerprint(path); err == nil && fp == k {
			if v := promotionVerified(ctx); v != nil {
				v.record(path, promotionCheckIdentity, p.CandidateSHA, fp)
			}
			return nil
		}
	}
	return e.promotionInspectAdopted(ctx, p, path)
}

// promotionMountProbes caches, per process and recovery directory, whether
// the mount there keeps the identity move-aside relies on.
var promotionMountProbes sync.Map

// promotionProbeMount checks once per process, in the recovery directory,
// that a file there keeps what move-aside trusts across waits: a real inode
// and change time, stable while nothing changes; the same inode after a
// no-replace rename; and a change time that moves when the bytes are
// rewritten in place with size and mtime restored. Mounts that synthesize
// inode numbers or lack change times fail it, and then the promotion copies.
func (e *Engine) promotionProbeMount(dir string) error {
	if e.promotionMountProbeHook != nil {
		return e.promotionMountProbeHook(dir)
	}
	key := filepath.Clean(filepath.Dir(dir))
	if cached, ok := promotionMountProbes.Load(key); ok {
		err, _ := cached.(error)
		return err
	}
	err := probePromotionMount(dir)
	promotionMountProbes.Store(key, err)
	return err
}

func probePromotionMount(dir string) error {
	f, err := os.CreateTemp(dir, ".mount-probe-")
	if err != nil {
		return err
	}
	from, to := f.Name(), f.Name()+".moved"
	defer os.Remove(from)
	defer os.Remove(to)
	content := []byte("navigatorr move-aside probe 0")
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	stat := func(path string) (promotionFingerprint, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return promotionFingerprint{}, err
		}
		fp, ok := promotionFileFingerprint(info)
		if !ok {
			return promotionFingerprint{}, fmt.Errorf("no inode number or change time")
		}
		return fp, nil
	}
	created, err := stat(from)
	if err != nil {
		return err
	}
	if again, err := stat(from); err != nil || again != created {
		return fmt.Errorf("file identity is not stable while unchanged")
	}
	if err := renamePromotionNoReplace(from, to); err != nil {
		return err
	}
	moved, err := stat(to)
	if err != nil {
		return err
	}
	if !moved.sameInode(created) {
		return fmt.Errorf("a rename does not keep the file identity")
	}
	mtime := time.Unix(0, moved.MtimeNs)
	// Coarse clocks may need a moment before a change time can differ.
	for i := 0; i < 150; i++ {
		content[len(content)-1] = byte('1' + i%9)
		if err := os.WriteFile(to, content, 0600); err != nil {
			return err
		}
		if err := os.Chtimes(to, mtime, mtime); err != nil {
			return err
		}
		rewritten, err := stat(to)
		if err != nil {
			return err
		}
		if rewritten.Ino != moved.Ino || rewritten.Dev != moved.Dev {
			return fmt.Errorf("a rewrite changed the file identity")
		}
		if rewritten.CtimeNs != moved.CtimeNs {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("a rewrite with size and mtime restored does not change the change time")
}
