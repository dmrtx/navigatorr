package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/mediainspect"
)

func (e *Engine) promotionPath(path string, write bool) (string, error) {
	if e.deps.Fs == nil {
		return "", fmt.Errorf("filesystem resolver is required")
	}
	var resolved string
	var err error
	if write {
		resolved, err = e.deps.Fs.ResolveWrite(path)
	} else {
		resolved, err = e.deps.Fs.ResolveRead(path)
	}
	if err != nil {
		return "", err
	}
	// Promotion can delete files and create a recovery copy. Do not silently
	// follow a symlink, even if its destination happens to be in an allowed root.
	if !filepath.IsAbs(path) || resolved != filepath.Clean(path) {
		return "", fmt.Errorf("promotion path must be absolute and must not traverse symlinks: %s", path)
	}
	return resolved, nil
}

// promotionRecoveryPath derives the deterministic recovery backup path for a
// promotion from durable promotion identity. preserve and every consumer of a
// persisted BackupPath must use this single scheme so a tampered path can never
// redirect recovery verification or cleanup.
func promotionRecoveryPath(service, sourceActionID, candidatePath string) string {
	key := sha256.Sum256([]byte(service + "\x00" + sourceActionID))
	return filepath.Join(filepath.Dir(candidatePath), ".promotion-recovery", hex.EncodeToString(key[:]), "original.bak")
}

func promotionExpectedBackupPath(p *promotionState) (string, error) {
	if p.CandidatePath == "" {
		return "", fmt.Errorf("promotion candidate path is required to derive the recovery location")
	}
	return promotionRecoveryPath(p.Service, p.SourceActionID, p.CandidatePath), nil
}

// promotionBackupPathMatches fails closed when a persisted BackupPath does not
// equal the path derived from durable promotion identity. It must gate any
// trust, verification, or deletion of recovery state.
func promotionBackupPathMatches(p *promotionState) error {
	if p.BackupPath == "" {
		return nil
	}
	expected, err := promotionExpectedBackupPath(p)
	if err != nil {
		return err
	}
	if filepath.Clean(p.BackupPath) != filepath.Clean(expected) {
		return fmt.Errorf("persisted recovery path %q does not match the deterministic promotion location %q; refusing to trust or delete it", p.BackupPath, expected)
	}
	return nil
}

// promotionRecoveryDirs returns the expected per-promotion recovery key
// directory and its direct .promotion-recovery parent. Cleanup removes only
// these direct parents, never arbitrary ancestors of a persisted path.
func promotionRecoveryDirs(backupPath string) (keyDir, parentDir string) {
	keyDir = filepath.Dir(backupPath)
	return keyDir, filepath.Dir(keyDir)
}

func (e *Engine) promotionHash(ctx context.Context, path string) (string, int64, error) {
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return "", 0, err
	}
	before, err := os.Lstat(resolved)
	if err != nil {
		return "", 0, err
	}
	if !before.Mode().IsRegular() {
		return "", 0, fmt.Errorf("promotion path is not a regular file: %s", path)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, opened) {
		return "", 0, fmt.Errorf("file changed while opening: %s", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, &promotionContextReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(resolved)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, after) || before.Size() != n || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("file changed while hashing: %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

type promotionContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *promotionContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (e *Engine) verifyPromotionHash(ctx context.Context, path, expected string) error {
	actual, _, err := e.promotionHash(ctx, path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("integrity violation: SHA-256 changed for %s", path)
	}
	return nil
}

const promotionStableHashAttempts = 5

// promotionHashStable retries the strict, single-shot promotionHash when the
// filesystem exposes transiently inconsistent metadata (a common race on
// network mounts immediately after a writer closes). It never relaxes the
// integrity contract: a successful result is always a full stable read, and a
// file that never settles is reported as an error after bounded retries.
func (e *Engine) promotionHashStable(ctx context.Context, path string) (string, int64, error) {
	var lastErr error
	for attempt := 0; attempt < promotionStableHashAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		hash, n, err := e.promotionHash(ctx, path)
		if err == nil {
			return hash, n, nil
		}
		if !transientPromotionHashError(err) {
			return "", 0, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return "", 0, lastErr
}

func transientPromotionHashError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "file changed while opening") || strings.Contains(msg, "file changed while hashing")
}

func (e *Engine) verifyPromotionHashStable(ctx context.Context, path, expected string) error {
	actual, _, err := e.promotionHashStable(ctx, path)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("integrity violation: SHA-256 changed for %s", path)
	}
	return nil
}

func (e *Engine) stepPromotePlan(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	if !e.AllowDestructive() {
		return promoteFailed(fmt.Errorf("allow_destructive must be enabled for candidate promotion"))
	}
	id := strings.TrimSpace(getString(ec.Inputs, "transcode_action_id"))
	seriesID := getInt(ec.Inputs, "series_id")
	if id == "" || seriesID <= 0 {
		return promoteFailed(fmt.Errorf("transcode_action_id and a positive series_id are required"))
	}
	inst, err := e.deps.Store.GetActionInstance(id)
	if err != nil {
		return promoteFailed(err)
	}
	if inst == nil {
		return promoteFailed(fmt.Errorf("transcode action was not found"))
	}
	if inst.ActionName != "transcode_media" || inst.Status != StatusCompleted {
		return promoteFailed(fmt.Errorf("promotion requires a completed transcode_media action"))
	}
	source := parseExecutionContext(inst, e)
	if getBool(source.State, "skip_transcode") {
		return promoteFailed(fmt.Errorf("the source action did not produce a candidate"))
	}
	p := &promotionState{SourceActionID: id, Service: getString(ec.Inputs, "service"), SeriesID: seriesID, OriginalPath: getString(source.State, "resolved_path"), CandidatePath: getString(source.State, "candidate_path"), OriginalSHA: getString(source.State, "original_sha256"), Commands: map[string]*promotionCommand{}}
	if p.Service == "" {
		p.Service = "sonarr"
	}
	if p.CandidatePath == "" {
		p.CandidatePath = getString(source.State, "output_path")
	}
	if p.OriginalSHA == "" || p.OriginalPath == "" || p.CandidatePath == "" || p.OriginalPath == p.CandidatePath {
		return promoteFailed(fmt.Errorf("completed transcode is missing an original/candidate integrity baseline"))
	}
	if !temporaryPromotionPath(p.CandidatePath) || temporaryPromotionPath(p.OriginalPath) {
		return promoteFailed(fmt.Errorf("candidate must be in .navigatorr-candidates and original must be in the active library"))
	}
	if _, err := e.promotionPath(p.OriginalPath, true); err != nil {
		return promoteFailed(err)
	}
	if _, err := e.promotionPath(p.CandidatePath, true); err != nil {
		return promoteFailed(err)
	}
	actual, size, err := e.promotionHash(ctx, p.OriginalPath)
	if err != nil {
		return promoteFailed(err)
	}
	if actual != p.OriginalSHA {
		return promoteFailed(fmt.Errorf("integrity violation: original SHA-256 changed since transcoding"))
	}
	p.OriginalBytes = size
	p.CandidateSHA, p.CandidateBytes, err = e.promotionHash(ctx, p.CandidatePath)
	if err != nil {
		return promoteFailed(err)
	}
	// Reuse the full transcode stream validator without accepting any prior
	// loss override. Approval is for a validated replacement, not lost streams.
	// Promotion intentionally performs a real full inspection of the published
	// candidate; the normal transcode pipeline's post-publish step stays
	// lightweight (the worker already validated the local candidate).
	validation, err := e.validateCandidateDetailed(ctx, source)
	if err != nil {
		return promoteFailed(err)
	}
	if validation.Status != StepCompleted {
		return promoteFailed(fmt.Errorf("candidate failed promotion validation: %s %s", validation.Error, validation.WaitingReason))
	}
	// The hash must describe the same bytes that were probed and validated.
	// Capturing a new digest only after probing could bless an intervening
	// candidate modification as the promotion's integrity baseline.
	if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	svc, err := e.promotionService(p)
	if err != nil {
		return promoteFailed(err)
	}
	var series struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
	}
	data, err := svc.Get(ctx, "/api/v3/series/"+strconv.Itoa(seriesID), nil)
	if err != nil {
		return promoteFailed(fmt.Errorf("resolve Sonarr series: %w", err))
	}
	if err := json.Unmarshal(data, &series); err != nil || series.ID != seriesID {
		return promoteFailed(fmt.Errorf("Sonarr series response does not match series_id"))
	}
	p.SeriesPath = filepath.Clean(series.Path)
	if _, err := e.promotionPath(p.SeriesPath, true); err != nil {
		return promoteFailed(err)
	}
	if !withinPromotionPath(p.SeriesPath, p.OriginalPath) || !withinPromotionPath(p.SeriesPath, p.CandidatePath) {
		return promoteFailed(fmt.Errorf("original and candidate must both be inside Sonarr's series path; remote path aliases are not guessed"))
	}
	snap, err := e.promotionSnapshot(ctx, svc, p)
	if err != nil {
		return promoteFailed(err)
	}
	for _, f := range snap.Files {
		if filepath.Clean(f.Path) == p.OriginalPath {
			if p.OriginalFileID != 0 {
				return promoteFailed(fmt.Errorf("multiple Sonarr episodeFiles point to the original path"))
			}
			p.OriginalFileID = f.ID
			p.Quality, p.Languages, p.ReleaseGroup = f.Quality, f.Languages, f.ReleaseGroup
		}
	}
	if p.OriginalFileID <= 0 {
		return promoteFailed(fmt.Errorf("original is not a Sonarr episodeFile in the selected series"))
	}
	for _, ep := range snap.Episodes {
		if ep.EpisodeFileID == p.OriginalFileID {
			p.EpisodeIDs = append(p.EpisodeIDs, ep.ID)
		}
	}
	sort.Ints(p.EpisodeIDs)
	if len(p.EpisodeIDs) == 0 {
		return promoteFailed(fmt.Errorf("no Sonarr episodes reference the original episodeFile"))
	}
	if len(p.Quality) == 0 || string(p.Quality) == "null" {
		return promoteFailed(fmt.Errorf("Sonarr quality metadata is missing; cannot safely construct ManualImport"))
	}
	if len(p.Languages) == 0 || string(p.Languages) == "null" {
		p.Languages = json.RawMessage("[]")
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"promotion": p, "candidate_path": p.CandidatePath, "original_path": p.OriginalPath, "affected_episode_ids": p.EpisodeIDs, "estimated_bytes_saved": p.OriginalBytes - p.CandidateBytes}}, nil
}

func (e *Engine) stepPromotePreserve(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	if err := e.promotionOriginalStillActive(ctx, svc, p); err != nil {
		return promoteFailed(err)
	}
	claimed, err := e.deps.Store.ClaimPromotionOriginal(p.Service, p.OriginalFileID, ec.InstanceID)
	if err != nil {
		return promoteFailed(err)
	}
	if !claimed {
		return promoteFailed(fmt.Errorf("the original Sonarr episodeFile is reserved by another promotion; resume its recorded action before trying another candidate"))
	}
	if p.BackupPath == "" {
		expected, err := promotionExpectedBackupPath(p)
		if err != nil {
			return promoteFailed(err)
		}
		p.BackupPath = expected
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	}
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
		return promoteFailed(err)
	}
	if err := os.MkdirAll(filepath.Dir(p.BackupPath), 0700); err != nil {
		return promoteFailed(err)
	}
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
		return promoteFailed(err)
	}
	if _, err := os.Lstat(p.BackupPath); err == nil {
		if err := e.verifyPromotionHash(ctx, p.BackupPath, p.OriginalSHA); err != nil {
			return promoteFailed(err)
		}
		if err := e.promotionRemovePartial(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
	} else if !os.IsNotExist(err) {
		return promoteFailed(err)
	} else {
		partial := p.BackupPath + ".partial"
		if _, err := e.promotionPath(partial, true); err != nil {
			return promoteFailed(err)
		}
		if info, err := os.Lstat(partial); err == nil {
			if !info.Mode().IsRegular() {
				return promoteFailed(fmt.Errorf("recovery partial is not a regular file"))
			}
			// Retry/re-entry: a previous attempt may have completed the private
			// copy but crashed before the atomic rename. Reuse a stable, exact
			// copy instead of discarding it and copying the original again.
			// An unstable or mismatched partial is never trusted for reuse.
			if info.Size() == p.OriginalBytes && p.OriginalBytes > 0 {
				hash, n, hashErr := e.promotionHashStable(ctx, partial)
				if hashErr == nil && hash == p.OriginalSHA && n == p.OriginalBytes {
					if err := e.promotionPublishPartial(ctx, ec, p, partial); err != nil {
						return promoteFailed(err)
					}
					goto preserved
				}
			}
			// No Sonarr mutation has started in this step. The original was just
			// reverified, so an interrupted private copy can be rebuilt safely.
			if err := os.Remove(partial); err != nil {
				return promoteFailed(err)
			}
		} else if !os.IsNotExist(err) {
			return promoteFailed(err)
		}
		if err := e.promotionCopyToPartial(ctx, p.OriginalPath, partial); err != nil {
			return promoteFailed(err)
		}
		// promotionCopyToPartial returns only after the destination writer is
		// flushed and closed, so this hashes a finished file, never an active
		// writer. Stable hashing absorbs transient post-close metadata races.
		if err := e.verifyPromotionHashStable(ctx, partial, p.OriginalSHA); err != nil {
			return promoteFailed(err)
		}
		// This is an independent copy in the promotion's private directory.
		// Atomic rename needs no NAS hard-link support. The durable original
		// reservation prevents another promotion from publishing here.
		if err := e.promotionPublishPartial(ctx, ec, p, partial); err != nil {
			return promoteFailed(err)
		}
	}
preserved:
	p.BackupVerified = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"recovery_path": p.BackupPath, "recovery_retained": true}}, nil
}

// promotionCopyToPartial copies the original into the private recovery
// partial. It returns only after the destination has been synced and closed,
// so callers must not hash or publish the partial before it returns.
func (e *Engine) promotionCopyToPartial(ctx context.Context, srcPath, partial string) error {
	if _, err := e.promotionPath(partial, true); err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = src.Close()
		return err
	}
	_, copyErr := io.Copy(dst, &promotionContextReader{ctx: ctx, r: src})
	_ = src.Close()
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	closeErr := dst.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if hook := e.promotionCopyBoundaryHook; hook != nil {
		hook(partial)
	}
	return nil
}

func (e *Engine) promotionPublishPartial(ctx context.Context, ec *ExecutionContext, p *promotionState, partial string) error {
	if err := promotionBackupPathMatches(p); err != nil {
		return err
	}
	if _, err := e.promotionPath(partial, true); err != nil {
		return err
	}
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
		return err
	}
	if _, err := os.Lstat(p.BackupPath); err == nil {
		if err := e.verifyPromotionHash(ctx, p.BackupPath, p.OriginalSHA); err != nil {
			return err
		}
		return e.promotionRemovePartial(ctx, ec, p)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(partial, p.BackupPath); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(p.BackupPath)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (e *Engine) promotionRemovePartial(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	if err := promotionBackupPathMatches(p); err != nil {
		return err
	}
	partial := p.BackupPath + ".partial"
	if _, err := e.promotionPath(partial, true); err != nil {
		return err
	}
	if _, err := os.Lstat(partial); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := e.verifyPromotionHashStable(ctx, partial, p.OriginalSHA); err != nil {
		return err
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Remove(partial)
}

func (e *Engine) promotionVerifyRecovered(ctx context.Context, p *promotionState) error {
	if !p.BackupVerified || p.BackupPath == "" {
		return fmt.Errorf("verified recovery copy is required before Sonarr mutations")
	}
	if err := promotionBackupPathMatches(p); err != nil {
		return err
	}
	return e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA)
}

func (e *Engine) promotionInspectAdopted(ctx context.Context, path, expectedSHA string) error {
	if _, err := e.promotionPath(path, false); err != nil {
		return err
	}
	rep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, path)
	if err != nil {
		return err
	}
	if !rep.Probed || len(rep.Video) == 0 || !promotionHEVC(rep.Video[0].Codec) {
		return fmt.Errorf("Sonarr's physical file is not a verified HEVC candidate")
	}
	return e.verifyPromotionHash(ctx, path, expectedSHA)
}

func promotionHEVC(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "hevc", "h265", "h.265", "x265":
		return true
	}
	return false
}
