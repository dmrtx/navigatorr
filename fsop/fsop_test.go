package fsop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testResolver(t *testing.T) (*Resolver, string, string) {
	t.Helper()
	root := t.TempDir()
	outside := t.TempDir()
	r, err := NewResolver([]string{root}, []string{root})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	_ = outside
	return r, root, outside
}

func TestOutsideRootsRefused(t *testing.T) {
	r, _, outside := testResolver(t)
	evil := filepath.Join(outside, "secret.txt")
	os.WriteFile(evil, []byte("x"), 0o644)
	if _, err := r.FileStat(evil); err == nil {
		t.Error("read outside roots was allowed")
	}
	if _, err := r.ResolveWrite(evil); err == nil {
		t.Error("write outside roots was allowed")
	}
}

func mustRoot(t *testing.T, r *Resolver) string {
	t.Helper()
	if len(r.readRoots) == 0 {
		t.Fatal("no roots")
	}
	return r.readRoots[0]
}

func TestSymlinkEscapeRefused(t *testing.T) {
	r, root, outside := testResolver(t)
	secret := filepath.Join(outside, "secret.txt")
	os.WriteFile(secret, []byte("s"), 0o644)
	link := filepath.Join(root, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := r.FileStat(link); err == nil {
		t.Error("symlink escape was allowed")
	}
}

func TestMoveDeleteInsideRoots(t *testing.T) {
	r, root, _ := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	b := filepath.Join(root, "sub", "b.mkv")
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(a, []byte("data"), 0o644)
	if res, err := r.Move(context.Background(), a, b); err != nil || res.Method != "rename" {
		t.Fatalf("move: %v %+v", err, res)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if err := r.Delete(b); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Directories are never deleted.
	if err := r.Delete(filepath.Join(root, "sub")); err == nil {
		t.Error("directory delete was allowed")
	}
	// No silent overwrite.
	os.WriteFile(a, []byte("1"), 0o644)
	os.WriteFile(b, []byte("2"), 0o644)
	if _, err := r.Move(context.Background(), a, b); err == nil {
		t.Error("overwrite was allowed")
	}
}

func TestHashAndCancel(t *testing.T) {
	r, root, _ := testResolver(t)
	p := filepath.Join(root, "a.mkv")
	os.WriteFile(p, []byte("data"), 0o644)
	h, n, err := r.Hash(context.Background(), p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if n != 4 || h == "" {
		t.Errorf("hash=%q size=%d", h, n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Hash(ctx, p); err == nil {
		t.Error("cancelled hash was not interrupted")
	}
}

func TestListBounded(t *testing.T) {
	r, root, _ := testResolver(t)
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(root, string(rune('a'+i))+".mkv"), []byte("x"), 0o644)
	}
	list, err := r.List(root, 1, 5)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 5 {
		t.Errorf("got %d entries, want 5", len(list))
	}
}

// crossDevice makes every rename fail like two separate mounts would.
func crossDevice(t *testing.T) {
	t.Helper()
	old := renameFn
	renameFn = func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFn = old })
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.Contains(fi.Name(), ".nav-mv-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestMoveCreatesMissingFolders(t *testing.T) {
	r, root, _ := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("data"), 0o644)
	b := filepath.Join(root, "Show", "Season 01", "a.mkv")
	res, err := r.Move(context.Background(), a, b)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if len(res.CreatedDirs) != 2 {
		t.Errorf("created dirs = %v", res.CreatedDirs)
	}
	if _, err := os.Stat(b); err != nil {
		t.Errorf("moved file missing: %v", err)
	}
}

func TestMoveAcrossFilesystemsCopiesVerifiesThenRemoves(t *testing.T) {
	crossDevice(t)
	r, root, _ := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("some video bytes"), 0o640)
	when := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	os.Chtimes(a, when, when)
	b := filepath.Join(root, "Anime", "One Pace", "a.mkv")
	res, err := r.Move(context.Background(), a, b)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if res.Method != "copy" || res.Bytes != 16 || len(res.SHA256) != 64 {
		t.Errorf("result = %+v", res)
	}
	got, _ := os.ReadFile(b)
	if string(got) != "some video bytes" {
		t.Errorf("content = %q", got)
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Error("source still exists after a verified copy")
	}
	fi, _ := os.Stat(b)
	if !fi.ModTime().Equal(when) || fi.Mode().Perm() != 0o640 {
		t.Errorf("mtime/mode not kept: %v %v", fi.ModTime(), fi.Mode())
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestMoveAcrossFilesystemsNeverOverwrites(t *testing.T) {
	crossDevice(t)
	r, root, _ := testResolver(t)
	a, b := filepath.Join(root, "a.mkv"), filepath.Join(root, "b.mkv")
	os.WriteFile(a, []byte("1"), 0o644)
	os.WriteFile(b, []byte("2"), 0o644)
	if _, err := r.Move(context.Background(), a, b); err == nil {
		t.Fatal("overwrite was allowed")
	}
	if got, _ := os.ReadFile(b); string(got) != "2" {
		t.Errorf("destination changed: %q", got)
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("source was removed")
	}
}

func TestMoveAcrossFilesystemsCancelledKeepsSource(t *testing.T) {
	crossDevice(t)
	r, root, _ := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("data"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := filepath.Join(root, "x", "a.mkv")
	if _, err := r.Move(ctx, a, b); err == nil {
		t.Fatal("cancelled move succeeded")
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("source lost")
	}
	if _, err := os.Stat(b); err == nil {
		t.Error("partial destination left")
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestMoveAcrossFilesystemsRefusesDirectories(t *testing.T) {
	crossDevice(t)
	r, root, _ := testResolver(t)
	d := filepath.Join(root, "dir")
	os.MkdirAll(d, 0o755)
	_, err := r.Move(context.Background(), d, filepath.Join(root, "other"))
	if err == nil || !strings.Contains(err.Error(), "only files") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Error("directory lost")
	}
}

func TestMoveOtherRenameErrorsPassThrough(t *testing.T) {
	old := renameFn
	renameFn = func(from, to string) error { return syscall.EACCES }
	defer func() { renameFn = old }()
	r, root, _ := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("x"), 0o644)
	_, err := r.Move(context.Background(), a, filepath.Join(root, "b.mkv"))
	if !errors.Is(err, syscall.EACCES) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("source lost")
	}
}

func TestMoveOutsideRootsStillRefused(t *testing.T) {
	r, root, outside := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("x"), 0o644)
	if _, err := r.Move(context.Background(), a, filepath.Join(outside, "a.mkv")); err == nil {
		t.Error("move out of the roots was allowed")
	}
	if _, err := r.Move(context.Background(), a, root); err == nil {
		t.Error("move onto a root was allowed")
	}
}

func TestMoveKeepsVerifiedCopyWhenSourceCannotBeRemoved(t *testing.T) {
	crossDevice(t)
	old := removeSourceFn
	removeSourceFn = func(string) error { return syscall.EIO }
	defer func() { removeSourceFn = old }()
	r, root, _ := testResolver(t)
	a, b := filepath.Join(root, "a.mkv"), filepath.Join(root, "Anime", "a.mkv")
	os.WriteFile(a, []byte("data"), 0o644)
	res, err := r.Move(context.Background(), a, b)
	if err == nil || !strings.Contains(err.Error(), "both files exist") {
		t.Fatalf("err = %v", err)
	}
	if res.Method != "copy_source_kept" || len(res.SHA256) != 64 || len(res.CreatedDirs) != 1 {
		t.Errorf("result = %+v", res)
	}
	if got, _ := os.ReadFile(b); string(got) != "data" {
		t.Errorf("verified copy lost: %q", got)
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("source lost")
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestMoveSourceChangedWhileVerifying(t *testing.T) {
	crossDevice(t)
	r, root, _ := testResolver(t)
	a, b := filepath.Join(root, "a.mkv"), filepath.Join(root, "Anime", "a.mkv")
	os.WriteFile(a, []byte("data"), 0o644)
	afterVerifyHook = func() { os.WriteFile(a, []byte("data and more"), 0o644) }
	defer func() { afterVerifyHook = nil }()
	if _, err := r.Move(context.Background(), a, b); err == nil {
		t.Fatal("moved a source that changed")
	}
	if got, _ := os.ReadFile(a); string(got) != "data and more" {
		t.Errorf("source touched: %q", got)
	}
	if _, err := os.Stat(b); err == nil {
		t.Error("stale copy left at the destination")
	}
	if _, err := os.Stat(filepath.Dir(b)); err == nil {
		t.Error("created folder left behind")
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestMoveRefusesRootThatLostItsMount(t *testing.T) {
	mounted := true
	old := mountPointFn
	mountPointFn = func(string) (bool, error) { return mounted, nil }
	defer func() { mountPointFn = old }()
	r, root, _ := testResolver(t) // the root is a mount point here
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("x"), 0o644)
	// A remount (new device number) is still a mount: fine.
	if _, err := r.Move(context.Background(), a, filepath.Join(root, "Ok", "a.mkv")); err != nil {
		t.Fatalf("move on a mounted root: %v", err)
	}
	// The share goes away: the root is an empty folder on the local disk.
	mounted = false
	b := filepath.Join(root, "Ok", "a.mkv")
	if _, err := r.Move(context.Background(), b, filepath.Join(root, "New", "a.mkv")); err == nil {
		t.Fatal("moved into an unmounted root")
	}
	if _, err := os.Stat(filepath.Join(root, "New")); err == nil {
		t.Error("folder created in an unmounted root")
	}
	if _, err := os.Stat(b); err != nil {
		t.Error("source lost")
	}
}

func TestMoveNeverCreatesAMissingRoot(t *testing.T) {
	root, gone := t.TempDir(), filepath.Join(t.TempDir(), "nas")
	r, err := NewResolver([]string{root}, []string{root, gone})
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("x"), 0o644)
	if _, err := r.Move(context.Background(), a, filepath.Join(gone, "Show", "S01", "a.mkv")); err == nil {
		t.Fatal("moved into a root that is not there")
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("the missing root was created")
	}
	if _, err := os.Stat(a); err != nil {
		t.Error("source lost")
	}
}

func TestRenameCheckedNeverReplaces(t *testing.T) {
	_, root, _ := testResolver(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.WriteFile(a, []byte("new"), 0o644)
	os.WriteFile(b, []byte("old"), 0o644)
	if supported, _ := renameNoReplace(a, b); !supported {
		t.Skip("no rename-without-replace here")
	}
	if err := renameChecked(a, b); err == nil {
		t.Error("replaced an existing file")
	}
	if got, _ := os.ReadFile(b); string(got) != "old" {
		t.Errorf("destination = %q", got)
	}
}

func TestMoveIntoItselfRefused(t *testing.T) {
	r, root, _ := testResolver(t)
	d := filepath.Join(root, "dir")
	os.MkdirAll(d, 0o755)
	if _, err := r.Move(context.Background(), d, filepath.Join(d, "inner", "dir")); err == nil {
		t.Error("moved a folder into itself")
	}
	if _, err := os.Stat(filepath.Join(d, "inner")); err == nil {
		t.Error("folder created before refusing")
	}
}

func TestPlaceNeverReplaces(t *testing.T) {
	r, root, _ := testResolver(t)
	tmp, dst := filepath.Join(root, "t"), filepath.Join(root, "d")
	os.WriteFile(tmp, []byte("new"), 0o644)
	os.WriteFile(dst, []byte("old"), 0o644)
	if err := r.place(tmp, dst); err == nil {
		t.Fatal("replaced an existing file")
	}
	if got, _ := os.ReadFile(dst); string(got) != "old" {
		t.Errorf("destination = %q", got)
	}
}

func TestPlaceWithoutHardLinks(t *testing.T) {
	old := linkFn
	linkFn = func(string, string) error { return syscall.EPERM }
	defer func() { linkFn = old }()
	r, root, _ := testResolver(t)
	tmp, dst := filepath.Join(root, "t"), filepath.Join(root, "d")
	os.WriteFile(tmp, []byte("new"), 0o644)
	if err := r.place(tmp, dst); err != nil {
		t.Fatalf("place: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "new" {
		t.Errorf("destination = %q", got)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Error("temporary file still there")
	}
	// And it still refuses to replace when links are unavailable.
	os.WriteFile(tmp, []byte("newer"), 0o644)
	if err := r.place(tmp, dst); err == nil {
		t.Error("replaced without hard links")
	}
}

func TestMoveFailedCopyRemovesFoldersItMade(t *testing.T) {
	crossDevice(t)
	r, root, _ := testResolver(t)
	a := filepath.Join(root, "a.mkv")
	os.WriteFile(a, []byte("data"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Move(ctx, a, filepath.Join(root, "x", "y", "a.mkv")); err == nil {
		t.Fatal("cancelled move succeeded")
	}
	if _, err := os.Stat(filepath.Join(root, "x")); err == nil {
		t.Error("folders left behind")
	}
}
