//go:build !(linux || darwin)

package nodes

import (
	"os"
	"path/filepath"
)

func dirOf(path string) string { return filepath.Dir(path) }

// ownerOf cannot name the owner here.
func ownerOf(fi os.FileInfo) (int, os.FileMode) { return -1, fi.Mode().Perm() }
