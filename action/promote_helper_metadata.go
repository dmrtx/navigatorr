package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Moving the original aside for a promotion that never reads it; see
// docs/nas-helper.md, "Stage 5b design".
//
// A promotion admitted by the hash-once flow (originalByMetadata) trusts the
// original by its size and modification time and has no full read for a helper
// proof to follow. The proof is taken from the helper instead, once, when the
// recovery mode is chosen: the file the helper sees must be the one the
// transcode inspected (size and mtime), must hash, over its sampled ranges, to
// the same digest the mount computes, and that digest becomes RecoverySample,
// which every later check of the moved file compares against. The trust is the
// one the promotion already had, plus an inode, a change time and agreement
// between two independent paths to the file.

const (
	recoveryReasonHelperStat      = "helper_stat_mismatch"
	recoveryReasonHelperDisagrees = "helper_and_mount_disagree"
)

// helperMtimeMatches reports whether the helper's modification time is the one
// the transcode recorded. The mount reports SMB's 100 ns units; the NAS
// filesystem has the exact nanosecond, so both are cut to 100 ns. The second
// result is the signed difference, for the mode detail.
func helperMtimeMatches(statMod string, mtimeNs int64) (bool, int64, error) {
	want, err := time.Parse(time.RFC3339Nano, statMod)
	if err != nil {
		return false, 0, fmt.Errorf("promotion original stat is unreadable: %w", err)
	}
	return mtimeNs/100 == want.UnixNano()/100, mtimeNs - want.UnixNano(), nil
}

// promotionChooseByHelperSample decides the recovery mode of a metadata-mode
// promotion. Only filesystem promotions are covered first; anything the helper
// cannot vouch for keeps the copy, with a reason that says which check failed.
// On success it records the helper proof and RecoverySample on p, which the
// caller saves with the mode.
func (e *Engine) promotionChooseByHelperSample(ctx context.Context, p *promotionState) (string, string, string) {
	if p.Service != "filesystem" {
		return promotionRecoveryCopy, recoveryReasonOriginalNotRead, ""
	}
	for _, path := range []string{p.BackupPath, p.BackupPath + ".partial"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return promotionRecoveryCopy, recoveryReasonLeftover, ""
		}
	}
	if p.RecoverySample != "" {
		return promotionRecoveryCopy, recoveryReasonLeftover, ""
	}
	sample, err := e.promotionHelperSample(ctx, p.OriginalPath)
	if err != nil || !sample.Identity.Complete {
		return promotionRecoveryCopy, recoveryReasonOriginalNotRead, ""
	}
	id := sample.Identity
	matches, diff, err := helperMtimeMatches(p.OriginalStatMod, id.MtimeNs)
	if err != nil {
		return promotionRecoveryCopy, recoveryReasonOriginalNotRead, ""
	}
	if id.Size != p.OriginalBytes || !matches {
		return promotionRecoveryCopy, recoveryReasonHelperStat, fmt.Sprintf("size_equal=%t mtime_diff_ns=%d", id.Size == p.OriginalBytes, diff)
	}
	mount, err := e.promotionSampleDigestStable(ctx, p.OriginalPath, p.OriginalBytes)
	if err != nil {
		return promotionRecoveryCopy, recoveryReasonOriginalNotRead, ""
	}
	if mount != sample.Digest {
		return promotionRecoveryCopy, recoveryReasonHelperDisagrees, ""
	}
	if err := e.promotionProbeHelper(ctx, filepath.Dir(p.BackupPath), id); err != nil {
		var failure *promotionProbeFailure
		if errors.As(err, &failure) && failure.Definitive {
			logHelperProbe(p.Service, err)
			return promotionRecoveryCopy, recoveryReasonMountProbe, promotionProbeDetail(err)
		}
		if ctx.Err() == nil {
			logHelperProbe(p.Service, err)
		}
		return promotionRecoveryCopy, recoveryReasonOriginalNotRead, ""
	}
	p.RecoveryProof = promotionProofHelper
	p.OriginalHelper = helperProof{Identity: id, Sample: sample.Digest}.persisted()
	p.RecoverySample = sample.Digest
	return promotionRecoveryMoveAside, recoveryReasonMoveAside, ""
}

// promotionReproveOriginalByHelper re-proves, before any move aside intent, that
// the original is the file the mode was chosen for: the helper sees the same
// identity (change time included) and the sampled digest the promotion kept. A
// process restart in between does not matter, nothing here depends on one: the
// proof is the persisted identity and digest, and the stat conditions were
// checked when it was taken. It returns nil on any doubt, and the caller reads
// the file in full.
func (e *Engine) promotionReproveOriginalByHelper(ctx context.Context, p *promotionState) *helperProof {
	k := p.OriginalHelper
	if !p.originalByMetadata() || !p.helperProof() || k == nil || p.RecoverySample == "" || p.MoveAsideStartedAt != "" || k.Sample != p.RecoverySample {
		return nil
	}
	cur, err := e.promotionHelperSample(ctx, p.OriginalPath)
	if err != nil || !cur.Identity.Complete || cur.Identity != k.Identity || cur.Digest != p.RecoverySample || cur.Identity.Size != p.OriginalBytes {
		return nil
	}
	promotionRecordRead(ctx, p.OriginalPath, "nas_helper_identity", "", true)
	return helperProof{Identity: cur.Identity, Sample: cur.Digest}.persisted()
}

// promotionVerifyOriginalByHelper is promotionVerifyByHelper for a promotion
// that may not have read its original: before an intent the persisted proof is
// enough, else (or on any doubt) the file is read in full against OriginalSHA.
func (e *Engine) promotionVerifyOriginalByHelper(ctx context.Context, p *promotionState) (*helperProof, error) {
	if proof := e.promotionReproveOriginalByHelper(ctx, p); proof != nil {
		return proof, nil
	}
	return e.promotionVerifyByHelper(ctx, p.OriginalPath, p.OriginalSHA, p.OriginalHelper, false)
}
