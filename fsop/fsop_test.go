package fsop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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
	if err := r.Move(a, b); err != nil {
		t.Fatalf("move: %v", err)
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
	if err := r.Move(a, b); err == nil {
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
