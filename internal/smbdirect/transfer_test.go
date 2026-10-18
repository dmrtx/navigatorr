package smbdirect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hirochachacha/go-smb2"
)

func timedShare(files map[string][]byte) *memoryShare {
	fs := &memoryShare{files: files, times: map[string]time.Time{}}
	for name := range files {
		fs.touch(name)
	}
	return fs
}

func expectFor(b []byte) PublishExpect {
	sum := sha256.Sum256(b)
	return PublishExpect{SizeBytes: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
}

func writeCandidate(t *testing.T, payload []byte) string {
	t.Helper()
	local := filepath.Join(t.TempDir(), "candidate.mkv")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return local
}

// One publish moves M up and reads M back exactly once; the final is never
// re-read after the rename.
func TestPublishVerifiedSingleReadBackNoPostRenameHash(t *testing.T) {
	root := t.TempDir()
	fs := timedShare(map[string][]byte{})
	s := testStore(fs, root)
	payload := bytes.Repeat([]byte("encoded"), 4096)
	tr, err := s.PublishVerified(context.Background(), writeCandidate(t, payload), filepath.Join(root, "TV", "out.mkv"), "job-1", expectFor(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fs.files["TV/out.mkv"], payload) {
		t.Fatal("final does not hold the candidate")
	}
	if tr.WrittenBytes != int64(len(payload)) || tr.ReadBytes != int64(len(payload)) || tr.Attempts != 1 {
		t.Fatalf("transfer = %+v, want one upload and one read-back of %d bytes", tr, len(payload))
	}
	if fs.opens["TV/out.mkv.partial.job-1"] != 1 || fs.opens["TV/out.mkv"] != 0 {
		t.Fatalf("opens = %v, want one partial read-back and no final read", fs.opens)
	}
}

// The upload hashes what it sends, so a candidate that changed since
// validation is refused before rename even though no local pre-hash ran.
func TestPublishVerifiedRejectsChangedLocalCandidate(t *testing.T) {
	root := t.TempDir()
	fs := timedShare(map[string][]byte{})
	s := testStore(fs, root)
	accepted := []byte("accepted candidate")
	changed := []byte("replaced candidate")
	_, err := s.PublishVerified(context.Background(), writeCandidate(t, changed), filepath.Join(root, "out.mkv"), "job-1", expectFor(accepted), nil)
	if err == nil || len(fs.files) != 0 {
		t.Fatalf("err=%v files=%v, want refusal with nothing left on the share", err, fs.files)
	}
}

// A transport failure on rename keeps the verified partial; the recovery
// session renames it without uploading or reading it again.
func TestPublishVerifiedReconnectReusesVerifiedPartial(t *testing.T) {
	root := t.TempDir()
	fs := timedShare(map[string][]byte{})
	s := testStore(fs, root)
	payload := bytes.Repeat([]byte("x"), 100_000)
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		if connections == 1 {
			return fn(faultShare{share: fs, rename: func(string, string) error {
				return &smb2.TransportError{Err: syscall.ECONNRESET}
			}})
		}
		return fn(fs)
	}
	tr, err := s.PublishVerified(context.Background(), writeCandidate(t, payload), filepath.Join(root, "out.mkv"), "job-1", expectFor(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if connections != 2 || !tr.ReusedPartial || tr.WrittenBytes != int64(len(payload)) || tr.ReadBytes != int64(len(payload)) {
		t.Fatalf("connections=%d transfer=%+v, want reuse with one upload and one read-back", connections, tr)
	}
	if !bytes.Equal(fs.files["out.mkv"], payload) || len(fs.files) != 1 {
		t.Fatalf("files=%v", fs.files)
	}
}

// A rename the server committed before the reply was lost is recognised by
// the verified partial's identity, so the final is not read back.
func TestPublishVerifiedCommittedRenameProvedByIdentity(t *testing.T) {
	root := t.TempDir()
	fs := timedShare(map[string][]byte{})
	s := testStore(fs, root)
	payload := []byte("verified candidate")
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		return fn(faultShare{share: fs, rename: func(from, to string) error {
			if err := fs.RenameNoReplace(from, to); err != nil {
				return err
			}
			return &smb2.TransportError{Err: syscall.ECONNRESET}
		}})
	}
	tr, err := s.PublishVerified(context.Background(), writeCandidate(t, payload), filepath.Join(root, "out.mkv"), "job-1", expectFor(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if connections != 2 || fs.opens["out.mkv"] != 0 || tr.ReadBytes != int64(len(payload)) {
		t.Fatalf("connections=%d opens=%v transfer=%+v", connections, fs.opens, tr)
	}
}

// A later finalization retry (new process) finds the complete partial an
// earlier run uploaded and only proves it with one read-back.
func TestPublishVerifiedResumesFromCompletePartial(t *testing.T) {
	root := t.TempDir()
	payload := []byte("complete upload from an earlier run")
	fs := timedShare(map[string][]byte{"out.mkv.partial.job-1": append([]byte(nil), payload...)})
	s := testStore(fs, root)
	tr, err := s.PublishVerified(context.Background(), writeCandidate(t, payload), filepath.Join(root, "out.mkv"), "job-1", expectFor(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.ReusedPartial || tr.WrittenBytes != 0 || tr.ReadBytes != int64(len(payload)) {
		t.Fatalf("transfer=%+v, want no upload and one read-back", tr)
	}
	if !bytes.Equal(fs.files["out.mkv"], payload) {
		t.Fatal("final not published")
	}
}

// A same-size partial with the wrong bytes is never trusted.
func TestPublishVerifiedReplacesCorruptPartial(t *testing.T) {
	root := t.TempDir()
	payload := []byte("good bytes")
	fs := timedShare(map[string][]byte{"out.mkv.partial.job-1": []byte("evil bytes")})
	s := testStore(fs, root)
	tr, err := s.PublishVerified(context.Background(), writeCandidate(t, payload), filepath.Join(root, "out.mkv"), "job-1", expectFor(payload), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.ReusedPartial || !bytes.Equal(fs.files["out.mkv"], payload) {
		t.Fatalf("transfer=%+v final=%q", tr, fs.files["out.mkv"])
	}
}

// A different-size final is a conflict proved by stat alone.
func TestPublishVerifiedSizeConflictNeedsNoRead(t *testing.T) {
	root := t.TempDir()
	fs := timedShare(map[string][]byte{"out.mkv": []byte("someone else's longer file")})
	s := testStore(fs, root)
	payload := []byte("candidate")
	tr, err := s.PublishVerified(context.Background(), writeCandidate(t, payload), filepath.Join(root, "out.mkv"), "job-1", expectFor(payload), nil)
	if !errors.Is(err, ErrDestinationExists) || tr.ReadBytes != 0 || string(fs.files["out.mkv"]) != "someone else's longer file" {
		t.Fatalf("err=%v transfer=%+v", err, tr)
	}
}

// cutReader fails with a transport error after limit bytes.
type cutReader struct {
	*bytes.Reader
	left int
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, &smb2.TransportError{Err: syscall.ECONNRESET}
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.Reader.Read(p)
	c.left -= n
	return n, err
}

func (*cutReader) Close() error { return nil }

func downloadWithCut(t *testing.T, expected string) (Transfer, []byte, []byte) {
	t.Helper()
	root := t.TempDir()
	content := bytes.Repeat([]byte("0123456789abcdef"), 200_000) // 3.2 MB
	fs := timedShare(map[string][]byte{"source.mkv": content})
	s := testStore(fs, root)
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		if connections == 1 {
			return fn(faultShare{share: fs, open: func(string) (readFile, error) {
				return &cutReader{Reader: bytes.NewReader(content), left: 2_000_000}, nil
			}})
		}
		return fn(fs)
	}
	dst := filepath.Join(t.TempDir(), "input.mkv")
	tr, err := s.DownloadAtomicVerified(context.Background(), filepath.Join(root, "source.mkv"), dst, expected)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return tr, got, content
}

// With a digest to verify against, a reconnect continues from the last
// written offset instead of re-reading all of N.
func TestDownloadResumesFromVerifiedOffset(t *testing.T) {
	sum := sha256.Sum256(bytes.Repeat([]byte("0123456789abcdef"), 200_000))
	tr, got, content := downloadWithCut(t, hex.EncodeToString(sum[:]))
	if !bytes.Equal(got, content) {
		t.Fatal("resumed download is not byte-identical")
	}
	if !tr.Resumed || tr.Attempts != 2 || tr.ReadBytes != int64(len(content)) {
		t.Fatalf("transfer=%+v, want one resumed pass reading N=%d once", tr, len(content))
	}
	if tr.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("streamed digest %s does not cover both sessions", tr.SHA256)
	}
}

// Without a digest the caller cannot prove a splice, so the download starts
// over exactly as before and the retry is visible in the transfer.
func TestDownloadWithoutDigestRestartsFromZero(t *testing.T) {
	tr, got, content := downloadWithCut(t, "")
	if !bytes.Equal(got, content) || tr.Resumed || tr.Attempts != 2 || tr.ReadBytes != int64(len(content))+2_000_000 {
		t.Fatalf("transfer=%+v", tr)
	}
}

// A source that changed between sessions is read again from byte zero.
func TestDownloadResumeRefusedWhenSourceChanged(t *testing.T) {
	root := t.TempDir()
	first := bytes.Repeat([]byte("a"), 300_000)
	second := bytes.Repeat([]byte("b"), 300_000)
	fs := timedShare(map[string][]byte{"source.mkv": first})
	s := testStore(fs, root)
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		if connections == 1 {
			return fn(faultShare{share: fs, open: func(string) (readFile, error) {
				return &cutReader{Reader: bytes.NewReader(first), left: 100_000}, nil
			}})
		}
		fs.files["source.mkv"] = second
		fs.touch("source.mkv")
		return fn(fs)
	}
	sum := sha256.Sum256(second)
	dst := filepath.Join(t.TempDir(), "input.mkv")
	tr, err := s.DownloadAtomicVerified(context.Background(), filepath.Join(root, "source.mkv"), dst, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if tr.Resumed || !bytes.Equal(got, second) || tr.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("transfer=%+v len=%d", tr, len(got))
	}
}

var _ io.Seeker = memoryReader{}
