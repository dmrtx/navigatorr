//go:build !linux && !darwin

package fsop

import "errors"

func renameNoReplace(from, to string) (bool, error) {
	return false, errors.New("rename without replace is not available on this platform")
}
