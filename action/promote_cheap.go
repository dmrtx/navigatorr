package action

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jakenesler/navigatorr/internal/sampledigest"
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
const promotionSampleChunk = sampledigest.Chunk

// promotionSampleRanges is how many interior ranges are sampled besides the
// first and the last. A file this many chunks long or shorter is read whole.
const promotionSampleRanges = sampledigest.Ranges

// originalByMetadata reports whether the original is proved by its size and
// modification time rather than by its digest.
func (p *promotionState) originalByMetadata() bool { return p.OriginalStatMod != "" }

// errPromotionOriginalChanged is a stat that no longer matches the transcode's.
var errPromotionOriginalChanged = errors.New("integrity violation: the original changed since it was transcoded (size or modification time)")

// promotionOriginalStatMatches reports whether the original is the file the
// transcode inspected, by the stat plan recorded.
func (e *Engine) promotionOriginalStatMatches(ctx context.Context, p *promotionState) error {
	want, err := time.Parse(time.RFC3339Nano, p.OriginalStatMod)
	if err != nil {
		return fmt.Errorf("promotion original stat is unreadable: %w", err)
	}
	var info os.FileInfo
	for attempt := 0; attempt < promotionStableHashAttempts; attempt++ {
		if info, err = e.promotionStatIdentity(p.OriginalPath); !errors.Is(err, os.ErrInvalid) {
			break
		}
		// A file whose identity moved as it was opened, which a network mount
		// reports transiently, as promotionHashStable retries.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	if info.Size() != p.OriginalBytes || !info.ModTime().Equal(want) {
		return errPromotionOriginalChanged
	}
	return nil
}

// promotionOriginalProved is the check of the original for a promotion that
// does not read it. Until the recovery copy exists the stat must match. After
// it, a file the library or a restore wrote again has a new modification time
// and is still the original when its size and sampled ranges reproduce the
// recovery copy's, which is what recovering from it needs.
func (e *Engine) promotionOriginalProved(ctx context.Context, p *promotionState) error {
	err := e.promotionOriginalStatMatches(ctx, p)
	// RecoverySample alone does not say a recovery exists: a move aside records it
	// at choice time. Only a published copy or a moved original makes the
	// sampled ranges a stand-in for the stat.
	if !errors.Is(err, errPromotionOriginalChanged) || p.RecoverySample == "" || !(p.BackupVerified || p.MovedAside) {
		return err
	}
	got, sampleErr := e.promotionSampleDigestStable(ctx, p.OriginalPath, p.OriginalBytes)
	if sampleErr != nil {
		return sampleErr
	}
	if got != p.RecoverySample {
		return errPromotionOriginalChanged
	}
	return nil
}

// verifyPromotionOriginal and verifyPromotionOriginalStable prove the original
// is the file that was transcoded: by stat when the promotion allows it, else
// by a full read like verifyPromotionHash and verifyPromotionHashStable.
func (e *Engine) verifyPromotionOriginal(ctx context.Context, p *promotionState) error {
	if p.originalByMetadata() {
		return e.promotionOriginalProved(ctx, p)
	}
	return e.verifyPromotionHash(ctx, p.OriginalPath, p.OriginalSHA)
}

func (e *Engine) verifyPromotionOriginalStable(ctx context.Context, p *promotionState) error {
	if p.originalByMetadata() {
		return e.promotionOriginalProved(ctx, p)
	}
	return e.verifyPromotionHashStable(ctx, p.OriginalPath, p.OriginalSHA)
}

// promotionSampleOffsets are the starts of the ranges sampled from a file of
// the given size.
func promotionSampleOffsets(size int64) []int64 { return sampledigest.Offsets(size) }

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
	return sampledigest.Digest(ctx, f, size)
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

// verifyPromotionReusedRecovery is verifyPromotionRecovery for a file an
// earlier attempt left. Until a copy of this promotion was proved against the
// original (RecoverySample), nothing says its bytes were ever complete, so it
// is read in full against the pinned digest, once, and then sampled. Nothing
// reads the original.
func (e *Engine) verifyPromotionReusedRecovery(ctx context.Context, p *promotionState, path string) error {
	if p.originalByMetadata() && p.RecoverySample == "" {
		if err := e.verifyPromotionHashStable(ctx, path, p.OriginalSHA); err != nil {
			return err
		}
		sum, err := e.promotionSampleDigestStable(ctx, path, p.OriginalBytes)
		if err != nil {
			return err
		}
		p.RecoverySample = sum
		return nil
	}
	return e.verifyPromotionRecovery(ctx, p, path)
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
	if want == "" && p.movesAside() {
		// The original is the recovery once moved: there is nothing left to take
		// the digest from.
		return fmt.Errorf("the recovery sample of the original is missing")
	}
	if want == "" {
		if err := e.promotionOriginalStatMatches(ctx, p); err != nil {
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
