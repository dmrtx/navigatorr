package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
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

func (e *Engine) promotionLstat(path string) (os.FileInfo, error) {
	if e.promotionLstatHook != nil {
		return e.promotionLstatHook(path)
	}
	return os.Lstat(path)
}

func (e *Engine) promotionHash(ctx context.Context, path string) (string, int64, error) {
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return "", 0, err
	}
	before, err := e.promotionLstat(resolved)
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
		return "", 0, &promotionFileChangedError{path: path, phase: "opening"}
	}
	h := sha256.New()
	observer, _ := ctx.Value(promotionHashProgressKey{}).(promotionHashProgress)
	if observer != nil {
		if err := observer(path, 0, opened.Size()); err != nil {
			return "", 0, err
		}
	}
	n, err := e.hashMedia(ctx, h, &promotionContextReader{ctx: ctx, r: f, path: path, total: opened.Size(), progress: observer})
	if hook := e.promotionReadHook; hook != nil {
		hook(path, n)
	}
	if err != nil {
		return "", 0, err
	}
	after, err := e.promotionLstat(resolved)
	if err != nil {
		return "", 0, err
	}
	finished, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	// Use the opened descriptor for size/mtime stability. Pathname attributes
	// may still describe the pre-close NAS copy even though the descriptor sees
	// the complete file. Path stats prove identity, never completion metadata.
	if !os.SameFile(opened, after) || !os.SameFile(opened, finished) || opened.Size() != n || finished.Size() != n || !opened.ModTime().Equal(finished.ModTime()) {
		return "", 0, &promotionFileChangedError{path: path, phase: "hashing"}
	}
	sum := hex.EncodeToString(h.Sum(nil))
	promotionRememberHash(ctx, path, sum, opened, finished)
	return sum, n, nil
}

type promotionContextReader struct {
	ctx         context.Context
	r           io.Reader
	path        string
	total, read int64
	progress    promotionHashProgress
}

func (r *promotionContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	r.read += int64(n)
	if r.progress != nil && n > 0 {
		if observeErr := r.progress(r.path, r.read, r.total); observeErr != nil {
			return n, observeErr
		}
	}
	return n, err
}

// verifyPromotionHash and verifyPromotionHashStable read the whole file
// unless this same continuous execution already did and the file's strict
// fingerprint proves it unchanged since.
func (e *Engine) verifyPromotionHash(ctx context.Context, path, expected string) error {
	if e.promotionStillVerified(ctx, path, expected) {
		return nil
	}
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
		// Each retry is another whole read; make them visible.
		log.Printf("promotion: %v (full read %d of %d); reading it again", err, attempt+1, promotionStableHashAttempts)
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return "", 0, lastErr
}

// promotionFileChangedError is a file whose identity or size moved while it
// was opened or hashed. The hasher serves the original, the candidate and the
// recovery copies alike and does not know which it holds, so the error carries
// no failure class: it is a statement about a path, not about the source. It
// is matched by type, never by reading its text.
type promotionFileChangedError struct{ path, phase string }

func (e *promotionFileChangedError) Error() string {
	return fmt.Sprintf("file changed while %s: %s", e.phase, e.path)
}

func transientPromotionHashError(err error) bool {
	var changed *promotionFileChangedError
	return errors.As(err, &changed)
}

func (e *Engine) verifyPromotionHashStable(ctx context.Context, path, expected string) error {
	if e.promotionStillVerified(ctx, path, expected) {
		return nil
	}
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
	movieID := getInt(ec.Inputs, "movie_id")
	if movieID > 0 {
		if seriesID > 0 || getString(ec.Inputs, "service") != "radarr" || getString(ec.Inputs, "batch_promote_parent_id") != "" {
			return promoteFailed(fmt.Errorf("movie promotion requires service=radarr, movie_id and no series/batch inputs"))
		}
		seriesID = movieID // Durable media claims are namespaced by service.
	} else if getString(ec.Inputs, "service") == "radarr" {
		return promoteFailed(fmt.Errorf("Radarr promotion requires movie_id"))
	}
	if id == "" || (seriesID <= 0 && getString(ec.Inputs, "service") != "filesystem") {
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
	if getBool(source.State, candidateRemovedKey) {
		cleanup, _ := source.State[candidateCleanupKey].(map[string]any)
		return promoteFailed(fmt.Errorf("the candidate was removed after it was %s and can no longer be promoted", getString(cleanup, "reason")))
	}
	p := &promotionState{SourceActionID: id, Service: getString(ec.Inputs, "service"), SeriesID: seriesID, OriginalPath: getString(source.State, "resolved_path"), CandidatePath: getString(source.State, "candidate_path"), OriginalSHA: getString(source.State, "original_sha256"), Commands: map[string]*promotionCommand{}}
	p.MovieID = movieID
	if getString(source.State, "mode") == "compact" {
		p.RegrabWarning = CompactRegrabWarning
	}
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
	// The original is hashed once, after approval, by preserve_original: an
	// approval can wait indefinitely, so a digest taken here would only be
	// read again. Plan proves it is still the transcoded regular file.
	original, err := e.promotionStatIdentity(p.OriginalPath)
	if err != nil {
		return promoteFailed(fmt.Errorf("original is not a readable regular file: %w", err))
	}
	if expected := getInt64(stateObject(source.State, "original"), "size_bytes"); expected > 0 && original.Size() != expected {
		return promoteFailed(fmt.Errorf("integrity violation: original size changed since transcoding"))
	}
	p.OriginalBytes = original.Size()
	p.CandidateSHA, p.CandidateBytes, err = e.promotionHash(ctx, p.CandidatePath)
	if err != nil {
		return promoteFailed(err)
	}
	// Modern workers attest the candidate before publication. Do not replace
	// that baseline with a fresh digest of potentially changed NAS bytes.
	if expected := getString(source.State, "candidate_sha256"); expected != "" && expected != p.CandidateSHA {
		return promoteFailed(fmt.Errorf("candidate SHA-256 changed since worker validation; promotion blocked"))
	}
	if expected := getInt64(source.State, "candidate_size_bytes"); expected > 0 && expected != p.CandidateBytes {
		return promoteFailed(fmt.Errorf("candidate size changed since worker validation; promotion blocked"))
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
	if p.Service == "filesystem" {
		if seriesID > 0 || movieID > 0 {
			return promoteFailed(fmt.Errorf("filesystem promotion cannot include library IDs"))
		}
		p.SeriesPath = filepath.Dir(p.OriginalPath)
		p.NewPath = strings.TrimSuffix(p.OriginalPath, filepath.Ext(p.OriginalPath)) + filepath.Ext(p.CandidatePath)
		if _, err := e.promotionPath(p.NewPath, true); err != nil {
			return promoteFailed(err)
		}
		if p.NewPath != p.OriginalPath {
			if _, err := os.Lstat(p.NewPath); !os.IsNotExist(err) {
				return promoteFailed(fmt.Errorf("filesystem replacement destination already exists or cannot be inspected"))
			}
		}
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"promotion": p, "candidate_path": p.CandidatePath, "original_path": p.OriginalPath, "final_path": p.NewPath, "estimated_bytes_saved": p.OriginalBytes - p.CandidateBytes}}, nil
	}
	svc, err := e.promotionService(p)
	if err != nil {
		return promoteFailed(err)
	}
	var series struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
	}
	data, err := svc.Get(ctx, p.mediaEndpoint(), nil)
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
	if p.RegrabWarning != "" {
		p.arrTracking = readArrTracking(ctx, svc, p.mediaEndpoint())
		if p.Monitored != nil && *p.Monitored && p.MovieID == 0 {
			// Sonarr searches per episode: an unmonitored episode in a
			// monitored series is not grabbed again.
			monitored := false
			for _, ep := range snap.Episodes {
				monitored = monitored || ep.EpisodeFileID == p.OriginalFileID && ep.Monitored
			}
			p.Monitored = &monitored
		}
	}
	return StepResult{Status: StepCompleted, Outputs: map[string]any{"promotion": p, "candidate_path": p.CandidatePath, "original_path": p.OriginalPath, "affected_episode_ids": p.EpisodeIDs, "estimated_bytes_saved": p.OriginalBytes - p.CandidateBytes}}, nil
}

// promotionPreserveProgress persists the preserve step's whole-file reads as
// coordinator work. On a large original the two re-hashes, the recovery copy
// and its verification are many minutes of NAS IO; without it the step looks
// frozen. Observations are display only and never replace a SHA-256 check.
type promotionPreserveProgress struct {
	e     *Engine
	ec    *ExecutionContext
	p     *promotionState
	phase string
	last  time.Time
}

func (w *promotionPreserveProgress) report(ctx context.Context, phase string, read, total int64) error {
	if phase == w.phase && read != 0 && read != total && time.Since(w.last) < time.Second {
		return nil
	}
	w.phase, w.last = phase, time.Now()
	progress := map[string]any{"phase": phase, "total_bytes": total, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if phase != "waiting_for_hash_slot" {
		progress["bytes_read"] = read
	}
	w.ec.State["work_progress"] = progress
	return w.e.persistExecutionState(ctx, w.ec)
}

// hashes observes promotionHash, which reports zero bytes before it queues
// for a hash slot. Only the step's own files are named; any other read is
// left unreported rather than mislabelled.
func (w *promotionPreserveProgress) hashes(ctx context.Context) promotionHashProgress {
	return func(path string, read, total int64) error {
		if read == 0 {
			return w.report(ctx, "waiting_for_hash_slot", 0, total)
		}
		switch path {
		case w.p.OriginalPath:
			return w.report(ctx, "verifying_original", read, total)
		case w.p.CandidatePath:
			return w.report(ctx, "verifying_candidate", read, total)
		case w.p.BackupPath, w.p.BackupPath + ".partial":
			return w.report(ctx, "verifying_recovery", read, total)
		}
		return nil
	}
}

func (w *promotionPreserveProgress) copies(ctx context.Context) promotionHashProgress {
	return func(_ string, read, total int64) error {
		if read == 0 {
			return w.report(ctx, "waiting_for_hash_slot", 0, total)
		}
		return w.report(ctx, "copying_recovery", read, total)
	}
}

func (e *Engine) stepPromotePreserve(ctx context.Context, ec *ExecutionContext) (StepResult, error) {
	p, svc, err := e.promotionMutation(ec)
	if err != nil {
		return promoteFailed(err)
	}
	progress := &promotionPreserveProgress{e: e, ec: ec, p: p}
	ctx = context.WithValue(ctx, promotionHashProgressKey{}, progress.hashes(ctx))
	// The step's own result replaces this progress; never leave a finished or
	// abandoned read looking current.
	defer delete(ec.State, "work_progress")
	if err := e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA); err != nil {
		return promoteFailed(err)
	}
	if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		return promoteFailed(err)
	}
	if p.Service != "filesystem" {
		if err := e.promotionOriginalStillActive(ctx, svc, p); err != nil {
			return promoteFailed(err)
		}
	}
	// Every adapter reserves the same original bytes, preventing a local and
	// library promotion from concurrently replacing one physical source.
	claimed, err := e.deps.Store.ClaimPromotionOriginal(promotionPhysicalIdentity(p), 1, ec.InstanceID)
	if err != nil || !claimed {
		if err == nil {
			err = fmt.Errorf("original file is reserved by another promotion")
		}
		return promoteFailed(err)
	}
	if p.Service != "filesystem" {
		claimed, err = e.deps.Store.ClaimPromotionOriginal(p.Service, p.OriginalFileID, ec.InstanceID)
	}
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
	if p.RecoveryMode == "" {
		p.RecoveryMode, p.RecoveryModeReason, p.RecoveryModeDetail = e.promotionChooseRecovery(ctx, p)
	}
	if p.movesAside() && !p.MovedAside {
		// The original stays where it is, verified and reserved, until right
		// before it would be removed; see promotionMoveAside. This step's
		// full hash is the identity that move must find again.
		p.OriginalFingerprint = promotionKnownFingerprint(ctx, p.OriginalPath, p.OriginalSHA)
		if p.OriginalFingerprint == nil {
			if p.MoveAsideStartedAt != "" {
				return promoteFailed(fmt.Errorf("the original has no stable file identity after a move aside began; recovery retained"))
			}
			p.RecoveryMode, p.RecoveryModeReason = promotionRecoveryCopy, recoveryReasonNoFingerprint
		}
	}
	if p.movesAside() {
		// No recovery exists yet: it is the original once moved aside, and
		// promotionMoveAside records it as verified then.
		delete(ec.State, "work_progress")
		if err := e.savePromotion(ctx, ec, p); err != nil {
			return promoteFailed(err)
		}
		return StepResult{Status: StepCompleted, Outputs: map[string]any{"recovery_path": p.BackupPath, "recovery_mode": p.RecoveryMode, "recovery_mode_reason": p.RecoveryModeReason, "recovery_mode_detail": p.RecoveryModeDetail}}, nil
	}
	if err := e.promotionMakeRecoveryCopy(ctx, ec, p, progress); err != nil {
		return promoteFailed(err)
	}
	delete(ec.State, "work_progress")
	p.RecoveryVerifiedBeforeReplacement = true
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return promoteFailed(err)
	}
	out := map[string]any{"recovery_path": p.BackupPath, "recovery_mode": p.RecoveryMode, "recovery_mode_reason": p.RecoveryModeReason, "recovery_mode_detail": p.RecoveryModeDetail, "recovery_retained": true}
	if p.RecoveryCopy != nil {
		out["recovery_copy"] = p.RecoveryCopy
	}
	return StepResult{Status: StepCompleted, Outputs: out}, nil
}

// Why a promotion makes a recovery copy instead of moving the original
// aside. They are stored with the promotion so a copy that was not expected
// can be explained afterwards; none carries a path.
const (
	recoveryReasonMoveAside     = "original_moves_aside"
	recoveryReasonAlwaysCopy    = "copy_forced"
	recoveryReasonNoFingerprint = "no_stable_identity_for_the_original"
	recoveryReasonLeftover      = "earlier_recovery_file_present"
	recoveryReasonMountProbe    = "mount_probe_failed"
	recoveryReasonRenameRefused = "move_aside_refused"
	recoveryReasonLegacy        = "recorded_before_the_mode_existed"
)

// promotionChooseRecovery moves the original aside unless something rules it
// out: a copy left by an earlier attempt or release keeps the copy design, as
// does a file without a stable identity to follow through the rename. The
// second result says which, and the third, for a failed mount probe, what the
// probe observed (never a path).
func (e *Engine) promotionChooseRecovery(ctx context.Context, p *promotionState) (string, string, string) {
	if e.promotionAlwaysCopy {
		return promotionRecoveryCopy, recoveryReasonAlwaysCopy, ""
	}
	if promotionKnownFingerprint(ctx, p.OriginalPath, p.OriginalSHA) == nil {
		return promotionRecoveryCopy, recoveryReasonNoFingerprint, ""
	}
	for _, path := range []string{p.BackupPath, p.BackupPath + ".partial"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return promotionRecoveryCopy, recoveryReasonLeftover, ""
		}
	}
	if err := e.promotionProbeMount(filepath.Dir(p.BackupPath)); err != nil {
		detail := promotionProbeDetail(err)
		log.Printf("promotion %s: the recovery directory's mount does not keep file identity (%s); making a recovery copy instead of moving the original aside", p.Service, detail)
		return promotionRecoveryCopy, recoveryReasonMountProbe, detail
	}
	return promotionRecoveryMoveAside, recoveryReasonMoveAside, ""
}

// promotionMakeRecoveryCopy publishes a verified copy of the original at
// BackupPath, reusing one an earlier attempt finished. The caller has just
// verified the original.
func (e *Engine) promotionMakeRecoveryCopy(ctx context.Context, ec *ExecutionContext, p *promotionState, progress *promotionPreserveProgress) error {
	if _, err := e.promotionPath(p.BackupPath, true); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.BackupPath), 0700); err != nil {
		return err
	}
	if _, err := os.Lstat(p.BackupPath); err == nil {
		if err := e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA); err != nil {
			return err
		}
		if err := e.promotionRemovePartial(ctx, ec, p); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		partial := p.BackupPath + ".partial"
		if _, err := e.promotionPath(partial, true); err != nil {
			return err
		}
		if info, err := os.Lstat(partial); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("recovery partial is not a regular file")
			}
			// Retry/re-entry: a previous attempt may have completed the private
			// copy but crashed before the atomic rename. Reuse a stable, exact
			// copy instead of discarding it and copying the original again.
			// An unstable or mismatched partial is never trusted for reuse.
			if info.Size() == p.OriginalBytes && p.OriginalBytes > 0 {
				hash, n, hashErr := e.promotionHashStable(ctx, partial)
				if hashErr == nil && hash == p.OriginalSHA && n == p.OriginalBytes {
					if err := e.promotionPublishPartial(ctx, ec, p, partial); err != nil {
						return err
					}
					return e.promotionRecordBackup(ctx, ec, p)
				}
			}
			// No library mutation removed the original. It was just
			// reverified, so an interrupted private copy can be rebuilt safely.
			if err := os.Remove(partial); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		started := time.Now()
		copied, err := e.promotionCopyToPartial(ctx, p.OriginalPath, partial, progress.copies(ctx))
		if err != nil {
			return err
		}
		copied.DurationMs = time.Since(started).Milliseconds()
		p.RecoveryCopy = &copied
		log.Printf("promotion %s: recovery copy made by %s in %dms (clone: %s)", ec.InstanceID, copied.Method, copied.DurationMs, promotionOr(copied.CloneError, "used"))
		// promotionCopyToPartial returns only after the destination writer is
		// flushed and closed, so this hashes a finished file, never an active
		// writer. Stable hashing absorbs transient post-close metadata races.
		// However the copy was made, this full read-back is what proves it.
		if err := e.verifyPromotionHashStable(ctx, partial, p.OriginalSHA); err != nil {
			return err
		}
		// This is an independent copy in the promotion's private directory.
		// Atomic rename needs no NAS hard-link support. The durable original
		// reservation prevents another promotion from publishing here.
		if err := e.promotionPublishPartial(ctx, ec, p, partial); err != nil {
			return err
		}
	}
	return e.promotionRecordBackup(ctx, ec, p)
}

// promotionRecordBackup marks the published copy verified and keeps the
// fingerprint of its full read, so later steps can prove it unchanged in its
// private directory without reading it again.
func (e *Engine) promotionRecordBackup(ctx context.Context, ec *ExecutionContext, p *promotionState) error {
	if p.RecoveryMode == "" {
		p.RecoveryMode, p.RecoveryModeReason = promotionRecoveryCopy, recoveryReasonLegacy
	}
	p.BackupVerified = true
	p.BackupFingerprint = promotionKnownFingerprint(ctx, p.BackupPath, p.OriginalSHA)
	return e.savePromotion(ctx, ec, p)
}

func promotionOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// promotionCopyToPartial copies the original into the private recovery
// partial. It returns only after the destination has been synced and closed,
// so callers must not hash or publish the partial before it returns.
// progress, when set, observes the bytes copied, starting with zero before the
// copy queues for a hash slot. The copy is a clone, a kernel copy or a stream,
// whichever the filesystem accepts first; the result says which.
func (e *Engine) promotionCopyToPartial(ctx context.Context, srcPath, partial string, progress promotionHashProgress) (promotionCopyResult, error) {
	var result promotionCopyResult
	if _, err := e.promotionPath(partial, true); err != nil {
		return result, err
	}
	// The partial gets new bytes: whatever this execution knew of that path no
	// longer applies, and the read-back must be a full one.
	promotionForget(ctx, partial)
	src, err := os.Open(srcPath)
	if err != nil {
		return result, err
	}
	var total int64
	if info, err := src.Stat(); err == nil {
		total = info.Size()
	}
	if progress != nil {
		if err := progress(partial, 0, total); err != nil {
			_ = src.Close()
			return result, err
		}
	}
	dst, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		_ = src.Close()
		return result, err
	}
	result, copyErr := e.promotionCopyData(ctx, dst, src, partial, total, progress)
	if hook := e.promotionReadHook; hook != nil && result.Streamed > 0 {
		hook(srcPath, result.Streamed)
	}
	_ = src.Close()
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	closeErr := dst.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return result, copyErr
	}
	if hook := e.promotionCopyBoundaryHook; hook != nil {
		hook(partial)
	}
	return result, nil
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
		if err := e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA); err != nil {
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
	e.promotionCarryRenamed(ctx, partial, p.BackupPath)
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

// promotionVerifyRecovered proves the original is still recoverable. Until a
// move-aside promotion moves it, that is the verified original in place, read
// in full after any wait. A published recovery sits in its private 0700
// directory, so its fingerprint proves it unchanged since its full read.
func (e *Engine) promotionVerifyRecovered(ctx context.Context, p *promotionState) error {
	if err := promotionBackupPathMatches(p); err != nil {
		return err
	}
	if p.movesAside() && !p.MovedAside {
		if p.BackupPath == "" || p.OriginalFingerprint == nil {
			return fmt.Errorf("verified original is required before library mutations")
		}
		if p.MoveAsideStartedAt == "" {
			// Anything already there would block the move later; stop
			// before the library is changed instead.
			if _, err := os.Lstat(p.BackupPath); !os.IsNotExist(err) {
				return fmt.Errorf("the recovery path is occupied before the original was moved aside; recovery retained")
			}
		} else if _, err := os.Lstat(p.OriginalPath); os.IsNotExist(err) {
			// The rename may have finished before its checkpoint, which only
			// a restart reaches: read the moved file in full.
			return e.verifyPromotionHashStable(ctx, p.BackupPath, p.OriginalSHA)
		}
		return e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA)
	}
	if !p.BackupVerified || p.BackupPath == "" {
		return fmt.Errorf("verified recovery copy is required before Sonarr mutations")
	}
	_, err := e.promotionVerifyByIdentity(ctx, p.BackupPath, p.OriginalSHA, p.BackupFingerprint)
	return err
}

// Called only after finalization verifies the final library file and adoption.
// The old temporary path is optional: Sonarr or delayed NAS metadata may make
// its removal visible between discovery and hashing. Never apply this tolerance
// to the required final file or recovery copy.
func (e *Engine) promotionRemoveLeftoverCandidate(ctx context.Context, ec *ExecutionContext, p *promotionState, snap *promotionLibrary) error {
	if _, err := e.promotionPath(p.CandidatePath, true); err != nil {
		return err
	}
	if _, err := e.promotionLstat(p.CandidatePath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := e.verifyPromotionHash(ctx, p.CandidatePath, p.CandidateSHA); err != nil {
		// ENOENT from opening/reading the optional leftover is authoritative.
		// Rechecking Lstat here is not a fresh confirmation on NAS mounts: it
		// can keep returning the cached, pre-rename inode after open failed.
		// Other errors, especially digest mismatches, still retain recovery.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, f := range snap.Files {
		if filepath.Clean(f.Path) == filepath.Clean(p.CandidatePath) {
			return fmt.Errorf("temporary candidate path is still registered in Sonarr")
		}
	}
	if err := e.savePromotion(ctx, ec, p); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(p.CandidatePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *Engine) promotionInspectAdopted(ctx context.Context, p *promotionState, path string) error {
	// Preserve the first physical HEVC check (including legacy checkpoints).
	// Once bound to CandidateSHA, identical bytes prove identical streams and
	// later adoption/rename checks need no repeated NAS media probe.
	if !p.CandidateMediaVerified {
		if _, err := e.promotionPath(path, false); err != nil {
			return err
		}
		rep, err := mediainspect.InspectDetailed(ctx, e.deps.Ffprobe, path)
		if err != nil {
			return err
		}
		if video := rep.MainVideo(); !rep.Probed || len(video) == 0 || !promotionHEVC(video[0].Codec) {
			return fmt.Errorf("Sonarr's physical file is not a verified HEVC candidate")
		}
	}
	if err := e.verifyPromotionHashStable(ctx, path, p.CandidateSHA); err != nil {
		return err
	}
	p.CandidateMediaVerified = true
	if fp := promotionKnownFingerprint(ctx, path, p.CandidateSHA); fp != nil {
		p.CandidateFingerprint = fp
	}
	return nil
}

func promotionHEVC(codec string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "hevc", "h265", "h.265", "x265":
		return true
	}
	return false
}
