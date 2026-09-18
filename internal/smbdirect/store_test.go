package smbdirect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memoryShare struct {
	files map[string][]byte
	dirs  map[string]bool
}

type memoryInfo struct {
	name string
	size int64
	dir  bool
}

func (i memoryInfo) Name() string { return i.name }
func (i memoryInfo) Size() int64  { return i.size }
func (i memoryInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o755
	}
	return 0o600
}
func (i memoryInfo) ModTime() time.Time { return time.Time{} }
func (i memoryInfo) IsDir() bool        { return i.dir }
func (i memoryInfo) Sys() any           { return nil }

type memoryReader struct{ *bytes.Reader }

func (memoryReader) Close() error { return nil }

type memoryWriter struct {
	bytes.Buffer
	fs     *memoryShare
	name   string
	closed bool
}

func (w *memoryWriter) Sync() error { return nil }
func (w *memoryWriter) Close() error {
	if !w.closed {
		w.fs.files[w.name] = append([]byte(nil), w.Bytes()...)
		w.closed = true
	}
	return nil
}

func (m *memoryShare) Open(name string) (readFile, error) {
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return memoryReader{bytes.NewReader(append([]byte(nil), b...))}, nil
}

func (m *memoryShare) OpenExclusive(name string) (writeFile, error) {
	if _, ok := m.files[name]; ok {
		return nil, os.ErrExist
	}
	// Reserve immediately so a second exclusive open cannot race it.
	m.files[name] = nil
	return &memoryWriter{fs: m, name: name}, nil
}

func (m *memoryShare) Stat(name string) (os.FileInfo, error) {
	if name == "." {
		return memoryInfo{name: ".", dir: true}, nil
	}
	if m.dirs[name] {
		return memoryInfo{name: filepath.Base(name), dir: true}, nil
	}
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return memoryInfo{name: filepath.Base(name), size: int64(len(b))}, nil
}

func (m *memoryShare) Mkdir(name string, _ os.FileMode) error {
	if m.dirs == nil {
		m.dirs = make(map[string]bool)
	}
	if m.dirs[name] {
		return os.ErrExist
	}
	m.dirs[name] = true
	return nil
}

func (m *memoryShare) RenameNoReplace(oldName, newName string) error {
	if _, ok := m.files[newName]; ok {
		return os.ErrExist
	}
	b, ok := m.files[oldName]
	if !ok {
		return os.ErrNotExist
	}
	m.files[newName] = b
	delete(m.files, oldName)
	return nil
}

func (m *memoryShare) Remove(name string) error {
	if _, ok := m.files[name]; !ok {
		return os.ErrNotExist
	}
	delete(m.files, name)
	return nil
}

func testStore(fs *memoryShare, root string) *Store {
	s := New(Config{Enabled: true, LocalRoot: root, Timeout: time.Minute})
	s.connect = func(_ context.Context, fn func(share) error) error { return fn(fs) }
	return s
}

func TestConfigNormalizeAndRemotePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	cfg := Config{
		Enabled: true, LocalRoot: root, Server: "nas.local", Share: "media",
		Username: "worker", PasswordFile: filepath.Join(t.TempDir(), "secret"), TimeoutText: "2m",
	}
	if err := cfg.Normalize([]string{root}); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if cfg.Server != "nas.local:445" || cfg.Timeout != 2*time.Minute {
		t.Fatalf("normalized config = %#v", cfg)
	}
	s := New(cfg)
	got, ok := s.RemotePath(filepath.Join(root, "TV", "episode.mkv"))
	if !ok || got != "TV/episode.mkv" {
		t.Fatalf("RemotePath = %q, %v", got, ok)
	}
	if _, ok := s.RemotePath(filepath.Join(filepath.Dir(root), "outside.mkv")); ok {
		t.Fatal("RemotePath accepted path outside local_root")
	}
}

func TestDownloadAtomic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	fs := &memoryShare{files: map[string][]byte{"TV/source.mkv": []byte("complete source")}}
	s := testStore(fs, root)
	dst := filepath.Join(t.TempDir(), "work", "input.mkv")
	if err := s.DownloadAtomic(context.Background(), filepath.Join(root, "TV", "source.mkv"), dst); err != nil {
		t.Fatalf("DownloadAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "complete source" {
		t.Fatalf("staged = %q, %v", got, err)
	}
}

func TestPublishVerifiedNoClobberAndIdempotentRetry(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	fs := &memoryShare{files: map[string][]byte{}}
	s := testStore(fs, root)
	local := filepath.Join(t.TempDir(), "candidate.mkv")
	if err := os.WriteFile(local, []byte("candidate bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "TV", "candidate.mkv")
	if err := s.Publish(context.Background(), local, destination, "job-1"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := string(fs.files["TV/candidate.mkv"]); got != "candidate bytes" {
		t.Fatalf("remote final = %q", got)
	}
	if _, exists := fs.files["TV/candidate.mkv.partial.job-1"]; exists {
		t.Fatal("partial remained after publish")
	}
	if err := s.Publish(context.Background(), local, destination, "job-1"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	fs.files["TV/candidate.mkv"] = []byte("unrelated")
	if err := s.Publish(context.Background(), local, destination, "job-1"); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("different destination error = %v", err)
	}
	if got := string(fs.files["TV/candidate.mkv"]); got != "unrelated" {
		t.Fatalf("existing destination was changed: %q", got)
	}
}

func TestPublishReplacesOnlyOwnStalePartial(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	fs := &memoryShare{files: map[string][]byte{
		"out.mkv.partial.job-1": []byte("incomplete"),
		"out.mkv.partial.other": []byte("untouched"),
	}}
	s := testStore(fs, root)
	local := filepath.Join(t.TempDir(), "candidate.mkv")
	if err := os.WriteFile(local, []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(context.Background(), local, filepath.Join(root, "out.mkv"), "job-1"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := string(fs.files["out.mkv.partial.other"]); got != "untouched" {
		t.Fatalf("unrelated partial changed: %q", got)
	}
}

func TestCopyContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyContext(ctx, io.Discard, bytes.NewReader([]byte("x"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("copyContext error = %v", err)
	}
}
