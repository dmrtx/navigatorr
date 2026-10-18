package action

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
// strict also compares ctime; otherwise the inode, size and mtime must match.
// A fingerprint from an earlier process, or any doubt, reads the whole file.
// It returns the fingerprint now proved, or nil when the platform has none.
func (e *Engine) promotionVerifyByIdentity(ctx context.Context, path, sha string, known *promotionFingerprint, strict bool) (*promotionFingerprint, error) {
	if k, ok := known.fromThisProcess(); ok {
		fp, err := e.promotionCurrentFingerprint(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && (fp == k || !strict && fp.sameInode(k)) {
			if v := promotionVerified(ctx); v != nil {
				if strict {
					v.files[path] = promotionVerifiedFile{sha: sha, fp: fp}
				}
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
		proved, err := e.promotionVerifyByIdentity(ctx, p.OriginalPath, p.OriginalSHA, p.OriginalFingerprint, true)
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
	if err != nil || !fp.sameInode(*p.OriginalFingerprint) {
		return fmt.Errorf("recovery location does not hold the verified original; recovery retained")
	}
	if renamed {
		e.promotionCarryRenamed(ctx, p.OriginalPath, p.BackupPath)
	}
	p.MovedAside, p.BackupVerified, p.BackupFingerprint = true, true, fp.persisted()
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
	p.RecoveryMode, p.MoveAsideStartedAt = promotionRecoveryCopy, ""
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	progress := &promotionPreserveProgress{e: e, ec: ec, p: p}
	defer delete(ec.State, "work_progress")
	return e.promotionMakeRecoveryCopy(context.WithValue(ctx, promotionHashProgressKey{}, progress.hashes(ctx)), ec, p, progress)
}

// promotionConfirmAdopted re-proves after a wait that path is the candidate
// this promotion fully verified, for steps that cannot lose data: the
// library renaming it keeps its inode and mtime. Anything else, or a
// fingerprint from an earlier process, is read in full, and finalization
// always reads it before recovery is removed.
func (e *Engine) promotionConfirmAdopted(ctx context.Context, p *promotionState, path string) error {
	if k, ok := p.CandidateFingerprint.fromThisProcess(); ok && p.CandidateMediaVerified {
		if _, err := e.promotionPath(path, false); err != nil {
			return err
		}
		if fp, err := e.promotionCurrentFingerprint(path); err == nil && fp.sameInode(k) {
			if v := promotionVerified(ctx); v != nil {
				v.record(path, promotionCheckIdentity, p.CandidateSHA, fp)
			}
			return nil
		}
	}
	return e.promotionInspectAdopted(ctx, p, path)
}
