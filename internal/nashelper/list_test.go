package nashelper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func (f *fixture) list(path string) (int, ListResponse, []byte) {
	f.t.Helper()
	status, raw := f.call("POST", "/v1/list", testToken, PathRequest{Path: path})
	var resp ListResponse
	_ = json.Unmarshal(raw, &resp)
	return status, resp, raw
}

func TestListReportsEveryEntryWithItsOwnTypeSizeAndTime(t *testing.T) {
	for _, walk := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "walk"}[walk], func(t *testing.T) {
			f := newFixtureWith(t, 2, walk)
			f.tree()
			if err := os.Symlink(filepath.Join(f.root, "a.mkv"), filepath.Join(f.root, "link.mkv")); err != nil {
				t.Skip("no symlinks here")
			}
			status, resp, raw := f.list(f.root)
			if status != http.StatusOK || resp.InstanceID == "" || resp.DirMtimeNs <= 0 {
				t.Fatal(status, string(raw))
			}
			got := map[string]ListEntry{}
			for _, e := range resp.Entries {
				got[e.Name] = e
			}
			if len(got) != 3 {
				t.Fatalf("entries: %+v", resp.Entries)
			}
			if a := got["a.mkv"]; a.Type != EntryFile || a.Size != 1000 || a.MtimeNs <= 0 {
				t.Fatalf("file: %+v", a)
			}
			if d := got["show"]; d.Type != EntryDir {
				t.Fatalf("dir: %+v", d)
			}
			// A link is a link: its target is neither followed nor sized.
			if l := got["link.mkv"]; l.Type != EntrySymlink || l.Size == 1000 {
				t.Fatalf("link: %+v", l)
			}
			// Hidden names are listed; the caller decides what to show.
			_, inner, _ := f.list(filepath.Join(f.root, "show"))
			names := map[string]bool{}
			for _, e := range inner.Entries {
				names[e.Name] = true
			}
			if !names[".hidden"] || !names["s1"] || !names["empty"] || len(names) != 3 {
				t.Fatalf("inner: %+v", inner.Entries)
			}
		})
	}
}

func TestListRefusesWhatIsNotAServedDirectory(t *testing.T) {
	f := newFixture(t, 2)
	f.tree()
	outside := t.TempDir()
	for name, path := range map[string]string{
		"outside the roots": outside,
		"a file":            filepath.Join(f.root, "a.mkv"),
		"relative":          "show",
		"missing":           filepath.Join(f.root, "nothing"),
	} {
		if status, _, raw := f.list(path); status == http.StatusOK {
			t.Errorf("%s was listed: %s", name, raw)
		}
	}
	if status, _ := f.call("POST", "/v1/list", "wrong", PathRequest{Path: f.root}); status != http.StatusUnauthorized {
		t.Fatal("a listing needs the token", status)
	}
	// A link to a directory outside the roots is not entered.
	if err := os.Symlink(outside, filepath.Join(f.root, "escape")); err == nil {
		if status, _, raw := f.list(filepath.Join(f.root, "escape")); status == http.StatusOK {
			t.Fatalf("a link out of the roots was listed: %s", raw)
		}
	}
}

func TestListRefusesADirectoryTooBigForOneAnswer(t *testing.T) {
	if testing.Short() {
		t.Skip("creates thousands of files")
	}
	f := newFixture(t, 2)
	big := filepath.Join(f.root, "big")
	if err := os.Mkdir(big, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxListEntries+1; i++ {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("%05d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	status, _, raw := f.list(big)
	if status != http.StatusBadRequest {
		t.Fatalf("expected a refusal, got %d: %.200s", status, raw)
	}
	// The refusal is not an answer about the directory: a caller lists it through the mount.
	var e struct{ Error *Error }
	_ = json.Unmarshal(raw, &e)
	if e.Error == nil || IsAnswer(e.Error) || !ShouldFallBack(e.Error) {
		t.Fatalf("%+v", e.Error)
	}
}

func TestAListingCanBeCancelled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the cancellable walk is Linux's")
	}
	f := newFixture(t, 2)
	f.tree()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.srv.roots.List(ctx, f.root); err == nil {
		t.Fatal("a cancelled listing returned")
	}
}

func TestListRefusesANameJSONCannotCarry(t *testing.T) {
	f := newFixture(t, 2)
	dir := filepath.Join(f.root, "odd")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.mkv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad\xff.mkv"), nil, 0o600); err != nil {
		t.Skip("this filesystem refuses names that are not UTF-8")
	}
	status, _, raw := f.list(dir)
	var e struct{ Error *Error }
	_ = json.Unmarshal(raw, &e)
	// Not an answer about the folder: the caller lists it through the mount, where the name is real.
	if status != http.StatusBadRequest || e.Error == nil || !ShouldFallBack(e.Error) {
		t.Fatal(status, string(raw))
	}
}

func TestListsDoNotWaitBehindFolderSizeWalks(t *testing.T) {
	f := newFixture(t, 2)
	f.tree()
	for range cap(f.srv.foldSem) {
		f.srv.foldSem <- struct{}{}
	}
	defer func() {
		for range cap(f.srv.foldSem) {
			<-f.srv.foldSem
		}
	}()
	if status, resp, raw := f.list(f.root); status != http.StatusOK || len(resp.Entries) == 0 {
		t.Fatal(status, string(raw))
	}
}
