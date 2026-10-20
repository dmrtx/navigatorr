package transcodeworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Publication methods recorded in a publicationReport.
const (
	publishMethodRename = "rename" // atomic no-replace rename of the partial
	publishMethodLink   = "link"   // atomic hard link of the partial
	publishMethodCopy   = "copy"   // exclusive-create copy after the partial
	publishMethodDirect = "direct" // exclusive create of the final name, no partial
)

// publicationReport travels on the publish context. The caller may preset the
// local candidate and its accepted digest; the finalizer fills in how the
// output was committed and how many bytes crossed the share.
type publicationReport struct {
	// Inputs.
	localSource string // the local candidate; lets the copy fallback skip re-reading the partial
	expectSHA   string // accepted candidate digest, checked while copying from localSource

	// Outputs.
	Method            string
	CopyFallback      bool  // the output was not committed atomically
	AtomicUnsupported bool  // both primitives definitely refused; safe to remember
	ReadBytes         int64 // bytes read back from the share
	WrittenBytes      int64 // bytes written to the share
}

type publicationReportKey struct{}

func withPublicationReport(ctx context.Context, rep *publicationReport) context.Context {
	return context.WithValue(ctx, publicationReportKey{}, rep)
}

func publicationReportFrom(ctx context.Context) *publicationReport {
	if ctx == nil {
		return nil
	}
	rep, _ := ctx.Value(publicationReportKey{}).(*publicationReport)
	return rep
}

// commitCaps remembers, per destination filesystem, that neither an atomic
// no-replace rename nor a hard link worked, so later publications to that
// share skip the partial and its second transfer. The knowledge expires so a
// remounted or upgraded share is probed again.
type commitCaps struct {
	mu      sync.Mutex
	noAtom  map[uint64]time.Time
	horizon time.Duration
}

var sharedCommitCaps = &commitCaps{noAtom: map[uint64]time.Time{}, horizon: time.Hour}

func (c *commitCaps) atomicUnsupported(dir string) bool {
	if c == nil {
		return false
	}
	dev, ok := deviceOf(dir)
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.noAtom[dev]
	if ok && time.Since(at) > c.horizon {
		delete(c.noAtom, dev)
		return false
	}
	return ok
}

func (c *commitCaps) markAtomicUnsupported(dir string) {
	if c == nil {
		return
	}
	if dev, ok := deviceOf(dir); ok {
		c.mu.Lock()
		c.noAtom[dev] = time.Now()
		c.mu.Unlock()
	}
}

// exclusiveCopy creates dst with O_EXCL and copies src into it, hashing the
// bytes into h when non-nil. On an ordinary failure dst is removed; when the
// bytes were written and closed but the entry could not be observed it is
// preserved and ErrAmbiguousPublication is returned. src is never removed.
func exclusiveCopy(ctx context.Context, src, dst string, hook func(dstPath string) error, h io.Writer) (err error) {
	info, err := regularFileInfo(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		if os.IsExist(err) {
			return fs.ErrExist
		}
		return fmt.Errorf("%w: creating %s: %v", ErrStorageIO, dst, err)
	}
	published := false
	// preserve keeps a written-and-closed destination when its post-close
	// visibility could not be established; the bytes may be a complete, valid
	// copy and must not be destroyed before content-equality reconciliation.
	preserve := false
	defer func() {
		if !published && !preserve {
			_ = out.Close()
			_ = os.Remove(dst)
		}
	}()
	if hook != nil {
		if herr := hook(dst); herr != nil {
			return herr
		}
	}
	if err := copyRegularFileToHashed(ctx, src, out, info.Size(), info.Mode().Perm(), h); err != nil {
		if errors.Is(err, errPostCloseUnverified) {
			preserve = true
			return fmt.Errorf("%w: %v", ErrAmbiguousPublication, err)
		}
		return err
	}
	published = true
	return nil
}

// copyFallbackSource picks what the exclusive-copy fallback reads. The local
// candidate gives the same bytes as the partial (which was written from it and
// size-verified) without reading M back over the network; the partial is used
// when no candidate is known or its size no longer matches.
func copyFallbackSource(rep *publicationReport, partial string) (string, hash.Hash) {
	if rep == nil || strings.TrimSpace(rep.localSource) == "" {
		return partial, nil
	}
	li, lerr := os.Stat(rep.localSource)
	pi, perr := os.Stat(partial)
	if lerr != nil || perr != nil || !li.Mode().IsRegular() || li.Size() != pi.Size() {
		return partial, nil
	}
	if rep.expectSHA == "" {
		return rep.localSource, nil
	}
	return rep.localSource, sha256.New()
}

// checkCopiedDigest enforces the accepted digest on bytes copied from the
// local candidate. dst was created exclusively by this call, so removing it on
// a mismatch never touches anyone else's file.
func checkCopiedDigest(rep *publicationReport, h hash.Hash, dst string) error {
	if h == nil || rep == nil || rep.expectSHA == "" {
		return nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, rep.expectSHA) {
		_ = os.Remove(dst)
		return candidateInvalid(fmt.Errorf("%w: local candidate no longer matches its accepted sha256; publication withdrawn", ErrSourceInvalid))
	}
	return nil
}

// linkDefinitelyUnsupported reports whether a hard-link failure means the
// filesystem cannot link at all, as opposed to a transient or unrelated
// failure that must not change how later publications commit.
func linkDefinitelyUnsupported(err error) bool {
	if err == nil || errors.Is(err, fs.ErrExist) || errors.Is(err, ErrSourceInvalid) {
		return false
	}
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL)
}

// logCopyFallback makes every non-atomic publication visible in the worker log.
func logCopyFallback(method, dst string) {
	log.Printf("transcode publication: %s supports neither an atomic no-replace rename nor hard links; committed by %s exclusive create (not atomic)", filepath.Dir(dst), method)
}
