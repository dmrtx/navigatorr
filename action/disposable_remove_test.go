//go:build linux || darwin

package action

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/store"
)

// truncateFixture is a finished attempt with a 40-byte candidate and a 25-byte
// partial beside a sibling attempt's candidate and the library original.
type truncateFixture struct {
	e                                     *Engine
	st                                    *store.Store
	original, candidate, partial, sibling string
}

func newTruncateFixture(t *testing.T, truncate bool) *truncateFixture {
	t.Helper()
	root := candidateRoot(t)
	st := setupTestStore(t)
	e := discardEngine(t, st, root)
	e.deps.Config.Cleanup.TruncateBeforeDelete = truncate
	original := filepath.Join(root, "Sample Show", "Season 1", "sample-episode-01.mkv")
	writeCandidateFile(t, original, "original")
	candidate := seedCandidateAttempt(t, st, "attempt", StatusCancelled, original, "job-attempt", nil, nil)
	writeCandidateFile(t, candidate, strings.Repeat("c", 40))
	partial := candidate + ".partial.job-attempt"
	writeCandidateFile(t, partial, strings.Repeat("p", 25))
	sibling := filepath.Join(filepath.Dir(candidate), "sample-episode-02.job-other.mkv")
	writeCandidateFile(t, sibling, "another attempt")
	return &truncateFixture{e: e, st: st, original: original, candidate: candidate, partial: partial, sibling: sibling}
}

// holdOpen keeps a descriptor on path, so its size can still be read after the
// name is unlinked: an emptied file reads 0, an untouched one its full size.
func holdOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func sizeOf(t *testing.T, f *os.File) int64 {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func (f *truncateFixture) discard(t *testing.T) map[string]any {
	t.Helper()
	if _, err := f.e.DiscardLeftoverCandidate(context.Background(), "attempt"); err != nil {
		t.Fatal(err)
	}
	return candidateCleanupOf(t, f.e, mustInstance(t, f.st, "attempt"))
}

func TestDiscardKeepsBytesUntilUnlinkWhenTruncationIsOff(t *testing.T) {
	f := newTruncateFixture(t, false)
	cand, part := holdOpen(t, f.candidate), holdOpen(t, f.partial)
	cleanup := f.discard(t)
	if sizeOf(t, cand) != 40 || sizeOf(t, part) != 25 {
		t.Fatal("files were emptied with the setting off")
	}
	if _, ok := cleanup["truncate"]; ok || getString(cleanup, "outcome") != "removed" {
		t.Fatalf("setting off must leave no truncate note: %v", cleanup)
	}
}

func TestDiscardEmptiesTheCandidateAndPartialBeforeUnlinking(t *testing.T) {
	f := newTruncateFixture(t, true)
	cand, part := holdOpen(t, f.candidate), holdOpen(t, f.partial)
	cleanup := f.discard(t)
	if sizeOf(t, cand) != 0 || sizeOf(t, part) != 0 {
		t.Fatalf("files were not emptied: %d %d", sizeOf(t, cand), sizeOf(t, part))
	}
	if getString(cleanup, "truncate") != truncateEmptied || getInt(cleanup, "bytes_freed") != 65 || getString(cleanup, "outcome") != "removed" {
		t.Fatalf("outcome: %v", cleanup)
	}
	for _, path := range []string{f.candidate, f.partial} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s kept: %v", filepath.Base(path), err)
		}
	}
	for _, path := range []string{f.original, f.sibling} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatalf("%s touched: %v", filepath.Base(path), err)
		}
	}
}

func TestDiscardNeverEmptiesAFileAnotherNameStillLinks(t *testing.T) {
	f := newTruncateFixture(t, true)
	other := filepath.Join(filepath.Dir(f.original), "linked-elsewhere.mkv")
	if err := os.Link(f.candidate, other); err != nil {
		t.Fatal(err)
	}
	cand, part := holdOpen(t, f.candidate), holdOpen(t, f.partial)
	cleanup := f.discard(t)
	if sizeOf(t, cand) != 40 {
		t.Fatal("a file with another link was emptied")
	}
	if got, err := os.ReadFile(other); err != nil || string(got) != strings.Repeat("c", 40) {
		t.Fatalf("the other name lost its bytes: %q %v", got, err)
	}
	if sizeOf(t, part) != 0 {
		t.Fatal("the unlinked partial was not emptied")
	}
	if !strings.HasPrefix(getString(cleanup, "truncate"), truncateSkipped) || getInt(cleanup, "bytes_freed") != 25 {
		t.Fatalf("a skip must be reported and only the partial counted as freed: %v", cleanup)
	}
}

func TestDiscardNeverEmptiesAFileSwappedAfterItWasChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(t *testing.T, f *truncateFixture, victim string)
	}{
		{"a symlink to another file", func(t *testing.T, f *truncateFixture, victim string) {
			if err := os.Remove(f.candidate); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, f.candidate); err != nil {
				t.Fatal(err)
			}
		}},
		{"a different file of another size", func(t *testing.T, f *truncateFixture, victim string) {
			writeCandidateFile(t, f.candidate+".new", strings.Repeat("n", 55))
			if err := os.Rename(f.candidate+".new", f.candidate); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTruncateFixture(t, true)
			victim := filepath.Join(filepath.Dir(f.original), "victim.mkv")
			writeCandidateFile(t, victim, "must survive")
			var swapped *os.File
			f.e.candidateRemoveHook = func(path string) {
				if path != f.candidate {
					return
				}
				tc.swap(t, f, victim)
				if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
					swapped = holdOpen(t, path)
				}
			}
			cleanup := f.discard(t)
			if got, err := os.ReadFile(victim); err != nil || string(got) != "must survive" {
				t.Fatalf("the swap target was emptied: %q %v", got, err)
			}
			if swapped != nil && sizeOf(t, swapped) != 55 {
				t.Fatal("the swapped-in file was emptied")
			}
			if !strings.HasPrefix(getString(cleanup, "truncate"), truncateSkipped) {
				t.Fatalf("the skip must be reported: %v", cleanup)
			}
		})
	}
}

func TestDiscardNeverEmptiesAFileThatGainedALinkAfterItWasChecked(t *testing.T) {
	f := newTruncateFixture(t, true)
	other := filepath.Join(filepath.Dir(f.original), "linked-later.mkv")
	f.e.candidateRemoveHook = func(path string) {
		if path == f.candidate {
			if err := os.Link(path, other); err != nil {
				t.Fatal(err)
			}
		}
	}
	cleanup := f.discard(t)
	if got, err := os.ReadFile(other); err != nil || string(got) != strings.Repeat("c", 40) {
		t.Fatalf("the new link lost its bytes: %q %v", got, err)
	}
	if !strings.HasPrefix(getString(cleanup, "truncate"), truncateSkipped) {
		t.Fatalf("the skip must be reported: %v", cleanup)
	}
}

func TestDiscardEmptyFileIsJustUnlinked(t *testing.T) {
	f := newTruncateFixture(t, true)
	writeCandidateFile(t, f.candidate, "")
	writeCandidateFile(t, f.partial, "")
	cleanup := f.discard(t)
	if _, ok := cleanup["truncate"]; ok || getString(cleanup, "outcome") != "removed" {
		t.Fatalf("empty files need no emptying: %v", cleanup)
	}
}

// A removal that stops after the file was emptied leaves an empty file under
// the same name; the next approval finishes it.
func TestDiscardFinishesWhenAnEarlierRunStoppedAfterEmptyingTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newTruncateFixture(t, true)
	dir := filepath.Dir(f.candidate)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := f.e.DiscardLeftoverCandidate(context.Background(), "attempt"); err == nil {
		t.Fatal("unlink should have failed in a read-only directory")
	}
	cleanup := candidateCleanupOf(t, f.e, mustInstance(t, f.st, "attempt"))
	if getString(cleanup, "outcome") != "kept" || getString(cleanup, "truncate") != truncateEmptied || getInt(cleanup, "bytes_emptied") != 40 {
		t.Fatalf("a kept file that was already emptied must say so: %v", cleanup)
	}
	if detail := mustInstance(t, f.st, "attempt").WaitingReason; !strings.Contains(detail, "emptied but kept") {
		t.Fatalf("the visible note must say the file was emptied: %q", detail)
	}
	if info, err := os.Stat(f.candidate); err != nil || info.Size() != 0 {
		t.Fatalf("expected an emptied candidate to remain: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cleanup = f.discard(t)
	if getString(cleanup, "outcome") != "removed" {
		t.Fatalf("retry did not finish: %v", cleanup)
	}
	if _, err := os.Lstat(f.candidate); !os.IsNotExist(err) {
		t.Fatalf("empty candidate kept: %v", err)
	}
}

func TestRemoveDisposableMissingFileIsAlreadyRemoved(t *testing.T) {
	f := newTruncateFixture(t, true)
	info, err := os.Lstat(f.candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.candidate); err != nil {
		t.Fatal(err)
	}
	if note, err := f.e.removeDisposable(f.candidate, info); err != nil || !strings.HasPrefix(note, truncateSkipped) {
		t.Fatalf("a vanished file: %q %v", note, err)
	}
}
