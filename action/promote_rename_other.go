//go:build !linux && !darwin

package action

import "fmt"

func renamePromotionNoReplace(from, to string) error {
	return fmt.Errorf("atomic non-overwriting promotion rename is unsupported on this operating system")
}
