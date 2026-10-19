//go:build !linux

package action

// promotionFilesystemName is only known on Linux.
func promotionFilesystemName(string) string { return "" }
