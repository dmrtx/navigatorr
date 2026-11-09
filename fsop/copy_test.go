package fsop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noTempLeft fails when a hidden copy was left behind under dir.
func noTempLeft(t *testing.T, dir string) {
	t.Helper()
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), ".nav-mv-") {
			t.Errorf("temporary copy left behind: %s", p)
		}
		return nil
	})
}

func copyFixture(t *testing.T) (*Resolver, string, string, string) {
	t.Helper()
	r, root, _ := testResolver(t)
	src := filepath.Join(root, "library", "Film (2001)", "Film.mkv")
	os.MkdirAll(filepath.Dir(src), 0o755)
	if err := os.WriteFile(src, []byte("original film bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	os.Chtimes(src, old, old)
	return r, root, src, filepath.Join(root, "backup", "2026-11", "Film.mkv")
}

func TestCopyWritesVerifiedCopyAndReportsBothHashes(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	res, err := r.Copy(context.Background(), src, dst, "")
	if err != nil {
		t.Fatal(err)
	}
	want := sha("original film bytes")
	if res.Status != CopyCopied || res.SourceSHA256 != want || res.DestinationSHA256 != want || res.Bytes != int64(len("original film bytes")) {
		t.Errorf("result = %+v", res)
	}
	if res.Source != src || res.Destination != dst {
		t.Errorf("paths = %s -> %s", res.Source, res.Destination)
	}
	if got := readFile(t, dst); got != "original film bytes" {
		t.Errorf("destination = %q", got)
	}
	if got := readFile(t, src); got != "original film bytes" {
		t.Errorf("source changed: %q", got)
	}
	if len(res.CreatedDirs) != 2 || res.CreatedDirs[0] != filepath.Join(root, "backup") {
		t.Errorf("created dirs = %v", res.CreatedDirs)
	}
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if !di.ModTime().Equal(si.ModTime()) || di.Mode().Perm() != si.Mode().Perm() {
		t.Errorf("destination time/mode %v %v, source %v %v", di.ModTime(), di.Mode(), si.ModTime(), si.Mode())
	}
	noTempLeft(t, root)
}

func TestCopyIsIdempotent(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	if _, err := r.Copy(context.Background(), src, dst, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(dst)
	res, err := r.Copy(context.Background(), src, dst, "")
	if err != nil {
		t.Fatal(err)
	}
	want := sha("original film bytes")
	if res.Status != CopyAlreadyPresent || res.SourceSHA256 != want || res.DestinationSHA256 != want || len(res.CreatedDirs) != 0 {
		t.Errorf("second copy = %+v", res)
	}
	after, _ := os.Stat(dst)
	if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Error("an identical destination was rewritten")
	}
	noTempLeft(t, root)
}

func TestCopyNeverOverwritesWithoutTheConfirmedHash(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("an older backup"), 0o644)

	_, err := r.Copy(context.Background(), src, dst, "")
	var differs *DestinationDiffersError
	if !errors.As(err, &differs) {
		t.Fatalf("err = %v, want DestinationDiffersError", err)
	}
	if differs.SourceSHA256 != sha("original film bytes") || differs.DestinationSHA256 != sha("an older backup") {
		t.Errorf("differs = %+v", differs)
	}
	if got := readFile(t, dst); got != "an older backup" {
		t.Errorf("destination overwritten without confirmation: %q", got)
	}

	// A hash that is not the destination's is refused.
	if _, err := r.Copy(context.Background(), src, dst, sha("something else")); err == nil || !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("wrong confirmation: %v", err)
	}
	if got := readFile(t, dst); got != "an older backup" {
		t.Errorf("destination overwritten with a wrong confirmation: %q", got)
	}

	res, err := r.Copy(context.Background(), src, dst, differs.DestinationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != CopyReplaced || res.ReplacedSHA256 != sha("an older backup") || res.DestinationSHA256 != sha("original film bytes") {
		t.Errorf("replace = %+v", res)
	}
	if got := readFile(t, dst); got != "original film bytes" {
		t.Errorf("destination after replace = %q", got)
	}
	noTempLeft(t, root)
}

func TestCopyReplaceRefusesADestinationThatChanged(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("an older backup"), 0o644)
	afterVerifyHook = func() {
		os.WriteFile(dst, []byte("someone else wrote this"), 0o644)
	}
	defer func() { afterVerifyHook = nil }()

	_, err := r.Copy(context.Background(), src, dst, sha("an older backup"))
	if err == nil || !strings.Contains(err.Error(), "changed while the copy was made") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, dst); got != "someone else wrote this" {
		t.Errorf("a changed destination was replaced: %q", got)
	}
	noTempLeft(t, root)
}

func TestCopyReplaceNeedsAnExistingDestination(t *testing.T) {
	r, _, src, dst := copyFixture(t)
	if _, err := r.Copy(context.Background(), src, dst, sha("x")); err == nil || !strings.Contains(err.Error(), "nothing to replace") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("destination was written")
	}
}

func TestCopyRefusesASourceThatChangesDuringVerification(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	afterVerifyHook = func() { os.WriteFile(src, []byte("original film bytes, edited"), 0o640) }
	defer func() { afterVerifyHook = nil }()
	if _, err := r.Copy(context.Background(), src, dst, ""); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a copy of a changing source was put in place")
	}
	if _, err := os.Stat(filepath.Join(root, "backup")); !os.IsNotExist(err) {
		t.Error("the folders made for a failed copy were kept")
	}
	noTempLeft(t, root)
}

func TestCopyRefusesUnsafePaths(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	_, _, outside := testResolver(t)
	outsideFile := filepath.Join(outside, "x.mkv")
	os.WriteFile(outsideFile, []byte("x"), 0o644)
	link := filepath.Join(root, "library", "Film (2001)", "Film.link.mkv")
	if err := os.Link(src, link); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ src, dst, want string }{
		"source outside the read roots":       {outsideFile, dst, "source"},
		"destination outside the write roots": {src, filepath.Join(outside, "y.mkv"), "destination"},
		"source is a folder":                  {filepath.Dir(src), dst, "not a regular file"},
		"same file":                           {src, src, "same file"},
		"hard link of the source":             {src, link, "hard link"},
		"destination is a folder":             {src, filepath.Dir(src), "not a regular file"},
		"destination is the root":             {src, root, "allowed root"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Copy(context.Background(), tc.src, tc.dst, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if got := readFile(t, src); got != "original film bytes" {
		t.Errorf("source changed: %q", got)
	}
}

func TestCopyRefusesWhenTheDestinationHasNoRoom(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	spaceFn = func(string) (Space, error) { return Space{Avail: 3}, nil }
	defer func() { spaceFn = SpaceOf }()
	if _, err := r.Copy(context.Background(), src, dst, ""); err == nil || !strings.Contains(err.Error(), "not enough space") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "backup")); !os.IsNotExist(err) {
		t.Error("folders were left for a copy that did not fit")
	}
}

func TestCopyReplaceRefusesADestinationSwappedForAnEqualLookingFile(t *testing.T) {
	for name, change := range map[string]func(dst string, at time.Time){
		// Another file of the same size and time put in its place.
		"swapped": func(dst string, at time.Time) {
			other := dst + ".other"
			os.WriteFile(other, []byte("an olden backup"), 0o644)
			os.Chtimes(other, at, at)
			os.Rename(other, dst)
		},
		// The same file rewritten in place, its size and time restored.
		"rewritten": func(dst string, at time.Time) {
			os.WriteFile(dst, []byte("an olden backup"), 0o644)
			os.Chtimes(dst, at, at)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, root, src, dst := copyFixture(t)
			os.MkdirAll(filepath.Dir(dst), 0o755)
			os.WriteFile(dst, []byte("an older backup"), 0o644)
			at := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			os.Chtimes(dst, at, at)
			afterVerifyHook = func() { change(dst, at) }
			defer func() { afterVerifyHook = nil }()

			_, err := r.Copy(context.Background(), src, dst, sha("an older backup"))
			if err == nil || !strings.Contains(err.Error(), "nothing was replaced") {
				t.Fatalf("err = %v", err)
			}
			if got := readFile(t, dst); got != "an olden backup" {
				t.Errorf("an unconfirmed destination was replaced: %q", got)
			}
			noTempLeft(t, root)
		})
	}
}

func TestCopyAlreadyPresentRefusesASourceThatChanged(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	if _, err := r.Copy(context.Background(), src, dst, ""); err != nil {
		t.Fatal(err)
	}
	afterCompareHook = func() { os.WriteFile(src, []byte("original film bytes, edited"), 0o640) }
	defer func() { afterCompareHook = nil }()
	if _, err := r.Copy(context.Background(), src, dst, ""); err == nil || !strings.Contains(err.Error(), "source") || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v, want a changed source refused", err)
	}
	if got := readFile(t, dst); got != "original film bytes" {
		t.Errorf("destination = %q", got)
	}
	noTempLeft(t, root)
}

func TestCopyAlreadyPresentRefusesADestinationThatChanged(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	if _, err := r.Copy(context.Background(), src, dst, ""); err != nil {
		t.Fatal(err)
	}
	afterCompareHook = func() { os.WriteFile(dst, []byte("someone else wrote this"), 0o644) }
	defer func() { afterCompareHook = nil }()
	res, err := r.Copy(context.Background(), src, dst, "")
	if err == nil || !strings.Contains(err.Error(), "destination") || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("err = %v, status %q; want a changed destination refused", err, res.Status)
	}
	noTempLeft(t, root)
}

func TestCopyReplaceRefusesASourceThatChangesWhileTheDestinationIsChecked(t *testing.T) {
	r, root, src, dst := copyFixture(t)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, []byte("an older backup"), 0o644)
	afterConfirmHook = func() { os.WriteFile(src, []byte("original film bytes, edited"), 0o640) }
	defer func() { afterConfirmHook = nil }()
	if _, err := r.Copy(context.Background(), src, dst, sha("an older backup")); err == nil || !strings.Contains(err.Error(), "nothing was replaced") {
		t.Fatalf("err = %v", err)
	}
	if got := readFile(t, dst); got != "an older backup" {
		t.Errorf("destination = %q, want it untouched", got)
	}
	noTempLeft(t, root)
}
