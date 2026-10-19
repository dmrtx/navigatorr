package action

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// promotionMountProbes caches, per process and recovery directory, whether
// the mount there keeps the identity move-aside relies on.
var promotionMountProbes sync.Map

// Probe steps. A failed probe names the step so a copy that was not expected
// can be explained afterwards; see promotionProbeFailure.
const (
	probeStepCreate       = "create_file"
	probeStepStat         = "stat"
	probeStepUnstable     = "identity_unstable_while_unchanged"
	probeStepRename       = "rename_no_replace"
	probeStepRenameInode  = "rename_changed_identity"
	probeStepRewrite      = "rewrite_in_place"
	probeStepRewriteInode = "rewrite_changed_identity"
	probeStepCtime        = "rewrite_kept_change_time"
)

// promotionProbeFailure is why the mount probe failed, without any path:
// Step is one of the probeStep constants, Fs the filesystem type of the
// recovery directory, Detail what was observed, and Err the underlying error.
// Definitive means the mount itself was found not to keep file identity;
// anything else (an I/O error, a permission error, a full disk) says nothing
// about the mount and is not cached.
type promotionProbeFailure struct {
	Step       string
	Fs         string
	Detail     string
	Err        error
	Definitive bool
}

func (f *promotionProbeFailure) Error() string {
	parts := []string{f.Step}
	if f.Fs != "" {
		parts = append(parts, "fs="+f.Fs)
	}
	if f.Detail != "" {
		parts = append(parts, f.Detail)
	}
	if f.Err != nil {
		parts = append(parts, "error="+promotionErrorCause(f.Err))
	}
	return strings.Join(parts, " ")
}

func (f *promotionProbeFailure) Unwrap() error { return f.Err }

// promotionErrorCause is err without the paths an operating system error
// carries, which must not reach the stored state.
func promotionErrorCause(err error) string {
	for {
		var pathErr *os.PathError
		var linkErr *os.LinkError
		switch {
		case errors.As(err, &pathErr):
			err = pathErr.Err
		case errors.As(err, &linkErr):
			err = linkErr.Err
		default:
			return err.Error()
		}
	}
}

// promotionProbeDetail is the stored reason for a failed probe: bounded and
// free of paths.
func promotionProbeDetail(err error) string {
	detail := err.Error()
	var failure *promotionProbeFailure
	if !errors.As(err, &failure) {
		detail = promotionErrorCause(err)
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	return detail
}

// promotionProbeMount checks once per process, in the recovery directory,
// that a file there keeps what move-aside trusts across waits: a real inode
// and change time, stable while nothing changes; the same inode after a
// no-replace rename; and a change time that moves when the bytes are
// rewritten in place with size and mtime restored. Mounts that synthesize
// inode numbers or lack change times fail it, and then the promotion copies.
// Only a verdict about the mount is cached: an error that says nothing about
// it is asked again by the next promotion.
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
	var failure *promotionProbeFailure
	if err == nil || (errors.As(err, &failure) && failure.Definitive) {
		promotionMountProbes.Store(key, err)
	}
	return err
}

func probePromotionMount(dir string) error {
	fs := promotionFilesystemName(dir)
	fail := func(step, detail string, err error, definitive bool) error {
		return &promotionProbeFailure{Step: step, Fs: fs, Detail: detail, Err: err, Definitive: definitive}
	}
	f, err := os.CreateTemp(dir, ".mount-probe-")
	if err != nil {
		return fail(probeStepCreate, "", err, false)
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
		return fail(probeStepCreate, "", err, false)
	}
	stat := func(path string) (promotionFingerprint, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return promotionFingerprint{}, fail(probeStepStat, "", err, false)
		}
		fp, ok := promotionFileFingerprint(info)
		if !ok {
			return promotionFingerprint{}, fail(probeStepStat, "no inode number or change time", nil, true)
		}
		return fp, nil
	}
	created, err := stat(from)
	if err != nil {
		return err
	}
	if again, err := stat(from); err != nil {
		return err
	} else if again != created {
		return fail(probeStepUnstable, "changed: "+promotionFingerprintDiff(created, again), nil, true)
	}
	if err := renamePromotionNoReplace(from, to); err != nil {
		// A refused rename is a verdict on the mount; anything else is not.
		return fail(probeStepRename, "", err, promotionCopyUnsupported(err))
	}
	moved, err := stat(to)
	if err != nil {
		return err
	}
	if !moved.sameInode(created) {
		return fail(probeStepRenameInode, "changed: "+promotionFingerprintDiff(created, moved), nil, true)
	}
	mtime := time.Unix(0, moved.MtimeNs)
	// Coarse clocks may need a moment before a change time can differ.
	const attempts = 150
	var rewritten promotionFingerprint
	for i := 0; i < attempts; i++ {
		content[len(content)-1] = byte('1' + i%9)
		if err := os.WriteFile(to, content, 0600); err != nil {
			return fail(probeStepRewrite, "", err, false)
		}
		if err := os.Chtimes(to, mtime, mtime); err != nil {
			return fail(probeStepRewrite, "", err, false)
		}
		rewritten, err = stat(to)
		if err != nil {
			return err
		}
		if rewritten.Ino != moved.Ino || rewritten.Dev != moved.Dev {
			return fail(probeStepRewriteInode, "changed: "+promotionFingerprintDiff(moved, rewritten), nil, true)
		}
		if rewritten.CtimeNs != moved.CtimeNs {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	detail := fmt.Sprintf("after %d rewrites; mtime restored: %t; change time equals mtime: %t", attempts, rewritten.MtimeNs == moved.MtimeNs, rewritten.CtimeNs == rewritten.MtimeNs)
	return fail(probeStepCtime, detail, nil, true)
}

// promotionFingerprintDiff names the fields that differ, never their values.
func promotionFingerprintDiff(a, b promotionFingerprint) string {
	var fields []string
	for _, c := range []struct {
		name string
		diff bool
	}{
		{"dev", a.Dev != b.Dev}, {"ino", a.Ino != b.Ino}, {"size", a.Size != b.Size},
		{"mtime", a.MtimeNs != b.MtimeNs}, {"ctime", a.CtimeNs != b.CtimeNs},
	} {
		if c.diff {
			fields = append(fields, c.name)
		}
	}
	if len(fields) == 0 {
		return "nothing"
	}
	return strings.Join(fields, ",")
}
