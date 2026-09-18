package smbdirect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hirochachacha/go-smb2"
)

type faultShare struct {
	share
	open   func(string) (readFile, error)
	rename func(string, string) error
}

func (s faultShare) Open(name string) (readFile, error) {
	if s.open != nil {
		return s.open(name)
	}
	return s.share.Open(name)
}

func (s faultShare) RenameNoReplace(oldName, newName string) error {
	if s.rename != nil {
		return s.rename(oldName, newName)
	}
	return s.share.RenameNoReplace(oldName, newName)
}

type failedRead struct {
	*bytes.Reader
	err error
}

func (f failedRead) Read(p []byte) (int, error) {
	n, err := f.Reader.Read(p)
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}

func (failedRead) Close() error { return nil }

func TestDownloadRecoversSignedSessionBeforeEncode(t *testing.T) {
	root := t.TempDir()
	fs := &memoryShare{files: map[string][]byte{"source.mkv": []byte("complete source")}}
	s := testStore(fs, root)
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		if connections == 1 {
			return fn(faultShare{share: fs, open: func(string) (readFile, error) {
				return nil, &smb2.InvalidResponseError{Message: "signing required"}
			}})
		}
		return fn(fs)
	}
	dst := filepath.Join(t.TempDir(), "input.mkv")
	if err := s.DownloadAtomic(context.Background(), filepath.Join(root, "source.mkv"), dst); err != nil {
		t.Fatal(err)
	}
	if connections != 2 {
		t.Fatalf("connections = %d, want a single fresh-session recovery", connections)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, fs.files["source.mkv"]) {
		t.Fatalf("staged = %q, err = %v", got, err)
	}
	if !s.signedDialer("test password").Negotiator.RequireMessageSigning {
		t.Fatal("SMB reconnect must never relax mandatory signing")
	}
}

func TestDownloadRecoveryDiscardsPartialBytes(t *testing.T) {
	root := t.TempDir()
	// A large first attempt gets beyond the read preflight, then its session
	// dies. The replacement source is deliberately shorter to detect stale tails.
	first := bytes.Repeat([]byte("first attempt"), 20000)
	final := []byte("replacement source")
	fs := &memoryShare{files: map[string][]byte{"source.mkv": first}}
	s := testStore(fs, root)
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		if connections == 1 {
			return fn(faultShare{share: fs, open: func(string) (readFile, error) {
				return failedRead{Reader: bytes.NewReader(first), err: &smb2.TransportError{Err: syscall.ECONNRESET}}, nil
			}})
		}
		fs.files["source.mkv"] = final
		return fn(fs)
	}
	dst := filepath.Join(t.TempDir(), "input.mkv")
	if err := s.DownloadAtomic(context.Background(), filepath.Join(root, "source.mkv"), dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, final) || connections != 2 {
		t.Fatalf("staged length=%d want=%d, connections=%d, err=%v", len(got), len(final), connections, err)
	}
}

func TestStorageRecoveryIsBoundedAndSkipsPermanentErrors(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		class       string
		connections int
	}{
		{"signing", &smb2.InvalidResponseError{Message: "signing required"}, SMBSigningRequired, 2},
		{"expired session", &smb2.ResponseError{Code: 0xC000035C}, SMBSessionInvalid, 2},
		{"bad credentials", &smb2.ResponseError{Code: 0xC000006D}, SMBAuthFailed, 1},
		{"access denied", &smb2.ResponseError{Code: 0xC0000022}, StoragePermissionDenied, 1},
		{"local permission", os.ErrPermission, StoragePermissionDenied, 1},
		{"io failure", syscall.EIO, StorageIOError, 1},
		{"missing source", os.ErrNotExist, SourceUnreachable, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(&memoryShare{}, t.TempDir())
			connections := 0
			s.connect = func(context.Context, func(share) error) error {
				connections++
				return tc.err
			}
			_, err := s.Stat(context.Background(), filepath.Join(s.cfg.LocalRoot, "source.mkv"))
			var boundary *Error
			if !errors.As(err, &boundary) || Classify(err) != tc.class || !errors.Is(err, tc.err) {
				t.Fatalf("typed/wrapped cause lost: err=%v class=%s", err, Classify(err))
			}
			if connections != tc.connections {
				t.Fatalf("connections=%d want=%d", connections, tc.connections)
			}
		})
	}
}

func TestPublishReconcilesUncertainRenameOnFreshSession(t *testing.T) {
	root := t.TempDir()
	fs := &memoryShare{files: map[string][]byte{}}
	s := testStore(fs, root)
	local := filepath.Join(t.TempDir(), "candidate.mkv")
	payload := []byte("verified candidate")
	if err := os.WriteFile(local, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	connections, renames := 0, 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		return fn(faultShare{share: fs, rename: func(from, to string) error {
			renames++
			if err := fs.RenameNoReplace(from, to); err != nil {
				return err
			}
			// The server committed the rename but the reply was lost.
			return &smb2.TransportError{Err: syscall.ECONNRESET}
		}})
	}
	if err := s.Publish(context.Background(), local, filepath.Join(root, "candidate.mkv"), "job-1"); err != nil {
		t.Fatal(err)
	}
	if connections != 2 || renames != 1 || !bytes.Equal(fs.files["candidate.mkv"], payload) || len(fs.files) != 1 {
		t.Fatalf("connections=%d renames=%d files=%v", connections, renames, fs.files)
	}
}

func TestPublishRecoveryNeverClobbersConflictingFinal(t *testing.T) {
	root := t.TempDir()
	fs := &memoryShare{files: map[string][]byte{}}
	s := testStore(fs, root)
	local := filepath.Join(t.TempDir(), "candidate.mkv")
	if err := os.WriteFile(local, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	connections := 0
	s.connect = func(_ context.Context, fn func(share) error) error {
		connections++
		if connections == 1 {
			fs.files["candidate.mkv"] = []byte("someone else's file")
			return &smb2.TransportError{Err: syscall.ECONNRESET}
		}
		return fn(fs)
	}
	err := s.Publish(context.Background(), local, filepath.Join(root, "candidate.mkv"), "job-1")
	if !errors.Is(err, ErrDestinationExists) || string(fs.files["candidate.mkv"]) != "someone else's file" || connections != 2 {
		t.Fatalf("err=%v files=%v connections=%d", err, fs.files, connections)
	}
}

func TestStorageCancellationDoesNotReconnect(t *testing.T) {
	s := testStore(&memoryShare{}, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	connections := 0
	s.connect = func(context.Context, func(share) error) error {
		connections++
		cancel()
		return fmt.Errorf("read SMB source: %w", context.Canceled)
	}
	_, err := s.Stat(ctx, filepath.Join(s.cfg.LocalRoot, "source.mkv"))
	if !errors.Is(err, context.Canceled) || Classify(err) != "cancelled" || connections != 1 {
		t.Fatalf("err=%v class=%s connections=%d", err, Classify(err), connections)
	}
}
