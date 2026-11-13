//go:build !darwin && !linux

package transcodeworker

import "os"

// openFilePath is unknown here: a rotated log is truncated without keeping the
// old text.
func openFilePath(*os.File) string { return "" }
