package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"
)

// promotionFingerprint is a strict stat identity of a file: size, mtime and
// ctime to the nanosecond, plus device and inode. Any write changes mtime or
// ctime, and setting mtime back changes ctime again. Durable state is decoded
// through map[string]any, where a JSON number becomes a float64 and loses
// nanoseconds and large inode numbers, so every field is persisted as a string.
type promotionFingerprint struct {
	Size    int64  `json:"size,string"`
	MtimeNs int64  `json:"mtime_ns,string"`
	CtimeNs int64  `json:"ctime_ns,string"`
	Dev     uint64 `json:"dev,string"`
	Ino     uint64 `json:"ino,string"`
	// Process is set on a persisted fingerprint to the process that took
	// it. A fingerprint from an earlier process never proves a file: after
	// a restart every file is read in full again.
	Process string `json:"process,omitempty"`
}

// promotionProcess identifies this process's lifetime.
var promotionProcess = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// persisted returns fp stamped with this process, for durable state.
func (fp promotionFingerprint) persisted() *promotionFingerprint {
	fp.Process = promotionProcess
	return &fp
}

// fromThisProcess returns a persisted fingerprint without its stamp when this
// process took it, so it can be compared with a fresh one.
func (fp *promotionFingerprint) fromThisProcess() (promotionFingerprint, bool) {
	if fp == nil || fp.Process != promotionProcess {
		return promotionFingerprint{}, false
	}
	k := *fp
	k.Process = ""
	return k, true
}

// Verification kinds recorded in ec.State["integrity_checks"].
const (
	promotionCheckSHA256    = "sha256"      // full read
	promotionCheckStat      = "fingerprint" // unchanged since this execution's full read
	promotionCheckRenamed   = "sha256_renamed"
	promotionCheckIdentity  = "identity" // unchanged since a full read before a wait
	promotionIntegrityState = "integrity_checks"
)

type promotionVerifiedKey struct{}

// promotionVerifiedFiles remembers the files that one continuous execution
// fully hashed. Engine.execute creates one per call, and every wait (decision,
// external, reconcile), resume or restart starts a new call. Nothing in it is
// ever read back from durable state, so a full hash is always required again
// after a wait.
type promotionVerifiedFiles struct {
	ec    *ExecutionContext
	files map[string]promotionVerifiedFile
}

type promotionVerifiedFile struct {
	sha string
	fp  promotionFingerprint
}

func withPromotionVerifiedFiles(ctx context.Context, ec *ExecutionContext) context.Context {
	return context.WithValue(ctx, promotionVerifiedKey{}, &promotionVerifiedFiles{ec: ec, files: map[string]promotionVerifiedFile{}})
}

// withoutPromotionVerifiedFiles makes every hash under ctx a full read. The
// checks that remove recovery data always read the final file in full.
func withoutPromotionVerifiedFiles(ctx context.Context) context.Context {
	return context.WithValue(ctx, promotionVerifiedKey{}, (*promotionVerifiedFiles)(nil))
}

func promotionForget(ctx context.Context, path string) {
	if v := promotionVerified(ctx); v != nil {
		delete(v.files, path)
	}
}

func promotionVerified(ctx context.Context) *promotionVerifiedFiles {
	v, _ := ctx.Value(promotionVerifiedKey{}).(*promotionVerifiedFiles)
	return v
}

// record persists, with the execution's next checkpoint, how path was last
// verified. It is an audit trail only: nothing trusts it after a wait.
func (v *promotionVerifiedFiles) record(path, kind, sha string, fp promotionFingerprint) {
	checks, _ := v.ec.State[promotionIntegrityState].(map[string]any)
	if checks == nil {
		checks = map[string]any{}
		v.ec.State[promotionIntegrityState] = checks
	}
	checks[path] = map[string]any{"kind": kind, "sha256": sha, "fingerprint": fp, "at": time.Now().UTC().Format(time.RFC3339Nano)}
}

// promotionRememberHash keeps a full hash's fingerprint when the descriptor
// showed the same identity before and after the read. A platform or mount
// without inode numbers or ctime yields nothing, so later checks hash again.
func promotionRememberHash(ctx context.Context, path, sha string, opened, finished os.FileInfo) {
	v := promotionVerified(ctx)
	if v == nil {
		return
	}
	before, ok := promotionFileFingerprint(opened)
	after, ok2 := promotionFileFingerprint(finished)
	if !ok || !ok2 || before != after {
		delete(v.files, path)
		return
	}
	v.files[path] = promotionVerifiedFile{sha: sha, fp: after}
	v.record(path, promotionCheckSHA256, sha, after)
}

// promotionStatIdentity opens path like promotionHash does and returns the
// descriptor's attributes. Opening revalidates network-mount attribute caches
// that a bare Lstat may serve stale.
func (e *Engine) promotionStatIdentity(path string) (os.FileInfo, error) {
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return nil, err
	}
	before, err := e.promotionLstat(resolved)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, opened) {
		return nil, os.ErrInvalid
	}
	return opened, nil
}

// promotionStillVerified reports whether path is provably unchanged since
// this execution fully hashed it to expected. Any doubt answers false, and
// the caller then reads the whole file.
func (e *Engine) promotionStillVerified(ctx context.Context, path, expected string) bool {
	v := promotionVerified(ctx)
	if v == nil {
		return false
	}
	known, ok := v.files[path]
	if !ok || known.sha != expected {
		return false
	}
	info, err := e.promotionStatIdentity(path)
	if err != nil {
		return false
	}
	fp, ok := promotionFileFingerprint(info)
	if !ok || fp != known.fp {
		delete(v.files, path)
		return false
	}
	v.record(path, promotionCheckStat, expected, fp)
	return true
}

// promotionCarryRenamed moves a full hash across this step's own rename of
// from to to. The rename changes ctime, so the file must keep its device,
// inode, size and mtime exactly; otherwise to is hashed again when checked.
func (e *Engine) promotionCarryRenamed(ctx context.Context, from, to string) {
	v := promotionVerified(ctx)
	if v == nil {
		return
	}
	known, ok := v.files[from]
	delete(v.files, from)
	if !ok {
		return
	}
	info, err := e.promotionStatIdentity(to)
	if err != nil {
		return
	}
	fp, ok := promotionFileFingerprint(info)
	if !ok || fp.Dev != known.fp.Dev || fp.Ino != known.fp.Ino || fp.Size != known.fp.Size || fp.MtimeNs != known.fp.MtimeNs {
		return
	}
	v.files[to] = promotionVerifiedFile{sha: known.sha, fp: fp}
	v.record(to, promotionCheckRenamed, known.sha, fp)
}
