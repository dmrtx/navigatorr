//go:build !linux && !darwin

package action

import "os"

// Without a known stat layout every check is a full hash.
func promotionFileFingerprint(os.FileInfo) (promotionFingerprint, bool) {
	return promotionFingerprint{}, false
}
