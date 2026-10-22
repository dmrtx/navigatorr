//go:build !linux && !darwin

package action

import "os"

// candidateLastLink cannot read a link count here, so removing a candidate
// frees nothing in the report rather than overstating the space.
func candidateLastLink(os.FileInfo) bool { return false }
