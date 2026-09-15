package transcodeworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// This file implements operational staging and finalization primitives. They
// are deliberately independent of transcode.Plan/transcode execution-spec
// digests and of WorkerConfig: staging policy is an operational concern that
// must never change the resolved plan or its digest.

// StagingPolicy controls whether a transcode source is copied into a local
// staging directory before the encoder reads it.
type StagingPolicy string

const (
	// StagingPolicyAuto stages a source only when it IS inside one of the
	// configured external roots (for example a network mount or removable
	// volume), i.e. when the source is not on local storage. This is the
	// operational default.
	StagingPolicyAuto StagingPolicy = "auto"
	// StagingPolicyNever never stages: the encoder reads the source in place.
	StagingPolicyNever StagingPolicy = "never"
	// StagingPolicyAlways always stages, even for locally-attached sources.
	StagingPolicyAlways StagingPolicy = "always"
)

// ParseStagingPolicy parses the exact operational values "auto", "never" and
// "always". Surrounding whitespace is ignored, but the value is otherwise
// matched exactly (case sensitive); anything else is invalid.
func ParseStagingPolicy(raw string) (StagingPolicy, error) {
	switch StagingPolicy(strings.TrimSpace(raw)) {
	case StagingPolicyAuto:
		return StagingPolicyAuto, nil
	case StagingPolicyNever:
		return StagingPolicyNever, nil
	case StagingPolicyAlways:
		return StagingPolicyAlways, nil
	default:
		return "", fmt.Errorf("%w: %q (want auto|never|always)", ErrInvalidStagingPolicy, raw)
	}
}

// DefaultStagingPolicy returns the operational default policy ("auto").
func DefaultStagingPolicy() StagingPolicy { return StagingPolicyAuto }

// Sentinel errors. They let callers distinguish a missing/invalid input from an
// operational storage failure or a destination-exists conflict. Generic I/O
// errors are never reported as source corruption.
var (
	// ErrSourceMissing indicates the requested input file does not exist.
	ErrSourceMissing = errors.New("transcode source file missing")
	// ErrSourceInvalid indicates the input exists but is not a regular file
	// (for example a directory, device or symlink).
	ErrSourceInvalid = errors.New("transcode source file is not a regular file")
	// ErrDestinationExists indicates the destination already exists; staging
	// and finalization fail closed rather than overwrite it.
	ErrDestinationExists = errors.New("transcode destination already exists")
	// ErrDestinationInvalid indicates a missing or unusable destination path.
	ErrDestinationInvalid = errors.New("invalid transcode destination path")
	// ErrInvalidJobID indicates a job id that is empty or could escape the
	// per-job work directory.
	ErrInvalidJobID = errors.New("invalid transcode job id")
	// ErrInvalidStagingPolicy indicates an unknown staging policy value.
	ErrInvalidStagingPolicy = errors.New("invalid staging policy")
	// ErrStorageIO indicates an operational storage failure (open/create/
	// write/rename/sync) that is not by itself evidence of source corruption.
	ErrStorageIO = errors.New("transcode storage i/o failure")
	// ErrSizeMismatch indicates the copied byte count did not match the source.
	ErrSizeMismatch = errors.New("copied byte count mismatch")
)

// IsSourceMissing reports whether err is (or wraps) ErrSourceMissing.
func IsSourceMissing(err error) bool { return errors.Is(err, ErrSourceMissing) }

// IsSourceInvalid reports whether err is (or wraps) ErrSourceInvalid.
func IsSourceInvalid(err error) bool { return errors.Is(err, ErrSourceInvalid) }

// IsDestinationExists reports whether err is (or wraps) ErrDestinationExists.
func IsDestinationExists(err error) bool { return errors.Is(err, ErrDestinationExists) }

// IsStorageIO reports whether err is (or wraps) ErrStorageIO.
func IsStorageIO(err error) bool { return errors.Is(err, ErrStorageIO) }

// IsInvalidJobID reports whether err is (or wraps) ErrInvalidJobID.
func IsInvalidJobID(err error) bool { return errors.Is(err, ErrInvalidJobID) }

// maxSafeExtensionLen bounds how much of an untrusted extension is preserved.
const maxSafeExtensionLen = 16

// IsExternalPath reports whether path is one of the externalRoots or a
// descendant of one of them. Boundary-safe: a root "/mnt/media" matches
// "/mnt/media" and "/mnt/media/x" but never "/mnt/media2".
func IsExternalPath(path string, externalRoots []string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	clean := filepath.Clean(path)
	for _, root := range externalRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		cleanRoot := filepath.Clean(root)
		if clean == cleanRoot {
			return true
		}
		rel, err := filepath.Rel(cleanRoot, clean)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) {
			continue
		}
		if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// ShouldStageInput applies an operational staging policy to a source path.
//
//	always -> true
//	never  -> false
//	auto   -> IsExternalPath(sourcePath, externalRoots) (stage when the source
//	          IS inside one of the caller-supplied external roots)
//
// Unknown policy values fail closed: they return (false, ErrInvalidStagingPolicy)
// rather than silently behaving like auto.
func ShouldStageInput(policy StagingPolicy, sourcePath string, externalRoots []string) (bool, error) {
	switch policy {
	case StagingPolicyAlways:
		return true, nil
	case StagingPolicyNever:
		return false, nil
	case StagingPolicyAuto:
		return IsExternalPath(sourcePath, externalRoots), nil
	default:
		return false, fmt.Errorf("%w: %q", ErrInvalidStagingPolicy, policy)
	}
}

// validateJobID rejects job ids that are empty, contain path separators or NUL,
// are absolute, or are the special "." / ".." entries. Surrounding whitespace
// is rejected rather than silently trimmed.
func validateJobID(jobID string) error {
	if jobID == "" {
		return fmt.Errorf("%w: empty", ErrInvalidJobID)
	}
	if strings.TrimSpace(jobID) != jobID {
		return fmt.Errorf("%w: %q has surrounding whitespace", ErrInvalidJobID, jobID)
	}
	if jobID == "." || jobID == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidJobID, jobID)
	}
	if strings.ContainsAny(jobID, `/\`) {
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidJobID, jobID)
	}
	if strings.ContainsRune(jobID, '\x00') {
		return fmt.Errorf("%w: %q contains NUL", ErrInvalidJobID, jobID)
	}
	if filepath.IsAbs(jobID) {
		return fmt.Errorf("%w: %q is absolute", ErrInvalidJobID, jobID)
	}
	return nil
}

// JobWorkDir returns the deterministic per-job directory beneath localWorkDir.
// The job id is validated and the result is re-checked so it can never escape
// localWorkDir.
func JobWorkDir(localWorkDir, jobID string) (string, error) {
	root := strings.TrimSpace(localWorkDir)
	if root == "" {
		return "", fmt.Errorf("%w: local work dir is required", ErrStorageIO)
	}
	if err := validateJobID(jobID); err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	dir := filepath.Join(root, jobID)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.ContainsRune(rel, filepath.Separator) {
		return "", fmt.Errorf("%w: %q escapes local work dir", ErrInvalidJobID, jobID)
	}
	return dir, nil
}

// safeFileExtension returns the extension of an untrusted path only when it is
// short and alphanumeric; otherwise it returns "". The untrusted basename is
// never used to build the path.
func safeFileExtension(p string) string {
	ext := filepath.Ext(p)
	if ext == "" || len(ext) > maxSafeExtensionLen {
		return ""
	}
	for _, r := range ext[1:] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return ""
		}
	}
	return ext
}

// StagedInputPath returns the deterministic staged-input path for a job beneath
// localWorkDir. It is always "<localWorkDir>/<jobID>/input<ext>" where ext is a
// sanitized extension preserved from sourcePath.
func StagedInputPath(localWorkDir, jobID, sourcePath string) (string, error) {
	dir, err := JobWorkDir(localWorkDir, jobID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "input"+safeFileExtension(sourcePath)), nil
}

// LocalCandidatePath returns the deterministic local candidate path for a job
// beneath localWorkDir. It is always
// "<localWorkDir>/<jobID>/candidate<ext>" with a sanitized extension.
func LocalCandidatePath(localWorkDir, jobID, candidatePath string) (string, error) {
	dir, err := JobWorkDir(localWorkDir, jobID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "candidate"+safeFileExtension(candidatePath)), nil
}

// regularFileInfo Lstats path and requires it to be a regular file (symlinks,
// directories and devices are rejected). A missing file is ErrSourceMissing;
// other stat failures are ErrStorageIO.
func regularFileInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrSourceMissing, path)
		}
		return nil, fmt.Errorf("%w: statting %s: %v", ErrStorageIO, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s (mode %s)", ErrSourceInvalid, path, info.Mode())
	}
	return info, nil
}

// pathExists reports whether path exists (without following symlinks on the
// final component). Other Lstat failures are ErrStorageIO.
func pathExists(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("%w: checking %s: %v", ErrStorageIO, path, err)
	}
	return true, nil
}

// copyWithContext copies from source into dst, checking ctx between chunks so a
// cancelled context aborts promptly. Generic read failures are reported as
// ErrStorageIO, never as source corruption.
func copyWithContext(ctx context.Context, source string, dst *os.File) (int64, error) {
	src, err := os.Open(source)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("%w: %s", ErrSourceMissing, source)
		}
		return 0, fmt.Errorf("%w: opening source %s: %v", ErrStorageIO, source, err)
	}
	defer src.Close()

	buf := make([]byte, 128*1024)
	var written int64
	for {
		if cerr := ctx.Err(); cerr != nil {
			return written, cerr
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, fmt.Errorf("%w: writing %s: %v", ErrStorageIO, dst.Name(), werr)
			}
			written += int64(n)
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, fmt.Errorf("%w: reading source %s: %v", ErrStorageIO, source, rerr)
		}
	}
}

// copyRegularFileTo copies source into an already-open dst, applies perm
// (defaulting to 0644 when zero), Syncs and Closes dst, then verifies that the
// on-disk temp size matches the expected source size.
func copyRegularFileTo(ctx context.Context, source string, dst *os.File, wantSize int64, perm os.FileMode) error {
	if perm == 0 {
		perm = 0o644
	}
	if err := dst.Chmod(perm); err != nil {
		return fmt.Errorf("%w: setting mode on %s: %v", ErrStorageIO, dst.Name(), err)
	}
	written, err := copyWithContext(ctx, source, dst)
	if err != nil {
		return err
	}
	if written != wantSize {
		return fmt.Errorf("%w: read %d of %d bytes from %s", ErrSizeMismatch, written, wantSize, source)
	}
	if err := dst.Sync(); err != nil {
		return fmt.Errorf("%w: syncing %s: %v", ErrStorageIO, dst.Name(), err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("%w: closing %s: %v", ErrStorageIO, dst.Name(), err)
	}
	fi, err := os.Stat(dst.Name())
	if err != nil {
		return fmt.Errorf("%w: statting %s: %v", ErrStorageIO, dst.Name(), err)
	}
	if fi.Size() != wantSize {
		return fmt.Errorf("%w: %s size %d != source size %d", ErrSizeMismatch, dst.Name(), fi.Size(), wantSize)
	}
	return nil
}

// syncDir best-effort flushes a directory entry after a commit so the new name
// survives a crash. Failures are ignored: not every platform/filesystem allows
// syncing a directory handle.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// errNoReplaceUnsupported signals that the platform's atomic no-replace rename
// is unavailable, either because the platform lacks such a syscall or because
// the target filesystem (for example some SMB/NAS mounts) rejected it.
var errNoReplaceUnsupported = errors.New("atomic no-replace rename unsupported")

// commitNoReplace atomically publishes src at dst without ever replacing an
// existing dst.
//
// On Darwin it uses renamex_np(RENAME_EXCL), which performs the publication as
// a single rename and fails with EEXIST when dst exists; this works through
// VFS (and therefore on SMB/NAS mounts) without relying on hard links. If the
// filesystem rejects the exclusive rename (ENOTSUP/EINVAL/ENOSYS), it falls
// back to a hard-link publication, which is also atomic and no-clobber but is
// unsupported by some SMB/FAT shares. Other platforms use the hard-link
// fallback directly.
//
// src must be a fully written, synced, size-verified file on the same
// filesystem as dst so the destination name appears with complete contents in
// a single operation. Unlike os.Rename, this can never clobber a concurrently
// created dst: an existing destination is reported as ErrDestinationExists and
// its bytes are preserved. If neither primitive is supported the commit fails
// closed with ErrStorageIO, and it never falls back to a clobbering rename.
func commitNoReplace(src, dst string) error {
	err := renameNoReplace(src, dst)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
	case errors.Is(err, errNoReplaceUnsupported):
		if lerr := linkNoReplace(src, dst); lerr != nil {
			if errors.Is(lerr, fs.ErrExist) {
				return fmt.Errorf("%w: %s", ErrDestinationExists, dst)
			}
			return fmt.Errorf("%w: publishing %s to %s: %v", ErrStorageIO, src, dst, lerr)
		}
		return nil
	default:
		return fmt.Errorf("%w: publishing %s to %s: %v", ErrStorageIO, src, dst, err)
	}
}

// linkNoReplace creates dst as a hard link to src (link(2) on Unix,
// CreateHardLink on Windows), then drops src. Creating the link fails with
// EEXIST when dst already exists, so publication is atomic and no-clobber.
// Filesystems without hard-link support (some SMB/FAT shares) return an error
// that callers surface as ErrStorageIO.
func linkNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		if os.IsExist(err) {
			return fs.ErrExist
		}
		return err
	}
	// dst is already published with complete contents; dropping src is cleanup.
	// A failure here leaves a redundant name but never corrupts dst.
	_ = os.Remove(src)
	return nil
}

// StageInputAtomic atomically materializes source at stagedFinal.
//
// It requires a regular source, requires stagedFinal not to pre-exist, copies
// through a unique temp file in stagedFinal's directory, preserves the source
// permission bits, verifies the copied byte count, then commits the temp into
// place with a no-clobber link. On any error or cancellation only its own temp
// file is removed and stagedFinal is left absent. No content hashes are
// computed.
func StageInputAtomic(ctx context.Context, source, stagedFinal string) error {
	return stageInputAtomic(ctx, source, stagedFinal, nil)
}

// stageInputAtomic is StageInputAtomic with an optional test-only hook invoked
// after the temp file is written/synced/verified but before the commit. It lets
// tests observe that stagedFinal is still absent at commit time and inject a
// race.
func stageInputAtomic(ctx context.Context, source, stagedFinal string, hook func(tempPath string) error) (err error) {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("%w: empty source path", ErrSourceInvalid)
	}
	if strings.TrimSpace(stagedFinal) == "" {
		return fmt.Errorf("%w: empty staged-final path", ErrDestinationInvalid)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	info, err := regularFileInfo(source)
	if err != nil {
		return err
	}

	if exists, err := pathExists(stagedFinal); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %s", ErrDestinationExists, stagedFinal)
	}

	dir := filepath.Dir(stagedFinal)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: creating staging directory %s: %v", ErrStorageIO, dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(stagedFinal)+".stage-*")
	if err != nil {
		return fmt.Errorf("%w: creating staging temp in %s: %v", ErrStorageIO, dir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err := copyRegularFileTo(ctx, source, tmp, info.Size(), info.Mode().Perm()); err != nil {
		return err
	}
	if hook != nil {
		if herr := hook(tmpPath); herr != nil {
			return fmt.Errorf("%w: staging hook: %w", ErrStorageIO, herr)
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err := commitNoReplace(tmpPath, stagedFinal); err != nil {
		return err
	}
	committed = true
	syncDir(dir)
	return nil
}

// FinalizeOutputAtomic atomically publishes localCandidate at destination.
//
// The destination must not pre-exist (fail closed). The copy always goes
// through the exact partial path "<destination>.partial.<jobID>"; a stale
// partial for this job is replaced, but unrelated partials are never touched.
// The no-clobber commit happens only after the byte count is verified, so
// destination is absent until commit, and a concurrently created destination
// is never overwritten. On any error or cancellation only the exact own
// partial is removed.
func FinalizeOutputAtomic(ctx context.Context, localCandidate, destination, jobID string) error {
	return finalizeOutputAtomic(ctx, localCandidate, destination, jobID, nil)
}

// finalizeOutputAtomic is FinalizeOutputAtomic with an optional test-only hook
// invoked after the partial is written/synced/verified but before the commit.
// It lets tests observe that destination is still absent and inject a race.
func finalizeOutputAtomic(ctx context.Context, localCandidate, destination, jobID string, hook func(partialPath string) error) (err error) {
	if strings.TrimSpace(localCandidate) == "" {
		return fmt.Errorf("%w: empty local candidate path", ErrSourceInvalid)
	}
	if strings.TrimSpace(destination) == "" {
		return fmt.Errorf("%w: empty destination path", ErrDestinationInvalid)
	}
	if err := validateJobID(jobID); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	info, err := regularFileInfo(localCandidate)
	if err != nil {
		return err
	}

	if exists, err := pathExists(destination); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
	}

	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w: creating destination directory %s: %v", ErrStorageIO, dir, err)
	}

	partial := destination + ".partial." + jobID

	// Replace only this job's exact partial. Never glob or delete unrelated
	// partial files.
	if exists, err := pathExists(partial); err != nil {
		return err
	} else if exists {
		if rerr := os.Remove(partial); rerr != nil {
			return fmt.Errorf("%w: removing stale partial %s: %v", ErrStorageIO, partial, rerr)
		}
	}

	dst, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w: creating partial %s: %v", ErrStorageIO, partial, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = dst.Close()
			_ = os.Remove(partial)
		}
	}()

	if err := copyRegularFileTo(ctx, localCandidate, dst, info.Size(), info.Mode().Perm()); err != nil {
		return err
	}
	if hook != nil {
		if herr := hook(partial); herr != nil {
			return fmt.Errorf("%w: finalize hook: %w", ErrStorageIO, herr)
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}

	// No-clobber commit: fails with ErrDestinationExists if a destination
	// appeared after the pre-check, preserving its bytes.
	if err := commitNoReplace(partial, destination); err != nil {
		return err
	}
	committed = true
	syncDir(dir)
	return nil
}
