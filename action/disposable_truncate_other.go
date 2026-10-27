//go:build !(linux || darwin)

package action

import (
	"errors"
	"os"
)

// truncateDisposable is unavailable here; the file is unlinked as it was.
func truncateDisposable(string, os.FileInfo) error {
	return errors.New("not supported on this platform")
}
