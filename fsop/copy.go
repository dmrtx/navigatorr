package fsop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Copy outcomes.
const (
	// CopyCopied is a new destination, written and verified.
	CopyCopied = "copied"
	// CopyAlreadyPresent is a destination that already held the same bytes:
	// nothing was written.
	CopyAlreadyPresent = "already_present"
	// CopyReplaced is a different destination replaced, as confirmed.
	CopyReplaced = "replaced"
)

// CopyResult says what Copy did, with the hashes that prove it.
type CopyResult struct {
	Status      string `json:"status"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Bytes       int64  `json:"bytes"`
	// SourceSHA256 is the source as it was read for the copy;
	// DestinationSHA256 is the destination read back from disk afterwards.
	// They are equal on every success.
	SourceSHA256      string `json:"source_sha256"`
	DestinationSHA256 string `json:"destination_sha256"`
	// ReplacedSHA256 is the content a replace overwrote.
	ReplacedSHA256 string   `json:"replaced_sha256,omitempty"`
	CreatedDirs    []string `json:"created_dirs,omitempty"`
}

// DestinationDiffersError is a destination that exists with other content.
// Nothing was written; both hashes are reported so the caller can confirm a
// replace of exactly that content.
type DestinationDiffersError struct {
	Destination       string
	Bytes             int64
	SourceSHA256      string
	DestinationSHA256 string
}

func (e *DestinationDiffersError) Error() string {
	return fmt.Sprintf("destination %q already exists with different content (sha256 %s, source %s); nothing was written", e.Destination, e.DestinationSHA256, e.SourceSHA256)
}

// spaceFn reports a filesystem's space; tests replace it.
var spaceFn = SpaceOf

// afterCompareHook runs after an existing destination and the source are
// hashed; tests use it to change a file in that window.
var afterCompareHook func()

// Copy copies the file source (inside the read roots) to destination (inside
// the write roots) and proves it: the source is hashed while it is copied to
// a hidden temporary name next to the destination, the copy is flushed and
// read back from disk, and it is put in place only when size and SHA-256
// match and the source did not change meanwhile. Missing folders above the
// destination are created as Move does.
//
// It is idempotent: a destination that already holds the same bytes is left
// alone and reported as already present (the source is re-checked after both
// hashes). A destination with other content is never replaced unless replaceSHA256 names exactly that content (the
// destination_sha256 a DestinationDiffersError reported); the replace is an
// atomic rename, refused unless the destination is still the same file and
// hashes to the confirmed content right before it. Another writer that
// changes the destination in the instant between that check and the rename
// is not detected: POSIX has no compare-and-swap rename.
// Only regular files are copied; the source is never modified.
func (r *Resolver) Copy(ctx context.Context, source, destination, replaceSHA256 string) (CopyResult, error) {
	res := CopyResult{Source: source, Destination: destination}
	src, err := r.ResolveRead(source)
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	dst, err := r.ResolveWrite(destination)
	if err != nil {
		return res, fmt.Errorf("destination: %w", err)
	}
	res.Source, res.Destination = src, dst
	si, err := os.Stat(src)
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	if !si.Mode().IsRegular() {
		return res, fmt.Errorf("source %q is not a regular file; only files are copied", source)
	}
	if src == dst {
		return res, errors.New("source and destination are the same file")
	}
	if r.isRoot(dst) {
		return res, fmt.Errorf("destination %q is an allowed root", destination)
	}
	if err := r.rootsMounted(dst); err != nil {
		return res, err
	}

	var existing os.FileInfo
	if di, err := os.Lstat(dst); err == nil {
		if !di.Mode().IsRegular() {
			return res, fmt.Errorf("destination %q exists and is not a regular file", destination)
		}
		if os.SameFile(si, di) {
			return res, fmt.Errorf("destination %q is the source itself (a hard link), not a copy", destination)
		}
		srcSum, srcN, err := hashPath(ctx, src)
		if err != nil {
			return res, fmt.Errorf("hashing the source: %w", err)
		}
		dstSum, dstN, err := hashPath(ctx, dst)
		if err != nil {
			return res, fmt.Errorf("hashing the destination: %w", err)
		}
		if afterCompareHook != nil {
			afterCompareHook()
		}
		// The source must still be the file that was hashed: a retry must
		// never report a stale backup as present.
		if now, err := os.Stat(src); err != nil || !sameFileState(si, now) {
			return res, fmt.Errorf("source %q changed while it was compared with the destination; nothing was written", source)
		}
		if srcN == dstN && srcSum == dstSum {
			res.Status, res.Bytes, res.SourceSHA256, res.DestinationSHA256 = CopyAlreadyPresent, srcN, srcSum, dstSum
			return res, nil
		}
		if replaceSHA256 == "" {
			return res, &DestinationDiffersError{Destination: dst, Bytes: dstN, SourceSHA256: srcSum, DestinationSHA256: dstSum}
		}
		if replaceSHA256 != dstSum {
			return res, fmt.Errorf("destination %q holds sha256 %s, not the %s confirmed for replacement; nothing was written", destination, dstSum, replaceSHA256)
		}
		existing = di
		res.ReplacedSHA256 = dstSum
	} else if !os.IsNotExist(err) {
		return res, fmt.Errorf("destination: %w", err)
	} else if replaceSHA256 != "" {
		return res, fmt.Errorf("destination %q does not exist, so there is nothing to replace; nothing was written", destination)
	}

	parent := filepath.Dir(dst)
	created, err := mkdirAllReport(parent)
	if err == nil {
		var real string
		if real, err = r.ResolveWrite(parent); err == nil && real != parent {
			err = fmt.Errorf("%q now resolves to %q", parent, real)
		}
	}
	if err != nil {
		removeEmptyDirs(created)
		return res, fmt.Errorf("creating destination folder: %w", err)
	}
	fail := func(err error) (CopyResult, error) {
		removeEmptyDirs(created)
		return res, err
	}
	if sp, err := spaceFn(parent); err == nil {
		// The copy needs its own size; an old destination is only freed once
		// the new one is in place.
		if need := uint64(si.Size()); sp.Avail < need {
			return fail(fmt.Errorf("not enough space for the copy: %d bytes free, %d needed; nothing was written", sp.Avail, need))
		}
	}

	c, err := copyVerified(ctx, src, dst, fmt.Sprintf("source %q is not a regular file; only files are copied", source))
	if err != nil {
		return fail(fmt.Errorf("%w; nothing was written", err))
	}
	if existing != nil {
		// Replace only the content that was confirmed: the destination must
		// still be the same file, unchanged, and hash to the confirmed
		// content right before the rename (a rewrite that kept its size and
		// time is caught by the hash, a file swapped in by its identity).
		if err := confirmedStill(ctx, dst, existing, res.ReplacedSHA256); err != nil {
			c.discard()
			return fail(fmt.Errorf("destination %q changed while the copy was made (%v); nothing was replaced", destination, err))
		}
		if err := os.Rename(c.tmp, dst); err != nil {
			c.discard()
			return fail(fmt.Errorf("replacing the destination: %w", err))
		}
		res.Status = CopyReplaced
	} else {
		if err := r.place(c.tmp, dst); err != nil {
			c.discard()
			return fail(err)
		}
		res.Status = CopyCopied
	}
	// The file now at the destination is the temporary copy that was read
	// back (linked or renamed, never rewritten), so its read-back hash is the
	// destination's.
	res.CreatedDirs = created
	res.Bytes, res.SourceSHA256, res.DestinationSHA256 = c.bytes, c.sum, c.readBack
	if err := syncDir(parent); err != nil {
		return res, fmt.Errorf("the copy is in place but its folder could not be flushed: %w", err)
	}
	return res, nil
}

// confirmedStill checks that dst is still the file confirmed for replacement:
// the same regular file, size and time, holding the confirmed SHA-256.
func confirmedStill(ctx context.Context, dst string, existing os.FileInfo, sum string) error {
	now, err := os.Lstat(dst)
	if err != nil {
		return err
	}
	if !now.Mode().IsRegular() || !sameFileState(existing, now) {
		return errors.New("not the same file")
	}
	got, _, err := hashPath(ctx, dst)
	if err != nil {
		return err
	}
	if got != sum {
		return errors.New("content differs from the confirmed hash")
	}
	if now, err = os.Lstat(dst); err != nil || !sameFileState(existing, now) {
		return errors.New("not the same file")
	}
	return nil
}
