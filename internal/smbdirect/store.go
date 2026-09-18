// Package smbdirect provides the production media-store boundary used by the
// transcode worker to move media between a NAS and local SSD without a kernel
// SMB mount.
package smbdirect

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
	"golang.org/x/sys/unix"
)

var ErrDestinationExists = errors.New("SMB destination already exists with different content")

type Config struct {
	Enabled      bool          `json:"enabled" yaml:"enabled"`
	LocalRoot    string        `json:"local_root" yaml:"local_root"`
	Server       string        `json:"server" yaml:"server"`
	Share        string        `json:"share" yaml:"share"`
	Username     string        `json:"username" yaml:"username"`
	Domain       string        `json:"domain,omitempty" yaml:"domain,omitempty"`
	PasswordFile string        `json:"password_file" yaml:"password_file"`
	Timeout      time.Duration `json:"-" yaml:"-"`
	TimeoutText  string        `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

func (c *Config) Normalize(allowedRoots []string) error {
	if c == nil || !c.Enabled {
		return nil
	}
	c.LocalRoot = filepath.Clean(strings.TrimSpace(c.LocalRoot))
	c.Server = strings.TrimSpace(c.Server)
	c.Share = strings.TrimSpace(c.Share)
	c.Username = strings.TrimSpace(c.Username)
	c.Domain = strings.TrimSpace(c.Domain)
	c.PasswordFile = filepath.Clean(strings.TrimSpace(c.PasswordFile))
	if !filepath.IsAbs(c.LocalRoot) || c.LocalRoot == string(filepath.Separator) {
		return errors.New("smb_direct.local_root must be an absolute non-root path")
	}
	if !withinAnyRoot(c.LocalRoot, allowedRoots) {
		return fmt.Errorf("smb_direct.local_root %q must be within allowed_roots", c.LocalRoot)
	}
	if c.Server == "" || c.Share == "" || c.Username == "" || c.PasswordFile == "" {
		return errors.New("smb_direct server, share, username, and password_file are required")
	}
	if strings.ContainsAny(c.Share, `/\\`) || c.Share == "." || c.Share == ".." {
		return errors.New("smb_direct.share must be one share name")
	}
	if !filepath.IsAbs(c.PasswordFile) {
		return errors.New("smb_direct.password_file must be absolute")
	}
	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		if strings.Contains(c.Server, ":") {
			return fmt.Errorf("smb_direct.server must be host:port (IPv6 must use brackets): %q", c.Server)
		}
		c.Server = net.JoinHostPort(c.Server, "445")
	}
	if c.Timeout == 0 {
		if strings.TrimSpace(c.TimeoutText) == "" {
			c.Timeout = 30 * time.Minute
		} else {
			d, err := time.ParseDuration(c.TimeoutText)
			if err != nil || d <= 0 {
				return fmt.Errorf("smb_direct.timeout must be a positive Go duration: %q", c.TimeoutText)
			}
			c.Timeout = d
		}
	}
	return nil
}

func withinAnyRoot(target string, roots []string) bool {
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		rel, err := filepath.Rel(root, target)
		if err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

type readFile interface {
	io.Reader
	Close() error
}

type writeFile interface {
	io.Writer
	Sync() error
	Close() error
}

type share interface {
	Open(string) (readFile, error)
	OpenExclusive(string) (writeFile, error)
	Stat(string) (os.FileInfo, error)
	Mkdir(string, os.FileMode) error
	RenameNoReplace(string, string) error
	Remove(string) error
}

type smbShare struct{ inner *smb2.Share }

func (s smbShare) Open(name string) (readFile, error) { return s.inner.Open(name) }
func (s smbShare) OpenExclusive(name string) (writeFile, error) {
	return s.inner.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
func (s smbShare) Stat(name string) (os.FileInfo, error) { return s.inner.Stat(name) }
func (s smbShare) Mkdir(name string, mode os.FileMode) error {
	return s.inner.Mkdir(name, mode)
}
func (s smbShare) RenameNoReplace(oldName, newName string) error {
	return s.inner.Rename(oldName, newName)
}
func (s smbShare) Remove(name string) error { return s.inner.Remove(name) }

type Store struct {
	cfg     Config
	connect func(context.Context, func(share) error) error
}

func New(cfg Config) *Store {
	s := &Store{cfg: cfg}
	s.connect = s.connectSMB
	return s
}

func (s *Store) Maps(localPath string) bool {
	_, ok := s.RemotePath(localPath)
	return ok
}

func (s *Store) RemotePath(localPath string) (string, bool) {
	if s == nil || !s.cfg.Enabled {
		return "", false
	}
	clean := filepath.Clean(strings.TrimSpace(localPath))
	rel, err := filepath.Rel(s.cfg.LocalRoot, clean)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if strings.ContainsRune(rel, '\x00') {
		return "", false
	}
	remote := path.Clean(filepath.ToSlash(rel))
	if remote == "." || remote == ".." || strings.HasPrefix(remote, "../") {
		return "", false
	}
	return remote, true
}

func (s *Store) Stat(ctx context.Context, localPath string) (info os.FileInfo, retErr error) {
	defer func() { retErr = wrapError("stat", retErr) }()
	remote, ok := s.RemotePath(localPath)
	if !ok {
		return nil, fmt.Errorf("path %q is outside smb_direct.local_root", localPath)
	}
	err := s.withSessionRetry(ctx, "stat", func(fs share) error {
		var err error
		info, err = fs.Stat(remote)
		return err
	})
	return info, err
}

// DownloadAtomic copies a remote source to a local SSD path and publishes the
// local file only after a complete sync and size check.
func (s *Store) DownloadAtomic(ctx context.Context, remoteLocalPath, localPath string) (retErr error) {
	defer func() { retErr = wrapError("download", retErr) }()
	remote, ok := s.RemotePath(remoteLocalPath)
	if !ok {
		return fmt.Errorf("path %q is outside smb_direct.local_root", remoteLocalPath)
	}
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("creating local staging directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(localPath), ".smb-download-*.partial")
	if err != nil {
		return fmt.Errorf("creating local staging file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	err = s.withSessionRetry(ctx, "download", func(fs share) error {
		// A recovered session starts a fresh read. Never append to a partial
		// download or leave bytes from a longer previous attempt behind.
		if err := tmp.Truncate(0); err != nil {
			return fmt.Errorf("resetting local staging file: %w", err)
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewinding local staging file: %w", err)
		}
		info, err := fs.Stat(remote)
		if err != nil {
			return fmt.Errorf("statting SMB source: %w", err)
		}
		if !info.Mode().IsRegular() {
			return errors.New("SMB source is not a regular file")
		}
		src, err := fs.Open(remote)
		if err != nil {
			return fmt.Errorf("opening SMB source: %w", err)
		}
		// Exercise an authenticated source read before committing the staged
		// download. Small inputs may reach EOF within this preflight.
		preflight := make([]byte, 64*1024)
		read, readErr := io.ReadFull(src, preflight)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			_ = src.Close()
			return fmt.Errorf("reading SMB source preflight: %w", readErr)
		}
		n, copyErr := copyContext(ctx, tmp, io.MultiReader(bytes.NewReader(preflight[:read]), src))
		closeErr := src.Close()
		if copyErr != nil {
			return fmt.Errorf("reading SMB source: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing SMB source: %w", closeErr)
		}
		if n != info.Size() {
			return fmt.Errorf("SMB source changed while downloading: copied=%d stat=%d", n, info.Size())
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("downloading %q: %w", remoteLocalPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing staged input: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing staged input: %w", err)
	}
	if err := os.Rename(tmpName, localPath); err != nil {
		return fmt.Errorf("publishing staged input: %w", err)
	}
	return nil
}

// Publish uploads a local candidate to an exclusive job-owned partial, reads
// the entire partial back for SHA-256 verification, and renames without
// replacement. A retry succeeds only if an existing final is byte-identical.
func (s *Store) Publish(ctx context.Context, localCandidate, destination, jobID string) (retErr error) {
	defer func() { retErr = wrapError("publish", retErr) }()
	remote, ok := s.RemotePath(destination)
	if !ok {
		return fmt.Errorf("path %q is outside smb_direct.local_root", destination)
	}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" || strings.ContainsAny(jobID, `/\\`) || jobID == "." || jobID == ".." {
		return errors.New("invalid job id for SMB publication")
	}
	localHash, localSize, err := hashLocal(ctx, localCandidate)
	if err != nil {
		return fmt.Errorf("hashing local candidate: %w", err)
	}
	partial := remote + ".partial." + jobID
	return s.withSessionRetry(ctx, "publish", func(fs share) (retErr error) {
		if err := ensureRemoteDir(fs, path.Dir(remote)); err != nil {
			return fmt.Errorf("ensuring SMB destination directory: %w", err)
		}
		if info, err := fs.Stat(remote); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%w: remote destination is not a regular file", ErrDestinationExists)
			}
			hash, size, err := hashRemote(ctx, fs, remote)
			if err != nil {
				return fmt.Errorf("verifying existing destination: %w", err)
			}
			if size == localSize && hash == localHash {
				return nil
			}
			return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("checking destination: %w", err)
		}

		created := false
		defer func() {
			if created && retErr != nil {
				_ = fs.Remove(partial)
			}
		}()

		dst, err := fs.OpenExclusive(partial)
		if err != nil {
			if !os.IsExist(err) {
				return fmt.Errorf("creating exclusive SMB partial: %w", err)
			}
			// This exact name is owned by this job. A crashed upload may have
			// left it behind; remove only that partial and create it afresh.
			if err := fs.Remove(partial); err != nil {
				return fmt.Errorf("removing stale job-owned SMB partial: %w", err)
			}
			dst, err = fs.OpenExclusive(partial)
			if err != nil {
				return fmt.Errorf("recreating exclusive SMB partial: %w", err)
			}
		}
		created = true
		src, err := os.Open(localCandidate)
		if err != nil {
			_ = dst.Close()
			return err
		}
		_, copyErr := copyContext(ctx, dst, src)
		closeSrcErr := src.Close()
		syncErr := dst.Sync()
		closeDstErr := dst.Close()
		if err := errors.Join(copyErr, closeSrcErr, syncErr, closeDstErr); err != nil {
			return fmt.Errorf("uploading SMB partial: %w", err)
		}
		hash, size, err := hashRemote(ctx, fs, partial)
		if err != nil {
			return fmt.Errorf("reading back SMB partial: %w", err)
		}
		if size != localSize || hash != localHash {
			return fmt.Errorf("SMB partial readback mismatch: bytes=%d/%d sha256=%s/%s", size, localSize, hash, localHash)
		}
		if err := fs.RenameNoReplace(partial, remote); err != nil {
			if os.IsExist(err) {
				hash, size, verifyErr := hashRemote(ctx, fs, remote)
				if verifyErr != nil {
					return fmt.Errorf("verifying destination after rename conflict: %w", verifyErr)
				}
				if size == localSize && hash == localHash {
					_ = fs.Remove(partial)
					created = false
					return nil
				}
				return fmt.Errorf("%w: %s", ErrDestinationExists, destination)
			}
			return fmt.Errorf("renaming SMB partial without replacement: %w", err)
		}
		created = false
		hash, size, err = hashRemote(ctx, fs, remote)
		if err != nil {
			return fmt.Errorf("verifying SMB final: %w", err)
		}
		if size != localSize || hash != localHash {
			return fmt.Errorf("SMB final verification mismatch: bytes=%d/%d sha256=%s/%s", size, localSize, hash, localHash)
		}
		return nil
	})
}

func ensureRemoteDir(fs share, dir string) error {
	dir = path.Clean(dir)
	if dir == "." || dir == "" {
		return nil
	}
	current := ""
	for _, component := range strings.Split(dir, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("invalid SMB destination directory")
		}
		current = path.Join(current, component)
		info, err := fs.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%q exists but is not a directory", current)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return err
		}
		if err := fs.Mkdir(current, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// CheckRoot verifies authenticated read/write access without relying on a
// mounted filesystem. It creates and removes one random empty file at the
// share root and never inspects unrelated media.
func (s *Store) CheckRoot(ctx context.Context) (retErr error) {
	defer func() { retErr = wrapError("check_root", retErr) }()
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	name := ".navigatorr-doctor-" + hex.EncodeToString(token[:])
	return s.connect(ctx, func(fs share) error {
		if _, err := fs.Stat("."); err != nil {
			return fmt.Errorf("reading SMB share root: %w", err)
		}
		f, err := fs.OpenExclusive(name)
		if err != nil {
			return fmt.Errorf("creating SMB doctor file: %w", err)
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			_ = fs.Remove(name)
			return fmt.Errorf("closing SMB doctor file: %w", err)
		}
		if err := fs.Remove(name); err != nil {
			return fmt.Errorf("removing SMB doctor file: %w", err)
		}
		return nil
	})
}

func (s *Store) connectSMB(ctx context.Context, operation func(share) error) (retErr error) {
	password, err := readPasswordFile(s.cfg.PasswordFile)
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(opCtx, "tcp", s.cfg.Server)
	if err != nil {
		return fmt.Errorf("connecting to SMB server %s: %w", s.cfg.Server, err)
	}
	defer conn.Close()
	d := s.signedDialer(password)
	session, err := d.DialContext(opCtx, conn)
	if err != nil {
		if Classify(err) == StorageIOError {
			err = &Error{Class: SMBAuthFailed, Op: "authenticate", Err: err}
		}
		return fmt.Errorf("authenticating SMB session: %w", err)
	}
	var mounted *smb2.Share
	defer func() {
		if retErr != nil {
			// Invalidate the failed connection immediately. In particular,
			// do not wait for a damaged session to acknowledge Logoff.
			_ = conn.Close()
			return
		}
		// go-smb2's Session and Share default to context.Background even
		// after DialContext/Mount. Explicitly bound cleanup as well as I/O.
		cleanupCtx, cleanupCancel := context.WithTimeout(opCtx, 2*time.Second)
		defer cleanupCancel()
		if mounted != nil {
			_ = mounted.WithContext(cleanupCtx).Umount()
		}
		_ = session.WithContext(cleanupCtx).Logoff()
	}()
	mounted, err = session.WithContext(opCtx).Mount(s.cfg.Share)
	if err != nil {
		return fmt.Errorf("mounting SMB share %q in userspace: %w", s.cfg.Share, err)
	}
	return operation(smbShare{inner: mounted.WithContext(opCtx)})
}

// Every attempt, including recovery, uses the same signing requirement. There
// is deliberately no unsigned fallback for a server/session signing failure.
func (s *Store) signedDialer(password string) *smb2.Dialer {
	return &smb2.Dialer{
		Negotiator: smb2.Negotiator{RequireMessageSigning: true},
		Initiator:  &smb2.NTLMInitiator{User: s.cfg.Username, Password: password, Domain: s.cfg.Domain},
	}
}

func readPasswordFile(name string) (string, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("opening SMB password_file safely: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("SMB password_file must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("SMB password_file permissions %04o are too broad; require 0600 or stricter", info.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return "", errors.New("reading SMB password_file failed or exceeded 64 KiB")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if password == "" || strings.ContainsRune(password, '\x00') {
		return "", errors.New("SMB password_file contains an empty or invalid password")
	}
	return password, nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 1024*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func hashLocal(ctx context.Context, name string) (string, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := copyContext(ctx, h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func hashRemote(ctx context.Context, fs share, name string) (string, int64, error) {
	f, err := fs.Open(name)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := copyContext(ctx, h, f)
	closeErr := f.Close()
	return hex.EncodeToString(h.Sum(nil)), n, errors.Join(copyErr, closeErr)
}
