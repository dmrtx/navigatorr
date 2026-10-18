//go:build !linux

package action

import "os"

// Recovery copies stream on platforms without FICLONE and copy_file_range.
func promotionCloneFile(_, _ *os.File) error { return errPromotionCopyUnsupported }

func promotionCopyRange(_, _ *os.File, _ int) (int, error) { return 0, errPromotionCopyUnsupported }
