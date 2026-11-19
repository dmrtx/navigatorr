package action

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A failed removal is told apart: a file-system fault is "could not remove
// candidate file" (a server failure for the transcode API), anything else a
// "candidate file kept" state refusal.
func TestCandidateKeptErrorSeparatesIOFaultsFromStateRefusals(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&fs.PathError{Op: "remove", Path: "/x", Err: syscall.EACCES}, "could not remove candidate file: "},
		{&os.LinkError{Op: "rename", Old: "/a", New: "/b", Err: syscall.EIO}, "could not remove candidate file: "},
		{fmt.Errorf("wrapped: %w", syscall.ENOSPC), "could not remove candidate file: "},
		{errors.New("sample-episode.mkv is not a regular file"), "candidate file kept: "},
		{errors.New("the retained candidate could not be released on its worker: connection refused"), "candidate file kept: "},
	} {
		if got := candidateKeptError(c.err).Error(); !strings.HasPrefix(got, c.want) {
			t.Errorf("%v: %q, want prefix %q", c.err, got, c.want)
		}
	}
}
