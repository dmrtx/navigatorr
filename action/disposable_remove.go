package action

import "os"

// Outcomes of emptying a throwaway file before it is unlinked.
const (
	truncateEmptied = "emptied"
	truncateSkipped = "skipped: "
)

// candidateRemoval is what removing an attempt's candidate files did.
type candidateRemoval struct {
	// Freed is the bytes the removal gave back (names that another link still
	// holds free nothing).
	Freed int64
	// Truncate says what emptying the files first did: "" when it was not
	// attempted (the setting is off, or every file was already empty),
	// "emptied", or "skipped: <reason>". A skip wins over an emptied file.
	Truncate string
}

// add folds one more file's outcome in.
func (r *candidateRemoval) add(freed int64, truncate string) {
	r.Freed += freed
	switch {
	case truncate == "":
	case r.Truncate == "" || (r.Truncate == truncateEmptied && truncate != truncateEmptied):
		r.Truncate = truncate
	}
}

// truncateBeforeDelete reports whether throwaway files are emptied before they
// are unlinked (cleanup.truncate_before_delete).
func (e *Engine) truncateBeforeDelete() bool {
	return e.deps.Config != nil && e.deps.Config.Cleanup.TruncateBeforeDelete
}

// removeDisposable unlinks a file Navigatorr created and nothing else needs.
// info is the file's Lstat from just before; when the setting is on and this
// name is the file's only link, the file is emptied first, so a share that
// keeps SMB deletes in a recycle bin keeps an empty file, not the bytes.
// Emptying is an optimization and never fails the removal: whatever stops it
// (a link, a changed file, no write access) leaves the plain unlink, and the
// reason is returned. A missing file is already removed.
func (e *Engine) removeDisposable(path string, info os.FileInfo) (string, error) {
	var truncate string
	if e.truncateBeforeDelete() && info != nil && info.Size() > 0 {
		truncate = truncateEmptied
		if !candidateLastLink(info) {
			truncate = truncateSkipped + "another name may still link to the file"
		} else if err := truncateDisposable(path, info); err != nil {
			truncate = truncateSkipped + err.Error()
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return truncate, err
	}
	return truncate, nil
}
