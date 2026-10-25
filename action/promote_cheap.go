package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Promotion without reading the original again.
//
// A transcode that was preflighted by metadata (see transcode_source_identity.go)
// pinned the original's SHA-256 once, while the worker copied it, and accept
// checked its size and modification time instead of reading it. Promotion keeps
// to that: when plan finds the original unchanged by the same measure it records
// OriginalStatMod, and from then on
//
//   - every check of the original compares its size and modification time with
//     the transcode's; the file is never hashed again;
//   - the recovery copy is a copy of the original by the filesystem (clone,
//     kernel copy or stream), proved by its size and by a digest over a few
//     ranges of it, taken from the original when the copy is made and kept in
//     the promotion. Later checks read those ranges of the recovery copy and
//     nothing of the original, which the library may already have removed.
//
// A promotion without OriginalStatMod (an action admitted before the flow, or
// one whose original changed) keeps every full read.

// promotionSampleChunk is the size of each sampled range.
const promotionSampleChunk = 1 << 20

// promotionSampleRanges is how many interior ranges are sampled besides the
// first and the last. A file this many chunks long or shorter is read whole.
const promotionSampleRanges = 6

// originalByMetadata reports whether the original is proved by its size and
// modification time rather than by its digest.
func (p *promotionState) originalByMetadata() bool { return p.OriginalStatMod != "" }

// promotionOriginalStatMatches reports whether the original is the file the
// transcode inspected, by the stat plan recorded.
func (e *Engine) promotionOriginalStatMatches(p *promotionState) error {
	want, err := time.Parse(time.RFC3339Nano, p.OriginalStatMod)
	if err != nil {
		return fmt.Errorf("promotion original stat is unreadable: %w", err)
	}
	info, err := e.promotionStatIdentity(p.OriginalPath)
	if err != nil {
		return err
	}
	if info.Size() != p.OriginalBytes || !info.ModTime().Equal(want) {
		return fmt.Errorf("integrity violation: the original changed since it was transcoded (size or modification time)")
	}
	return nil
}

// verifyPromotionOriginal and verifyPromotionOriginalStable prove the original
// is the file that was transcoded: by stat when the promotion allows it, else
// by a full read like verifyPromotionHash and verifyPromotionHashStable.
func (e *Engine) verifyPromotionOriginal(ctx context.Context, p *promotionState) error {
	if p.originalByMetadata() {
		return e.promotionOriginalStatMatches(p)
	}
	return e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA)
}

func (e *Engine) verifyPromotionOriginalStable(ctx context.Context, p *promotionState) error {
	if p.originalByMetadata() {
		return e.promotionOriginalStatMatches(p)
	}
	return e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA)
}

// promotionSampleOffsets are the starts of the ranges sampled from a file of
// the given size: all of it when short, else the first, the last and evenly
// spaced ranges between, aligned to 4 KiB.
func promotionSampleOffsets(size int64) []int64 {
	if size <= promotionSampleChunk*(promotionSampleRanges+2) {
		var offsets []int64
		for off := int64(0); off < size; off += promotionSampleChunk {
			offsets = append(offsets, off)
		}
		return offsets
	}
	last := size - promotionSampleChunk
	offsets := []int64{0}
	for i := int64(1); i <= promotionSampleRanges; i++ {
		offsets = append(offsets, (last*i/(promotionSampleRanges+1))&^4095)
	}
	return append(offsets, last)
}

// promotionSampleDigest hashes size and the sampled ranges of path. It fails
// when path is not exactly size bytes long.
func (e *Engine) promotionSampleDigest(ctx context.Context, path string, size int64) (string, error) {
	resolved, err := e.promotionPath(path, false)
	if err != nil {
		return "", err
	}
	before, err := e.promotionLstat(resolved)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", fmt.Errorf("promotion path is not a regular file: %s", path)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, opened) {
		return "", &promotionFileChangedError{path: path, phase: "opening"}
	}
	if opened.Size() != size {
		return "", fmt.Errorf("integrity violation: %s is %d bytes, expected %d", path, opened.Size(), size)
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", size)
	buf := make([]byte, promotionSampleChunk)
	for _, off := range promotionSampleOffsets(size) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := int64(len(buf))
		if rest := size - off; rest < n {
			n = rest
		}
		if _, err := f.ReadAt(buf[:n], off); err != nil {
			return "", err
		}
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// promotionSampleDigestStable retries promotionSampleDigest while the
// filesystem reports a file whose identity moved as it was opened, as
// promotionHashStable does for a full read.
func (e *Engine) promotionSampleDigestStable(ctx context.Context, path string, size int64) (string, error) {
	var lastErr error
	for attempt := 0; attempt < promotionStableHashAttempts; attempt++ {
		sum, err := e.promotionSampleDigest(ctx, path, size)
		if err == nil {
			return sum, nil
		}
		if !transientPromotionHashError(err) {
			return "", err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return "", lastErr
}

// verifyPromotionRecovery proves path holds the original's bytes: a full read
// against the original's digest, or, for a promotion that does not re-read the
// original, its size and sampled ranges against the digest of the same ranges
// of the original taken when the recovery copy was made. The first call that
// has no recorded digest takes it from the original, which is still in place
// because the library is not touched before the recovery copy exists.
func (e *Engine) verifyPromotionRecovery(ctx context.Context, p *promotionState, path string) error {
	if !p.originalByMetadata() {
		return e.verifyPromotionHashStable(ctx, path, p.OriginalSHA)
	}
	want := p.RecoverySample
	if want == "" {
		if err := e.promotionOriginalStatMatches(p); err != nil {
			return err
		}
		var err error
		if want, err = e.promotionSampleDigestStable(ctx, p.OriginalPath, p.OriginalBytes); err != nil {
			return err
		}
	}
	got, err := e.promotionSampleDigestStable(ctx, path, p.OriginalBytes)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("integrity violation: the recovery copy does not match the original")
	}
	p.RecoverySample = want
	return nil
}
