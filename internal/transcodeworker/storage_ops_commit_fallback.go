//go:build !darwin

package transcodeworker

// renameNoReplace reports that this platform has no native atomic no-replace
// rename primitive wired up, so commitNoReplace uses the hard-link fallback and
// then, when hard links are unavailable too, the exclusive-copy fallback. The
// fallbacks remain no-clobber; a filesystem that supports none of the
// primitives fails closed with ErrStorageIO rather than clobbering the
// destination.
func renameNoReplace(src, dst string) error {
	return errNoReplaceUnsupported
}
