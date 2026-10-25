//go:build !unix

package fsop

import "errors"

// SpaceOf is not available on this platform.
func SpaceOf(string) (Space, error) {
	return Space{}, errors.New("filesystem space is not available on this platform")
}
