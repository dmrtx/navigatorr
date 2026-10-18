package action

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Recovery copy methods, cheapest first. A clone shares extents on a
// reflink-capable filesystem (btrfs, XFS, or an SMB server backed by one) and
// moves no data. A kernel copy moves no data through this process; over CIFS
// it becomes an SMB server-side copy. Streaming always works. The method is
// detected for each copy and recorded, never assumed.
const (
	promotionCopyClone  = "clone"
	promotionCopyKernel = "copy_file_range"
	promotionCopyStream = "stream"
)

// promotionCopyChunk bounds one kernel copy call, so a cancelled execution
// stops between chunks and progress keeps moving.
const promotionCopyChunk = 64 << 20

var errPromotionCopyUnsupported = errors.New("copy offload is not supported on this platform")

// promotionCopyUnsupported reports errors meaning "this filesystem or kernel
// cannot offload the copy", after which streaming is still safe.
func promotionCopyUnsupported(err error) bool {
	for _, target := range []error{errPromotionCopyUnsupported, syscall.ENOSYS, syscall.EXDEV, syscall.EOPNOTSUPP, syscall.ENOTSUP, syscall.EINVAL, syscall.ENOTTY} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// promotionCopyResult says how a recovery copy was made. CloneError keeps the
// reason a clone was not used, so production shows whether it ever works.
type promotionCopyResult struct {
	Method     string `json:"method"`
	DurationMs int64  `json:"duration_ms"`
	CloneError string `json:"clone_error,omitempty"`
	Streamed   int64  `json:"streamed_bytes,omitempty"`
}

// promotionCopyData fills the empty dst with src, holding a hash slot. Every
// method leaves the same contract: the caller syncs, closes and then reads
// the whole copy back against the original's SHA-256.
func (e *Engine) promotionCopyData(ctx context.Context, dst, src *os.File, path string, total int64, progress promotionHashProgress) (promotionCopyResult, error) {
	var result promotionCopyResult
	release, err := e.acquireHashSlot(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	report := func(n int64) error {
		if progress == nil || n == 0 {
			return nil
		}
		return progress(path, n, total)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	clone := e.promotionCloneHook
	if clone == nil {
		clone = promotionCloneFile
	}
	cloneErr := clone(dst, src)
	if cloneErr == nil {
		result.Method = promotionCopyClone
		return result, report(total)
	}
	// A clone either completes or changes nothing. Any refusal falls through
	// to a real copy, unless the destination is somehow no longer empty.
	if info, err := dst.Stat(); err != nil || info.Size() != 0 {
		if err == nil {
			err = fmt.Errorf("recovery partial is not empty after a refused clone: %w", cloneErr)
		}
		return result, err
	}
	result.CloneError = cloneErr.Error()
	copyRange := e.promotionCopyRangeHook
	if copyRange == nil {
		copyRange = promotionCopyRange
	}
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		n, err := copyRange(dst, src, promotionCopyChunk)
		if err != nil {
			// Both offsets advance only by what was copied, so streaming can
			// continue from here. Other errors are real and fail the copy.
			if promotionCopyUnsupported(err) {
				break
			}
			return result, err
		}
		if n == 0 {
			// EOF; or, before any byte, a filesystem that declines silently.
			if copied == 0 && total > 0 {
				break
			}
			result.Method = promotionCopyKernel
			return result, nil
		}
		copied += int64(n)
		if err := report(copied); err != nil {
			return result, err
		}
	}
	result.Method = promotionCopyStream
	result.Streamed, err = io.Copy(dst, &promotionContextReader{ctx: ctx, r: src, path: path, total: total, read: copied, progress: progress})
	return result, err
}
