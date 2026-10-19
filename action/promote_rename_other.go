//go:build !linux && !darwin

package action

import (
	"errors"
	"fmt"
)

// errors.ErrUnsupported makes a move-aside fall back to a recovery copy.
func renamePromotionNoReplace(from, to string) error {
	return fmt.Errorf("atomic non-overwriting promotion rename is unsupported on this operating system: %w", errors.ErrUnsupported)
}
