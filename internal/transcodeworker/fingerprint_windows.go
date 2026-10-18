//go:build windows

package transcodeworker

import "os"

// fingerprintOf has no inode to bind to on Windows, so every digest check
// reads the file again.
func fingerprintOf(os.FileInfo) (fileFingerprint, bool) { return fileFingerprint{}, false }

// deviceOf is unavailable on Windows; publication support is probed per call.
func deviceOf(string) (uint64, bool) { return 0, false }

// diskSpace is unavailable on Windows; the cache keeps only its byte cap.
func diskSpace(string) (avail, total uint64, ok bool) { return 0, 0, false }
