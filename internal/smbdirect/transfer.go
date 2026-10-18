package smbdirect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// Transfer reports what one storage operation moved over SMB. Byte counts are
// payload bytes through file handles, summed over every session the operation
// used, so a reconnect that re-reads data is visible in the totals.
type Transfer struct {
	ReadBytes    int64
	WrittenBytes int64
	// Attempts is the number of sessions used: 1 unless a reconnect recovered
	// the operation.
	Attempts int
	// Resumed reports that a download continued from its last locally written
	// offset after a reconnect instead of starting over.
	Resumed bool
	// ReusedPartial reports that a publish found this job's own partial already
	// holding exactly the expected bytes and skipped the upload.
	ReusedPartial bool
	// SHA256 is the digest of the bytes a download delivered locally.
	SHA256 string
}

// PublishExpect is the accepted candidate identity a publish must deliver.
type PublishExpect struct {
	SizeBytes int64
	SHA256    string
}

// countingShare counts payload bytes through the handles it opens.
type countingShare struct {
	share
	t *Transfer
}

func (c countingShare) Open(name string) (readFile, error) {
	f, err := c.share.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingReader{inner: f, n: &c.t.ReadBytes}, nil
}

func (c countingShare) OpenExclusive(name string) (writeFile, error) {
	f, err := c.share.OpenExclusive(name)
	if err != nil {
		return nil, err
	}
	return &countingWriter{inner: f, n: &c.t.WrittenBytes}, nil
}

var errNotSeekable = errors.New("SMB handle is not seekable")

type countingReader struct {
	inner readFile
	n     *int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	*r.n += int64(n)
	return n, err
}

func (r *countingReader) Seek(offset int64, whence int) (int64, error) {
	s, ok := r.inner.(io.Seeker)
	if !ok {
		return 0, errNotSeekable
	}
	return s.Seek(offset, whence)
}

func (r *countingReader) Close() error { return r.inner.Close() }

type countingWriter struct {
	inner writeFile
	n     *int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.inner.Write(p)
	*w.n += int64(n)
	return n, err
}

func (w *countingWriter) Sync() error  { return w.inner.Sync() }
func (w *countingWriter) Close() error { return w.inner.Close() }

// DownloadAtomic copies a remote source to a local SSD path and publishes the
// local file only after a complete sync and size check.
func (s *Store) DownloadAtomic(ctx context.Context, remoteLocalPath, localPath string) error {
	_, err := s.DownloadAtomicVerified(ctx, remoteLocalPath, localPath, "")
	return err
}

// DownloadAtomicVerified is DownloadAtomic that also hashes the bytes while
// they stream to disk, so the caller can check the source digest without a
// second local read.
//
// expectedSHA256 does not make this function reject anything: the caller
// compares Transfer.SHA256 and fails closed. It does permit resuming after a
// reconnect. A resumed download splices bytes from two sessions, which is only
// safe when the remote file looks unchanged (same size and modification time)
// AND the caller will verify the whole-file digest. Without an expected digest
// a reconnect restarts from byte zero, as it always has.
func (s *Store) DownloadAtomicVerified(ctx context.Context, remoteLocalPath, localPath, expectedSHA256 string) (t Transfer, retErr error) {
	defer func() { retErr = wrapError("download", retErr) }()
	remote, ok := s.RemotePath(remoteLocalPath)
	if !ok {
		return t, fmt.Errorf("path %q is outside smb_direct.local_root", remoteLocalPath)
	}
	allowResume := strings.TrimSpace(expectedSHA256) != ""
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return t, fmt.Errorf("creating local staging directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(localPath), ".smb-download-*.partial")
	if err != nil {
		return t, fmt.Errorf("creating local staging file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	// written counts bytes that reached the local file AND the hash. It is the
	// only offset a resumed session may continue from.
	var written int64
	var firstInfo os.FileInfo
	reset := func() error {
		written = 0
		h.Reset()
		if err := tmp.Truncate(0); err != nil {
			return fmt.Errorf("resetting local staging file: %w", err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewinding local staging file: %w", err)
		}
		return nil
	}
	err = s.withSessionRetry(ctx, "download", func(raw share) error {
		t.Attempts++
		fs := countingShare{share: raw, t: &t}
		info, err := fs.Stat(remote)
		if err != nil {
			return fmt.Errorf("statting SMB source: %w", err)
		}
		if !info.Mode().IsRegular() {
			return errors.New("SMB source is not a regular file")
		}
		resume := allowResume && written > 0 && sameRemoteVersion(firstInfo, info)
		if resume {
			// Drop any tail a failed local write may have left past the
			// hashed offset, then continue exactly there.
			if err := tmp.Truncate(written); err != nil {
				return fmt.Errorf("trimming local staging file: %w", err)
			}
			if _, err := tmp.Seek(written, io.SeekStart); err != nil {
				return fmt.Errorf("positioning local staging file: %w", err)
			}
		} else {
			// A recovered session that cannot prove the source unchanged starts
			// a fresh read. Never leave bytes from a longer earlier attempt.
			if err := reset(); err != nil {
				return err
			}
			firstInfo = info
		}
		src, err := fs.Open(remote)
		if err != nil {
			return fmt.Errorf("opening SMB source: %w", err)
		}
		if resume {
			if pos, serr := src.(io.Seeker).Seek(written, io.SeekStart); serr != nil || pos != written {
				_ = src.Close()
				if err := reset(); err != nil {
					return err
				}
				firstInfo = info
				if src, err = fs.Open(remote); err != nil {
					return fmt.Errorf("reopening SMB source: %w", err)
				}
			} else {
				t.Resumed = true
			}
		}
		// Exercise an authenticated source read before committing the staged
		// download. Small inputs may reach EOF within this preflight.
		preflight := make([]byte, 64*1024)
		read, readErr := io.ReadFull(src, preflight)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			_ = src.Close()
			return fmt.Errorf("reading SMB source preflight: %w", readErr)
		}
		copyErr := copyHashed(ctx, tmp, h, io.MultiReader(bytes.NewReader(preflight[:read]), src), &written)
		closeErr := src.Close()
		if copyErr != nil {
			return fmt.Errorf("reading SMB source: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing SMB source: %w", closeErr)
		}
		if written != info.Size() {
			return fmt.Errorf("SMB source changed while downloading: copied=%d stat=%d", written, info.Size())
		}
		return nil
	})
	if err != nil {
		return t, fmt.Errorf("downloading %q: %w", remoteLocalPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return t, fmt.Errorf("syncing staged input: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return t, fmt.Errorf("closing staged input: %w", err)
	}
	if err := os.Rename(tmpName, localPath); err != nil {
		return t, fmt.Errorf("publishing staged input: %w", err)
	}
	t.SHA256 = hex.EncodeToString(h.Sum(nil))
	return t, nil
}

// sameRemoteVersion reports whether two stats of one remote file describe the
// same version. A zero modification time is never evidence.
func sameRemoteVersion(a, b os.FileInfo) bool {
	if a == nil || b == nil || a.ModTime().IsZero() {
		return false
	}
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// copyHashed writes src to dst and feeds exactly the bytes that dst accepted
// into h, advancing *written with them.
func copyHashed(ctx context.Context, dst io.Writer, h hash.Hash, src io.Reader, written *int64) error {
	buf := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			w, writeErr := dst.Write(buf[:n])
			if w > 0 {
				_, _ = h.Write(buf[:w])
				*written += int64(w)
			}
			if writeErr != nil {
				return writeErr
			}
			if w != n {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// Publish uploads a local candidate to an exclusive job-owned partial, reads
// the entire partial back for SHA-256 verification, and renames without
// replacement. A retry succeeds only if an existing final is byte-identical.
func (s *Store) Publish(ctx context.Context, localCandidate, destination, jobID string) error {
	return s.PublishGuarded(ctx, localCandidate, destination, jobID, nil)
}

// PublishGuarded copies and verifies outside the guard, then guards only the
// atomic no-replace rename. Recovery repeats that guard on every fresh session.
// It hashes the local candidate first because its caller supplies no accepted
// identity; PublishVerified skips that local pass.
func (s *Store) PublishGuarded(ctx context.Context, localCandidate, destination, jobID string, guard func(func() error) error) error {
	localHash, localSize, err := hashLocal(ctx, localCandidate)
	if err != nil {
		return wrapError("publish", fmt.Errorf("hashing local candidate: %w", err))
	}
	_, err = s.PublishVerified(ctx, localCandidate, destination, jobID, PublishExpect{SizeBytes: localSize, SHA256: localHash}, guard)
	return err
}

// remoteIdentity is what a stat can tell about one remote file version. SMB
// exposes no stable file ID through this client, so the creation and last
// write times stand in for it; a server-side rename preserves both.
type remoteIdentity struct {
	size  int64
	mod   time.Time
	birth time.Time
}

func identityOf(info os.FileInfo) remoteIdentity {
	id := remoteIdentity{size: info.Size(), mod: info.ModTime()}
	if st, ok := info.Sys().(*smb2.FileStat); ok {
		id.birth = st.CreationTime
	}
	return id
}

// matches is true only with a usable timestamp: zero times prove nothing.
func (id remoteIdentity) matches(info os.FileInfo) bool {
	if id.mod.IsZero() {
		return false
	}
	other := identityOf(info)
	return id.size == other.size && id.mod.Equal(other.mod) && id.birth.Equal(other.birth)
}

// PublishVerified publishes localCandidate at destination with the accepted
// identity expect, moving the minimum data:
//
//   - The upload is hashed as it streams, so a local candidate that no longer
//     matches expect is caught without a separate local read.
//   - The partial is read back exactly once. That read-back is the proof that
//     the server holds the right bytes.
//   - The no-replace rename is guarded and is not followed by a re-hash: a
//     server-side rename does not change bytes. The final is checked by stat
//     identity (size, creation and write time) against the verified partial.
//   - A reconnect never redoes completed work. A partial that already holds
//     the expected bytes (proved by identity within this call, or by one
//     read-back otherwise) is reused, and a final whose identity matches the
//     verified partial is accepted as this job's own committed rename.
//
// Any final that is not provably this job's bytes is never replaced.
func (s *Store) PublishVerified(ctx context.Context, localCandidate, destination, jobID string, expect PublishExpect, guard func(func() error) error) (t Transfer, retErr error) {
	defer func() { retErr = wrapError("publish", retErr) }()
	remote, ok := s.RemotePath(destination)
	if !ok {
		return t, fmt.Errorf("path %q is outside smb_direct.local_root", destination)
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || strings.ContainsAny(jobID, `/\\`) || jobID == "." || jobID == ".." {
		return t, errors.New("invalid job id for SMB publication")
	}
	expect.SHA256 = strings.ToLower(strings.TrimSpace(expect.SHA256))
	if raw, err := hex.DecodeString(expect.SHA256); err != nil || len(raw) != sha256.Size || expect.SizeBytes <= 0 {
		return t, errors.New("SMB publication requires the accepted candidate size and SHA-256")
	}
	partial := remote + ".partial." + jobID
	// verified is the identity of the partial once its bytes were proved.
	var verified *remoteIdentity
	err := s.withSessionRetry(ctx, "publish", func(raw share) (retErr error) {
		t.Attempts++
		fs := countingShare{share: raw, t: &t}
		if err := ensureRemoteDir(fs, path.Dir(remote)); err != nil {
			return fmt.Errorf("ensuring SMB destination directory: %w", err)
		}
		if info, err := fs.Stat(remote); err == nil {
			return s.reconcileExistingFinal(ctx, fs, info, remote, partial, destination, expect, verified)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking destination: %w", err)
		}

		created := false
		defer func() {
			// A transport failure keeps the job-owned partial: the recovery
			// session (or a later finalization retry) may reuse its bytes.
			if created && retErr != nil && !reconnectable(retErr) {
				_ = fs.Remove(partial)
			}
		}()

		reused := false
		if info, err := fs.Stat(partial); err == nil {
			created = true
			if info.Mode().IsRegular() && info.Size() == expect.SizeBytes {
				if verified != nil && verified.matches(info) {
					reused = true
				} else {
					hash, size, err := hashRemote(ctx, fs, partial)
					if err != nil {
						return fmt.Errorf("reading back existing SMB partial: %w", err)
					}
					if size == expect.SizeBytes && hash == expect.SHA256 {
						id := identityOf(info)
						verified, reused = &id, true
					}
				}
			}
			if !reused {
				// This exact name is owned by this job. A crashed upload may have
				// left it behind; remove only that partial and create it afresh.
				if err := fs.Remove(partial); err != nil {
					return fmt.Errorf("removing stale job-owned SMB partial: %w", err)
				}
				created = false
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking SMB partial: %w", err)
		}
		if reused {
			t.ReusedPartial = true
		} else {
			if err := uploadPartial(ctx, fs, localCandidate, partial, expect, &created); err != nil {
				return err
			}
			hash, size, err := hashRemote(ctx, fs, partial)
			if err != nil {
				return fmt.Errorf("reading back SMB partial: %w", err)
			}
			if size != expect.SizeBytes || hash != expect.SHA256 {
				return fmt.Errorf("SMB partial readback mismatch: bytes=%d/%d sha256=%s/%s", size, expect.SizeBytes, hash, expect.SHA256)
			}
			info, err := fs.Stat(partial)
			if err != nil {
				return fmt.Errorf("statting verified SMB partial: %w", err)
			}
			id := identityOf(info)
			verified = &id
		}

		rename := func() error { return fs.RenameNoReplace(partial, remote) }
		var renameErr error
		if guard != nil {
			renameErr = guard(rename)
		} else {
			renameErr = rename()
		}
		if err := renameErr; err != nil {
			if os.IsExist(err) {
				info, statErr := fs.Stat(remote)
				if statErr != nil {
					return fmt.Errorf("verifying destination after rename conflict: %w", statErr)
				}
				if err := s.reconcileExistingFinal(ctx, fs, info, remote, partial, destination, expect, nil); err != nil {
					return err
				}
				created = false
				return nil
			}
			return fmt.Errorf("renaming SMB partial without replacement: %w", err)
		}
		created = false
		info, err := fs.Stat(remote)
		if err != nil {
			return fmt.Errorf("statting SMB final after rename: %w", err)
		}
		if !info.Mode().IsRegular() || info.Size() != expect.SizeBytes {
			return fmt.Errorf("SMB final verification mismatch: bytes=%d/%d", info.Size(), expect.SizeBytes)
		}
		if verified == nil || !verified.matches(info) {
			// No usable identity (a server that reports no write time) or a
			// timestamp that changed across the rename: the rename of verified
			// bytes most likely succeeded, but prove it rather than assume.
			hash, size, err := hashRemote(ctx, fs, remote)
			if err != nil {
				return fmt.Errorf("verifying SMB final: %w", err)
			}
			if size != expect.SizeBytes || hash != expect.SHA256 {
				return fmt.Errorf("SMB final verification mismatch: bytes=%d/%d sha256=%s/%s", size, expect.SizeBytes, hash, expect.SHA256)
			}
		}
		return nil
	})
	return t, err
}

// reconcileExistingFinal decides whether an existing final is this job's
// publication. A size mismatch is a conflict without any read. A final whose
// identity matches the partial this call verified, with that partial gone, is
// the rename this job committed before losing the reply. Anything else must be
// byte-identical to the accepted candidate.
func (s *Store) reconcileExistingFinal(ctx context.Context, fs share, info os.FileInfo, remote, partial, destination string, expect PublishExpect, verified *remoteIdentity) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: remote destination is not a regular file", ErrDestinationExists)
	}
	if info.Size() != expect.SizeBytes {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
	}
	if verified != nil && verified.matches(info) {
		if _, err := fs.Stat(partial); os.IsNotExist(err) {
			return nil
		}
	}
	hash, size, err := hashRemote(ctx, fs, remote)
	if err != nil {
		return fmt.Errorf("verifying existing destination: %w", err)
	}
	if size != expect.SizeBytes || hash != expect.SHA256 {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
	}
	if _, err := fs.Stat(partial); err == nil {
		_ = fs.Remove(partial)
	}
	return nil
}

// uploadPartial creates this job's exclusive partial and streams the local
// candidate into it, hashing what it sends. Bytes that differ from the
// accepted identity mean the local candidate changed after validation.
func uploadPartial(ctx context.Context, fs share, localCandidate, partial string, expect PublishExpect, created *bool) error {
	dst, err := fs.OpenExclusive(partial)
	if err != nil {
		return fmt.Errorf("creating exclusive SMB partial: %w", err)
	}
	*created = true
	src, err := os.Open(localCandidate)
	if err != nil {
		_ = dst.Close()
		return err
	}
	h := sha256.New()
	sent, copyErr := copyContext(ctx, dst, io.TeeReader(src, h))
	closeSrcErr := src.Close()
	syncErr := dst.Sync()
	closeDstErr := dst.Close()
	if err := errors.Join(copyErr, closeSrcErr, syncErr, closeDstErr); err != nil {
		return fmt.Errorf("uploading SMB partial: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); sent != expect.SizeBytes || got != expect.SHA256 {
		return fmt.Errorf("local candidate no longer matches its accepted identity: bytes=%d/%d sha256=%s/%s", sent, expect.SizeBytes, got, expect.SHA256)
	}
	return nil
}

// RemovePartial deletes this job's own "<destination>.partial.<jobID>" on the
// share, if present. Call it only when the job will never publish again
// (cancelled or terminally failed): a partial kept after a transport failure
// for reuse is otherwise M bytes left on the NAS. The final name is never
// touched, and a missing partial is not an error.
func (s *Store) RemovePartial(ctx context.Context, destination, jobID string) (retErr error) {
	defer func() { retErr = wrapError("remove partial", retErr) }()
	remote, ok := s.RemotePath(destination)
	if !ok {
		return fmt.Errorf("path %q is outside smb_direct.local_root", destination)
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || strings.ContainsAny(jobID, `/\\`) || jobID == "." || jobID == ".." {
		return errors.New("invalid job id for SMB partial cleanup")
	}
	partial := remote + ".partial." + jobID
	return s.withSessionRetry(ctx, "remove partial", func(fs share) error {
		info, err := fs.Stat(partial)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("SMB partial %s is not a regular file; left in place", partial)
		}
		if err := fs.Remove(partial); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}
