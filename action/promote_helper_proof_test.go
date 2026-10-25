package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jakenesler/navigatorr/internal/nashelper"
)

var timeFields = regexp.MustCompile(`"(mtime|ctime)_ns":"[0-9]*"`)

var digestField = regexp.MustCompile(`"digest":"[^"]*"`)

// proofHelper serves the harness root through a real helper. The test
// filesystem has no btrfs identity, so every answer is rewritten to claim a
// complete one when complete is set: the proof logic is what is under test.
type proofHelper struct {
	ts      *httptest.Server
	hashes  atomic.Int64
	samples atomic.Int64
	down    atomic.Bool
	// notFound, when set, makes sample answer not_found for a request body
	// naming this substring.
	notFound atomic.Pointer[string]
	// justHashed lets the sample that closes a hash through: only a sample on
	// its own can be answered not_found.
	justHashed atomic.Bool
	// hashedBodies keeps the request body of every hash.
	hashedMu     sync.Mutex
	hashedBodies []string
	// delayed makes that many identity answers about a probe file report times
	// that differ from the last, as Samba's delayed write times do.
	delayed atomic.Int64
	// digest, when set, replaces the digest every sample answers with.
	digest atomic.Pointer[string]
}

// hashed counts the full hashes whose request named something containing sub.
func (ph *proofHelper) hashed(sub string) int {
	ph.hashedMu.Lock()
	defer ph.hashedMu.Unlock()
	n := 0
	for _, b := range ph.hashedBodies {
		if strings.Contains(b, sub) {
			n++
		}
	}
	return n
}

func attachProofHelper(t *testing.T, h *promotionHarness, complete bool) *proofHelper {
	t.Helper()
	srv, err := nashelper.NewServer(nashelper.Config{Roots: []string{h.root}, Token: helperTestToken})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	ph := &proofHelper{}
	ph.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ph.down.Load() {
			jsonReply(w, http.StatusServiceUnavailable, `{"error":{"code":"internal","message":"down"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/hash":
			ph.hashes.Add(1)
			ph.hashedMu.Lock()
			ph.hashedBodies = append(ph.hashedBodies, string(body))
			ph.hashedMu.Unlock()
			ph.justHashed.Store(true)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sample":
			ph.samples.Add(1)
			if nf := ph.notFound.Load(); nf != nil && !ph.justHashed.Swap(false) && strings.Contains(string(body), *nf) {
				jsonReply(w, http.StatusNotFound, `{"error":{"code":"not_found","message":"x"}}`)
				return
			}
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		out := rec.Body.Bytes()
		// The test filesystem's own answer depends on the OS (Linux reports a
		// complete identity on ext4), so pin it either way.
		if r.URL.Path == "/v1/identity" && strings.Contains(string(body), ".helper-probe-") {
			if n := ph.delayed.Add(-1); n >= 0 {
				out = timeFields.ReplaceAll(out, []byte(`"${1}_ns":"9`+strconv.FormatInt(n, 10)+`"`))
			}
		}
		if d := ph.digest.Load(); d != nil && r.URL.Path == "/v1/sample" {
			out = digestField.ReplaceAll(out, []byte(`"digest":"`+*d+`"`))
		}
		if complete {
			out = bytes.ReplaceAll(out, []byte(`"complete":false`), []byte(`"complete":true`))
		} else {
			out = bytes.ReplaceAll(out, []byte(`"complete":true`), []byte(`"complete":false`))
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(out)
	}))
	t.Cleanup(ph.ts.Close)
	client, err := nashelper.NewClient(nashelper.ClientConfig{
		URL: ph.ts.URL, Token: helperTestToken,
		Mappings: []nashelper.PathMapping{{Local: h.root, Remote: h.root}},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.engine.deps.NASHelper = client
	return ph
}

// failMountProbe makes the mount proof unavailable, so a move aside can only
// come from the helper's proof.
func failMountProbe(h *promotionHarness) {
	h.engine.promotionMountProbeHook = func(string) error {
		return &promotionProbeFailure{Step: probeStepRenameInode, Fs: "smb2", Detail: "changed: mtime,ctime", Definitive: true}
	}
}

func resetHelperProbes() {
	helperProbeQuiet, helperProbePoll = 40*time.Millisecond, 5*time.Millisecond
	promotionHelperProbesMu.Lock()
	promotionHelperProbes = map[string]helperProbeVerdict{}
	promotionHelperProbesMu.Unlock()
}

func TestHelperProofMovesTheOriginalAsideWithoutReadingItOverTheMount(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	reads := countReads(h.engine)
	moves := 0
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		moves++
		return renamePromotionNoReplace(from, to)
	}
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted || moves != 1 || h.deletes != 1 {
		t.Fatalf("promotion: %s %s moves=%d deletes=%d", r.Status, r.Error, moves, h.deletes)
	}
	p, err := loadPromotion(&ExecutionContext{State: r.State})
	if err != nil {
		t.Fatal(err)
	}
	if p.RecoveryMode != promotionRecoveryMoveAside || p.RecoveryModeReason != recoveryReasonMoveAside || p.RecoveryProof != promotionProofHelper {
		t.Fatalf("recovery %q/%q proof %q", p.RecoveryMode, p.RecoveryModeReason, p.RecoveryProof)
	}
	if !p.MovedAside || p.RecoveryCopy != nil || p.BackupFingerprint != nil || p.BackupHelper == nil || !p.BackupHelper.Identity.Complete || !p.RecoveryCleanupCompleted {
		t.Fatalf("move-aside evidence: %+v", p)
	}
	if got := reads.of(h.original) + reads.of(p.BackupPath); got != 0 {
		t.Fatalf("%d bytes of the original were read through the mount", got)
	}
	if ph.hashes.Load() == 0 || ph.samples.Load() == 0 {
		t.Fatalf("helper not used: %d hashes, %d samples", ph.hashes.Load(), ph.samples.Load())
	}
}

func TestHelperProofWithoutACompleteIdentityKeepsTheMountProof(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	attachProofHelper(t, h, false)
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if p.RecoveryProof != "" || p.BackupHelper != nil || p.OriginalHelper != nil {
		t.Fatalf("an incomplete identity became a proof: %+v", p)
	}
	if p.RecoveryMode != promotionRecoveryMoveAside || p.BackupFingerprint == nil {
		t.Fatalf("the mount proof was not used: %+v", p)
	}
}

func TestHelperProofDownBeforeThePlanCopiesWhenTheMountCannotProve(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	ph.down.Store(true)
	failMountProbe(h)
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if p.RecoveryMode != promotionRecoveryCopy || p.RecoveryModeReason != recoveryReasonMountProbe || p.RecoveryProof != "" {
		t.Fatalf("recovery %+v", p)
	}
}

// The helper goes away between the rename and its confirmation: the moved
// file is read in full through the mount and accepted by its digest.
func TestHelperProofConfirmsByAFullReadWhenTheHelperIsGoneAfterTheRename(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	reads := countReads(h.engine)
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		err := renamePromotionNoReplace(from, to)
		ph.down.Store(true)
		return err
	}
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if !p.MovedAside || p.RecoveryProof != promotionProofHelper || p.RecoveryCopy != nil {
		t.Fatalf("evidence: %+v", p)
	}
	if got := reads.of(p.BackupPath); got < p.OriginalBytes {
		t.Fatalf("moved original read %d of %d bytes through the mount", got, p.OriginalBytes)
	}
}

// A helper answer never decides that the original is gone: the file is read
// in full instead, and the promotion goes on.
func TestHelperProofNotFoundBeforeTheRenameIsNotTheOriginalGone(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	r := h.run()
	needle := filepath.Base(h.original)
	ph.notFound.Store(&needle)
	hashesBefore := ph.hashes.Load()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if !p.MovedAside || p.OriginalRemovedByLibrary {
		t.Fatalf("evidence: %+v", p)
	}
	if ph.hashes.Load() == hashesBefore {
		t.Fatal("the original was not read in full after the helper could not sample it")
	}
}

// Touching the original after it was proved (ctime moves, bytes do not) costs
// a full read, and the move aside still goes on.
func TestHelperProofChangedCtimeReadsTheOriginalInFull(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	ph := attachProofHelper(t, h, true)
	failMountProbe(h)
	r := h.run()
	if err := os.Chmod(h.original, 0o640); err != nil {
		t.Fatal(err)
	}
	hashesBefore := ph.hashes.Load()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	if !movedAsideState(t, h, r.ID).MovedAside || ph.hashes.Load() == hashesBefore {
		t.Fatalf("moved %t, helper hashes %d -> %d", movedAsideState(t, h, r.ID).MovedAside, hashesBefore, ph.hashes.Load())
	}
}

// A crash leaves the intent durable on either side of the rename; neither side
// is blocked by a mount that reports another mtime.
func TestHelperProofResumesAfterACrashOnEitherSideOfTheRename(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_rename", true: "after_rename"}[renamed], func(t *testing.T) {
			resetHelperProbes()
			h := newPromotionHarness(t)
			ph := attachProofHelper(t, h, true)
			failMountProbe(h)
			crashed := false
			h.engine.promotionMoveAsideHook = func(from, to string) error {
				if crashed {
					return renamePromotionNoReplace(from, to)
				}
				crashed = true
				if renamed {
					if err := renamePromotionNoReplace(from, to); err != nil {
						return err
					}
					// What the mount reports for the moved file differs from what
					// the original had.
					later := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
					if err := os.Chtimes(to, later, later); err != nil {
						return err
					}
				}
				return errors.New("simulated crash")
			}
			r := h.run()
			r = h.finish(h.resume(r.ID, "approve"))
			if r.Status != StatusFailed || h.deletes != 0 {
				t.Fatalf("crash fixture: %s %s deletes=%d", r.Status, r.Error, h.deletes)
			}
			p := movedAsideState(t, h, r.ID)
			if p.MoveAsideStartedAt == "" || p.MovedAside || p.RecoveryProof != promotionProofHelper {
				t.Fatalf("intent not durable: %+v", p)
			}
			h.restart()
			before := ph.hashes.Load()
			r, err := h.engine.Retry(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r = h.finish(r); r.Status != StatusCompleted || h.imports != 1 || h.deletes != 1 {
				t.Fatalf("resume after crash: %s %s imports=%d deletes=%d", r.Status, r.Error, h.imports, h.deletes)
			}
			if ph.hashes.Load() == before {
				t.Fatal("a restart must read the original in full again")
			}
			if p = movedAsideState(t, h, r.ID); !p.MovedAside || p.BackupFingerprint != nil {
				t.Fatalf("evidence after resume: %+v", p)
			}
		})
	}
}

func TestHelperProofSameNamePublishFailureMovesTheOriginalBack(t *testing.T) {
	for _, scenario := range []string{"retry", "rename_before_checkpoint", "crash_after_move_back"} {
		t.Run(scenario, func(t *testing.T) {
			resetHelperProbes()
			h := newPromotionHarness(t)
			sameNameFixture(t, h)
			attachProofHelper(t, h, true)
			failMountProbe(h)
			stash := filepath.Join(h.root, "stash.mkv")
			h.engine.promotionMoveAsideHook = func(from, to string) error {
				if err := renamePromotionNoReplace(from, to); err != nil {
					return err
				}
				if scenario == "rename_before_checkpoint" {
					return syscall.EIO
				}
				return os.Rename(h.candidate, stash)
			}
			r := runLocalPromotion(t, h)
			r = h.resume(r.ID, "approve")
			if r.Status != StatusFailed || !strings.Contains(r.Error, "moved back to its library path") {
				t.Fatalf("failed publish: %s %s", r.Status, r.Error)
			}
			p := movedAsideState(t, h, r.ID)
			if err := testFileSHA(h.original, p.OriginalSHA); err != nil {
				t.Fatalf("original not back at its library path: %v", err)
			}
			if p.MovedAside || p.MoveAsideStartedAt != "" || p.MoveBackStartedAt != "" || p.BackupVerified || p.BackupHelper != nil || p.OriginalHelper == nil || p.RecoveryProof != promotionProofHelper {
				t.Fatalf("state after moving back: %+v", p)
			}
			if _, err := os.Lstat(h.candidate); os.IsNotExist(err) {
				if err := os.Rename(stash, h.candidate); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "crash_after_move_back" {
				rewritePromotion(t, h, r.ID, func(q *promotionState) {
					q.MovedAside, q.BackupVerified, q.BackupHelper = true, true, q.OriginalHelper
					q.MoveAsideStartedAt, q.MoveBackStartedAt = "2026-01-01T00:00:00Z", "2026-01-01T00:00:01Z"
				})
			}
			h.restart()
			h.engine.deps.Registry = nil
			r, err := h.engine.Retry(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != StatusCompleted {
				t.Fatalf("retry: %s %s", r.Status, r.Error)
			}
			p = movedAsideState(t, h, r.ID)
			if !p.MovedAside || !p.RecoveryCleanupCompleted {
				t.Fatalf("retry evidence: %+v", p)
			}
		})
	}
}

func TestHelperProofRestoreMovesTheOriginalBack(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_process", true: "after_restart"}[restarted], func(t *testing.T) {
			resetHelperProbes()
			h := newPromotionHarness(t)
			ph := attachProofHelper(t, h, true)
			failMountProbe(h)
			sameNameFixture(t, h)
			stash := h.candidate + ".stash"
			h.engine.promotionMoveAsideHook = func(from, to string) error {
				if err := renamePromotionNoReplace(from, to); err != nil {
					return err
				}
				return os.Rename(h.candidate, stash)
			}
			r := runLocalPromotion(t, h)
			if r = h.resume(r.ID, "approve"); r.Status != StatusFailed {
				t.Fatalf("failed publish: %s %s", r.Status, r.Error)
			}
			p := movedAsideState(t, h, r.ID)
			if p.RecoveryProof != promotionProofHelper {
				t.Fatalf("proof %q", p.RecoveryProof)
			}
			// A crash instead of the automatic move back: the original is at its
			// recovery path and the library path is empty.
			if err := os.Rename(h.original, p.BackupPath); err != nil {
				t.Fatal(err)
			}
			rewritePromotion(t, h, r.ID, func(q *promotionState) {
				q.MovedAside, q.BackupVerified, q.BackupHelper = true, true, q.OriginalHelper
				q.MoveAsideStartedAt = "2026-01-01T00:00:00Z"
				q.MoveBackStartedAt = ""
			})
			if _, err := h.engine.Cancel(context.Background(), r.ID, "stop"); err != nil {
				t.Fatal(err)
			}
			if restarted {
				h.restart()
			}
			before := ph.hashes.Load()
			if _, err := h.engine.RestoreTranscodeBackup(context.Background(), r.ID); err != nil {
				t.Fatalf("restore: %v", err)
			}
			p = movedAsideState(t, h, r.ID)
			if err := testFileSHA(h.original, p.OriginalSHA); err != nil {
				t.Fatalf("original not restored: %v", err)
			}
			if p.MovedAside || (!restarted && p.OriginalHelper == nil) || p.BackupHelper != nil {
				t.Fatalf("state after restore: %+v", p)
			}
			// The recovery's change time moved with the manual rename: either way
			// the file was read in full before it was moved.
			if ph.hashes.Load() == before {
				t.Fatal("a recovery that was renamed by hand was moved back without a full read")
			}
		})
	}
}

func TestHelperProofDuplicateDiscardRefusesAMovedAsideOriginal(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	attachProofHelper(t, h, true)
	failMountProbe(h)
	h.engine.promotionMoveAsideHook = func(from, to string) error { return errors.New("simulated crash") }
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	inst, err := h.st.GetActionInstance(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.duplicateBackupGuard(inst, parseExecutionContext(inst, h.engine)); err == nil || !strings.Contains(err.Error(), "moved-aside") {
		t.Fatalf("guard: %v", err)
	}
}

func TestHelperProofKeepsTheProofOfAReadWhoseMountFingerprintIsUnstable(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	rig.on(completeIdentities(rig))
	ec := &ExecutionContext{State: map[string]any{}}
	ctx := withPromotionVerifiedFiles(context.Background(), ec)
	sum, _, err := rig.engine.promotionHash(ctx, rig.path)
	if err != nil {
		t.Fatal(err)
	}
	known := promotionKnownHelper(ctx, rig.path, sum)
	if known == nil || !known.Identity.Complete || known.Sample == "" {
		t.Fatalf("no helper proof after a helper hash: %+v", known)
	}
	if promotionKnownHelper(ctx, rig.path, "other") != nil {
		t.Fatal("a proof answered for another digest")
	}
	// The mount fingerprint going unusable must not lose the helper proof, and
	// must not pretend to be a fingerprint.
	v := promotionVerified(ctx)
	entry := v.files[rig.path]
	entry.fpValid = false
	v.files[rig.path] = entry
	if promotionKnownFingerprint(ctx, rig.path, sum) != nil {
		t.Fatal("a zero fingerprint was returned")
	}
	if promotionKnownHelper(ctx, rig.path, sum) == nil {
		t.Fatal("helper proof lost")
	}
}

// completeIdentities forwards to the real helper and marks every identity
// complete, as btrfs would.
func completeIdentities(rig *helperRig) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		rec := httptest.NewRecorder()
		rig.real.ServeHTTP(rec, r)
		out := bytes.ReplaceAll(rec.Body.Bytes(), []byte(`"complete":false`), []byte(`"complete":true`))
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(out)
		return true
	}
}

func TestVerifyByHelperProvesAnUnchangedFileWithoutReadingItInFull(t *testing.T) {
	rig := newHelperRig(t, "", nil)
	rig.on(completeIdentities(rig))
	ec := &ExecutionContext{State: map[string]any{}}
	ctx := withPromotionVerifiedFiles(context.Background(), ec)
	sum, _, err := rig.engine.promotionHash(ctx, rig.path)
	if err != nil {
		t.Fatal(err)
	}
	known := promotionKnownHelper(ctx, rig.path, sum)
	if known == nil {
		t.Fatal("no proof")
	}
	// A later execution: the cache is gone, the persisted proof is not.
	fresh := func() context.Context { return withPromotionVerifiedFiles(context.Background(), ec) }
	posts := rig.posts.Load()
	proof, err := rig.engine.promotionVerifyByHelper(fresh(), rig.path, sum, known, false)
	if err != nil || proof == nil {
		t.Fatalf("verify: %v %+v", err, proof)
	}
	if rig.posts.Load() != posts {
		t.Fatal("an unchanged file was read in full")
	}
	if rec := readRecord(t, ec, rig.path); rec["via"] != "nas_helper_identity" {
		t.Fatalf("record %+v", rec)
	}

	t.Run("ctime moved", func(t *testing.T) {
		if err := os.Chmod(rig.path, 0o640); err != nil {
			t.Fatal(err)
		}
		before := rig.posts.Load()
		proof, err := rig.engine.promotionVerifyByHelper(fresh(), rig.path, sum, known, false)
		if err != nil || proof == nil || rig.posts.Load() == before {
			t.Fatalf("a touched file was not read in full: %v %+v", err, proof)
		}
		// A rename is allowed to move it.
		before = rig.posts.Load()
		if _, err := rig.engine.promotionVerifyByHelper(fresh(), rig.path, sum, proof, true); err != nil || rig.posts.Load() != before {
			t.Fatalf("a renamed file's moved ctime read it in full: %v", err)
		}
	})
	t.Run("helper sees another ctime while the mount fingerprint is intact", func(t *testing.T) {
		// The same continuous execution that read the file holds a fingerprint
		// that still matches (here: the file is untouched); a proof recording
		// another ctime must still send the file to a real read, whose own
		// proof replaces the stale one.
		ctx := fresh()
		sum2, _, err := rig.engine.promotionHash(ctx, rig.path)
		if err != nil {
			t.Fatal(err)
		}
		cur := promotionKnownHelper(ctx, rig.path, sum2)
		if cur == nil {
			t.Fatal("no proof")
		}
		stale := *cur
		stale.Identity.CtimeNs++
		before := rig.posts.Load()
		proof, err := rig.engine.promotionVerifyByHelper(ctx, rig.path, sum2, &stale, false)
		if err != nil || proof == nil || rig.posts.Load() == before {
			t.Fatalf("a changed ctime was accepted through the mount's fingerprint: %v %+v", err, proof)
		}
		if proof.Identity.CtimeNs != cur.Identity.CtimeNs {
			t.Fatalf("the proof kept the stale ctime: %d vs %d", proof.Identity.CtimeNs, cur.Identity.CtimeNs)
		}
	})
	t.Run("earlier process", func(t *testing.T) {
		other := *known
		other.Process = "an-earlier-process"
		before := rig.posts.Load()
		if _, err := rig.engine.promotionVerifyByHelper(fresh(), rig.path, sum, &other, false); err != nil || rig.posts.Load() == before {
			t.Fatalf("a proof from another process was trusted: %v", err)
		}
	})
	t.Run("changed bytes", func(t *testing.T) {
		data, _ := os.ReadFile(rig.path)
		info, err := os.Stat(rig.path)
		if err != nil {
			t.Fatal(err)
		}
		data[0] ^= 0xff
		if err := os.WriteFile(rig.path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(rig.path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.engine.promotionVerifyByHelper(fresh(), rig.path, sum, known, false); err == nil || !strings.Contains(err.Error(), "integrity violation") {
			t.Fatalf("rewritten bytes with a restored mtime passed: %v", err)
		}
	})
}

func TestConfirmMovedByHelper(t *testing.T) {
	setup := func(t *testing.T) (*helperRig, *ExecutionContext, context.Context, *promotionState, string) {
		rig := newHelperRig(t, "", nil)
		rig.on(completeIdentities(rig))
		ec := &ExecutionContext{State: map[string]any{}}
		ctx := withPromotionVerifiedFiles(context.Background(), ec)
		sum, _, err := rig.engine.promotionHash(ctx, rig.path)
		if err != nil {
			t.Fatal(err)
		}
		backup := filepath.Join(rig.root, "recovery.mkv")
		p := &promotionState{OriginalPath: rig.path, OriginalSHA: sum, BackupPath: backup, RecoveryProof: promotionProofHelper, OriginalHelper: promotionKnownHelper(ctx, rig.path, sum)}
		return rig, ec, ctx, p, backup
	}
	t.Run("same inode", func(t *testing.T) {
		rig, _, ctx, p, backup := setup(t)
		if err := renamePromotionNoReplace(rig.path, backup); err != nil {
			t.Fatal(err)
		}
		before := rig.posts.Load()
		proof, err := rig.engine.promotionConfirmMovedByHelper(ctx, p, false)
		if err != nil || proof == nil || rig.posts.Load() != before {
			t.Fatalf("confirm: %v %+v hashes %d -> %d", err, proof, before, rig.posts.Load())
		}
	})
	t.Run("a rename that copied", func(t *testing.T) {
		rig, ec, ctx, p, backup := setup(t)
		data, _ := os.ReadFile(rig.path)
		if err := os.WriteFile(backup, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rig.path); err != nil {
			t.Fatal(err)
		}
		before := rig.posts.Load()
		proof, err := rig.engine.promotionConfirmMovedByHelper(ctx, p, false)
		if err != nil || proof == nil || rig.posts.Load() == before {
			t.Fatalf("a copy was not read in full: %v %+v", err, proof)
		}
		if rec := readRecord(t, ec, backup); rec["via"] != "nas_helper" {
			t.Fatalf("the full read is not what the record says: %+v", rec)
		}
	})
	t.Run("a copy of other bytes", func(t *testing.T) {
		rig, _, ctx, p, backup := setup(t)
		if err := os.WriteFile(backup, []byte("something else"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(rig.path); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.engine.promotionConfirmMovedByHelper(ctx, p, false); err == nil || !strings.Contains(err.Error(), "recovery retained") {
			t.Fatalf("other bytes were accepted: %v", err)
		}
	})
}

func TestHelperProbeKeepsTheInodeAndCleansUp(t *testing.T) {
	resetHelperProbes()
	rig := newHelperRig(t, "", nil)
	rig.on(completeIdentities(rig))
	dir := filepath.Join(rig.root, "recovery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	original, err := rig.engine.helperIdentity(context.Background(), rig.path)
	if err != nil {
		t.Fatal(err)
	}
	original.Complete = true
	if err := rig.engine.promotionProbeHelper(context.Background(), dir, original); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left files behind: %v", entries)
	}
	hits := rig.hits.Load()
	if err := rig.engine.promotionProbeHelper(context.Background(), dir, original); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if rig.hits.Load() != hits {
		t.Fatal("a cached verdict asked the helper again")
	}
	t.Run("another subvolume", func(t *testing.T) {
		other := original
		other.Subvol = "another"
		err := rig.engine.promotionProbeHelper(context.Background(), dir, other)
		var failure *promotionProbeFailure
		if !errors.As(err, &failure) || !failure.Definitive {
			t.Fatalf("probe: %v", err)
		}
	})
}

func TestHelperProbeNeedsACompleteIdentityAndCachesTheVerdict(t *testing.T) {
	resetHelperProbes()
	rig := newHelperRig(t, "", nil)
	rig.on(func(w http.ResponseWriter, r *http.Request) bool {
		rec := httptest.NewRecorder()
		rig.real.ServeHTTP(rec, r)
		out := bytes.ReplaceAll(rec.Body.Bytes(), []byte(`"complete":true`), []byte(`"complete":false`))
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(out)
		return true
	})
	dir := filepath.Join(rig.root, "recovery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := rig.engine.promotionProbeHelper(context.Background(), dir, nashelper.Identity{Complete: true})
	var failure *promotionProbeFailure
	if !errors.As(err, &failure) || !failure.Definitive || failure.Step != probeStepStat {
		t.Fatalf("probe on an incomplete identity: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left files behind: %v", entries)
	}
	promotionHelperProbesMu.Lock()
	n := len(promotionHelperProbes)
	promotionHelperProbesMu.Unlock()
	if n != 1 {
		t.Fatalf("a definitive verdict was not cached (%d entries)", n)
	}
	hits := rig.hits.Load()
	if err := rig.engine.promotionProbeHelper(context.Background(), dir, nashelper.Identity{Complete: true}); !errors.As(err, &failure) || failure.Step != probeStepStat {
		t.Fatalf("cached verdict: %v", err)
	}
	if rig.hits.Load() != hits {
		t.Fatal("a cached verdict asked the helper again")
	}
}

// A helper that is unmapped for the directory or recently unreachable is not
// asked to run the probe: nothing is created, nothing is cached, and the caller
// falls through to the mount probe.
func TestHelperProbeDoesNotTouchTheDirectoryWithoutAHelper(t *testing.T) {
	resetHelperProbes()
	rig := newHelperRig(t, "", nil)
	rig.on(completeIdentities(rig))
	check := func(t *testing.T, dir string) {
		t.Helper()
		hits := rig.hits.Load()
		err := rig.engine.promotionProbeHelper(context.Background(), dir, nashelper.Identity{Complete: true})
		var failure *promotionProbeFailure
		if !errors.As(err, &failure) || failure.Definitive {
			t.Fatalf("probe: %v", err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 || rig.hits.Load() != hits {
			t.Fatalf("probe touched the directory or the helper: %v, %d hits", entries, rig.hits.Load()-hits)
		}
		promotionHelperProbesMu.Lock()
		n := len(promotionHelperProbes)
		promotionHelperProbesMu.Unlock()
		if n != 0 {
			t.Fatal("a probe that did not run was cached")
		}
	}
	t.Run("unmapped", func(t *testing.T) { check(t, t.TempDir()) })
	t.Run("breaker open", func(t *testing.T) {
		dir := filepath.Join(rig.root, "recovery")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		rig.engine.helperBreaker.trip(time.Now())
		check(t, dir)
	})
}

func TestHelperProbeThatCouldNotRunIsNotCached(t *testing.T) {
	resetHelperProbes()
	rig := newHelperRig(t, "", nil)
	rig.on(func(w http.ResponseWriter, r *http.Request) bool {
		jsonReply(w, http.StatusInternalServerError, `{"error":{"code":"internal","message":"x"}}`)
		return true
	})
	dir := filepath.Join(rig.root, "recovery")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := rig.engine.promotionProbeHelper(context.Background(), dir, nashelper.Identity{Complete: true})
	var failure *promotionProbeFailure
	if !errors.As(err, &failure) || failure.Definitive {
		t.Fatalf("probe: %v", err)
	}
	promotionHelperProbesMu.Lock()
	n := len(promotionHelperProbes)
	promotionHelperProbesMu.Unlock()
	if n != 0 {
		t.Fatal("a probe that said nothing about the filesystem was cached")
	}
}

func TestStateWithoutAProofFieldKeepsTheMountProof(t *testing.T) {
	p := &promotionState{RecoveryMode: promotionRecoveryMoveAside}
	if p.helperProof() {
		t.Fatal("an older promotion became a helper proof")
	}
	p.RecoveryProof = promotionProofHelper
	p.OriginalHelper = (&helperProof{Identity: nashelper.Identity{FSID: "1", Ino: 1<<62 + 7, Size: 1 << 40, MtimeNs: 1<<62 + 1, CtimeNs: 1<<62 + 3, Complete: true}, Sample: "abc"}).persisted()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(generic)
	var back promotionState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.OriginalHelper == nil || back.OriginalHelper.Identity != p.OriginalHelper.Identity || back.OriginalHelper.Process != promotionProcess {
		t.Fatalf("proof lost in the round trip: %+v", back.OriginalHelper)
	}
}

// The helper proof belongs to the move aside: when the rename is refused and the
// promotion falls back to a recovery copy, it proves its files like any copy.
func TestHelperProofRefusedRenameFallsBackToACopyWithoutTheProof(t *testing.T) {
	resetHelperProbes()
	h := newPromotionHarness(t)
	attachProofHelper(t, h, true)
	failMountProbe(h)
	h.engine.promotionMoveAsideHook = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	}
	r := h.run()
	r = h.finish(h.resume(r.ID, "approve"))
	if r.Status != StatusCompleted {
		t.Fatalf("promotion: %s %s", r.Status, r.Error)
	}
	p := movedAsideState(t, h, r.ID)
	if p.RecoveryMode != promotionRecoveryCopy || p.RecoveryProof != "" || p.OriginalHelper != nil || p.BackupHelper != nil || p.MovedAside || p.RecoveryCopy == nil || !p.RecoveryCleanupCompleted {
		t.Fatalf("fallback evidence: %+v", p)
	}
}
