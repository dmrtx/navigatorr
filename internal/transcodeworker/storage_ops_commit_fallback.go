//go:build !darwin

package transcodeworker

// renameNoReplace reports that this platform has no native atomic no-replace
// rename primitive wired up, so commitNoReplace uses the hard-link fallback.
// The fallback is still atomic and no-clobber; filesystems without hard-link
// support fail closed with ErrStorageIO rather than clobbering the destination.
func renameNoReplace(src, dst string) error {
	return errNoReplaceUnsupported
}
