// Package smbprobe implements the isolated direct-SMB acceptance probe used
// before Navigatorr is allowed to route production media through an SMB
// userspace client. It is intentionally not wired into transcodeworker.
package smbprobe

import (
	"context"
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
)

const defaultMaxSourceBytes int64 = 64 << 20

const (
	// SeedBasePath and SeedSourceName deliberately make fixture seeding much
	// narrower than the read-only gate. The seeder can never be pointed at a
	// media directory or at an arbitrary filename such as E04.
	SeedBasePath   = "navigatorr-probe"
	SeedSourceName = "input.bin"
	seedSize       = 64 << 10
)

// Config contains only probe-specific SMB settings. PasswordFile is required;
// passwords are never accepted on argv or embedded in this structure.
type Config struct {
	Server         string        `yaml:"server"`
	Share          string        `yaml:"share"`
	Username       string        `yaml:"username"`
	Domain         string        `yaml:"domain,omitempty"`
	PasswordFile   string        `yaml:"password_file"`
	BasePath       string        `yaml:"base_path"`
	ExpectedSHA256 string        `yaml:"expected_sha256"`
	Timeout        time.Duration `yaml:"-"`
	TimeoutText    string        `yaml:"timeout,omitempty"`
	MaxSourceBytes int64         `yaml:"max_source_bytes,omitempty"`
}

// Result is safe to print or persist: it never contains credentials.
type Result struct {
	OK               bool     `json:"ok"`
	Server           string   `json:"server"`
	Share            string   `json:"share"`
	RemoteSource     string   `json:"remote_source"`
	Bytes            int64    `json:"bytes"`
	SHA256           string   `json:"sha256"`
	NoClobberProven  bool     `json:"no_clobber_proven"`
	ReadbackProven   bool     `json:"readback_proven"`
	RenameProven     bool     `json:"rename_proven"`
	CleanupComplete  bool     `json:"cleanup_complete"`
	CleanupArtifacts []string `json:"cleanup_artifacts,omitempty"`
}

// SeedResult describes the one artificial fixture created by Seed. It contains
// no credentials and is safe to emit as JSON.
type SeedResult struct {
	OK           bool   `json:"ok"`
	Server       string `json:"server"`
	Share        string `json:"share"`
	RemoteSource string `json:"remote_source"`
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256"`
	Created      bool   `json:"created"`
}

// DeterministicFixtureSHA256 returns the fixed digest required in probe config
// before Seed will write anything to SMB.
func DeterministicFixtureSHA256() string {
	sum := sha256.Sum256(deterministicFixture())
	return hex.EncodeToString(sum[:])
}

// DeterministicFixtureSize returns the fixed fixture length in bytes.
func DeterministicFixtureSize() int64 { return seedSize }

func deterministicFixture() []byte {
	const marker = "navigatorr direct SMB acceptance fixture v1\n"
	payload := make([]byte, seedSize)
	for i := range payload {
		payload[i] = marker[i%len(marker)]
	}
	return payload
}

func (c *Config) normalize() error {
	c.Server = strings.TrimSpace(c.Server)
	c.Share = strings.TrimSpace(c.Share)
	c.Username = strings.TrimSpace(c.Username)
	c.Domain = strings.TrimSpace(c.Domain)
	c.PasswordFile = strings.TrimSpace(c.PasswordFile)
	c.ExpectedSHA256 = strings.ToLower(strings.TrimSpace(c.ExpectedSHA256))
	if c.Server == "" || c.Share == "" || c.Username == "" || c.PasswordFile == "" {
		return errors.New("server, share, username, and password_file are required")
	}
	if len(c.ExpectedSHA256) != 64 {
		return errors.New("expected_sha256 must be a 64-character SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(c.ExpectedSHA256); err != nil {
		return errors.New("expected_sha256 must be valid hexadecimal")
	}
	if strings.ContainsAny(c.Share, `/\`) || c.Share == "." || c.Share == ".." {
		return errors.New("share must be a single SMB share name")
	}
	if !filepath.IsAbs(c.PasswordFile) {
		return errors.New("password_file must be an absolute path")
	}
	if _, _, err := net.SplitHostPort(c.Server); err != nil {
		if strings.Contains(c.Server, ":") {
			return fmt.Errorf("server must be host:port (IPv6 must use brackets): %q", c.Server)
		}
		c.Server = net.JoinHostPort(c.Server, "445")
	}
	base, err := cleanRelative(c.BasePath, false)
	if err != nil {
		return fmt.Errorf("base_path: %w", err)
	}
	c.BasePath = base
	if c.Timeout == 0 {
		if strings.TrimSpace(c.TimeoutText) == "" {
			c.Timeout = 2 * time.Minute
		} else {
			d, err := time.ParseDuration(c.TimeoutText)
			if err != nil || d <= 0 {
				return fmt.Errorf("timeout must be a positive Go duration: %q", c.TimeoutText)
			}
			c.Timeout = d
		}
	}
	if c.MaxSourceBytes == 0 {
		c.MaxSourceBytes = defaultMaxSourceBytes
	}
	if c.MaxSourceBytes < 1 {
		return errors.New("max_source_bytes must be positive")
	}
	return nil
}

// ValidateConfig validates and normalizes a probe configuration.
func ValidateConfig(c *Config) error {
	if c == nil {
		return errors.New("nil SMB probe config")
	}
	return c.normalize()
}

func cleanRelative(raw string, allowEmpty bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" && allowEmpty {
		return "", nil
	}
	if raw == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(raw, '\x00') || strings.Contains(raw, `\`) {
		return "", errors.New("NUL and backslash are not allowed")
	}
	if strings.HasPrefix(raw, "/") {
		return "", errors.New("path must be share-relative")
	}
	for _, component := range strings.Split(raw, "/") {
		if component == ".." {
			return "", errors.New("path contains traversal")
		}
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes the configured share root")
	}
	return clean, nil
}

func remoteJoin(base, rel string) (string, error) {
	rel, err := cleanRelative(rel, false)
	if err != nil {
		return "", err
	}
	if base == "" {
		return rel, nil
	}
	joined := path.Join(base, rel)
	if joined != base && !strings.HasPrefix(joined, base+"/") {
		return "", errors.New("path escapes the configured base_path")
	}
	return joined, nil
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
	WithContext(context.Context) share
	Open(string) (readFile, error)
	OpenExclusive(string) (writeFile, error)
	Stat(string) (os.FileInfo, error)
	Mkdir(string, os.FileMode) error
	RenameNoReplace(string, string) error
	Remove(string) error
}

type smbShare struct{ inner *smb2.Share }

func (s smbShare) WithContext(ctx context.Context) share {
	return smbShare{inner: s.inner.WithContext(ctx)}
}
func (s smbShare) Open(name string) (readFile, error) { return s.inner.Open(name) }
func (s smbShare) OpenExclusive(name string) (writeFile, error) {
	return s.inner.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
func (s smbShare) Stat(name string) (os.FileInfo, error) { return s.inner.Stat(name) }
func (s smbShare) Mkdir(name string, mode os.FileMode) error {
	return s.inner.Mkdir(name, mode)
}

// Rename in the pinned github.com/hirochachacha/go-smb2 v1.1.0 sends
// FILE_RENAME_INFORMATION_TYPE_2 with ReplaceIfExists=0. The live probe also
// proves this against the actual NAS before the library can be productionized.
func (s smbShare) RenameNoReplace(oldName, newName string) error {
	return s.inner.Rename(oldName, newName)
}
func (s smbShare) Remove(name string) error { return s.inner.Remove(name) }

// Run connects directly to SMB, downloads an existing small fixture, uploads
// it exclusively, verifies a full readback hash, proves rename no-clobber, then
// performs and verifies a normal same-share rename. All probe artifacts use a
// unique directory and are cleaned on both success and failure.
func Run(ctx context.Context, cfg Config, remoteSource string) (Result, error) {
	if err := cfg.normalize(); err != nil {
		return Result{}, err
	}
	remoteSource, err := remoteJoin(cfg.BasePath, remoteSource)
	if err != nil {
		return Result{}, fmt.Errorf("remote source: %w", err)
	}
	var result Result
	err = withSMBShare(ctx, cfg, func(opCtx context.Context, fs share) error {
		var opErr error
		result, opErr = runWithShare(opCtx, cfg, remoteSource, fs)
		return opErr
	})
	return result, err
}

// Seed creates the single deterministic artificial fixture used by Run. It is
// intentionally restricted to navigatorr-probe/input.bin, creates both the
// directory and file without replacement, verifies a full readback hash, and
// removes only objects it created if verification fails.
func Seed(ctx context.Context, cfg Config, remoteSource string) (SeedResult, error) {
	if err := cfg.normalize(); err != nil {
		return SeedResult{}, err
	}
	if cfg.BasePath != SeedBasePath {
		return SeedResult{}, fmt.Errorf("fixture seeding requires base_path %q", SeedBasePath)
	}
	cleanSource, err := cleanRelative(remoteSource, false)
	if err != nil {
		return SeedResult{}, fmt.Errorf("seed source: %w", err)
	}
	if cleanSource != SeedSourceName {
		return SeedResult{}, fmt.Errorf("fixture seeding requires source %q", SeedSourceName)
	}
	wantHash := DeterministicFixtureSHA256()
	if cfg.ExpectedSHA256 != wantHash {
		return SeedResult{}, fmt.Errorf("expected_sha256 does not match deterministic fixture: got %s want %s", cfg.ExpectedSHA256, wantHash)
	}
	if cfg.MaxSourceBytes < DeterministicFixtureSize() {
		return SeedResult{}, fmt.Errorf("max_source_bytes=%d is smaller than deterministic fixture size %d", cfg.MaxSourceBytes, DeterministicFixtureSize())
	}
	remotePath, err := remoteJoin(cfg.BasePath, cleanSource)
	if err != nil {
		return SeedResult{}, fmt.Errorf("seed source: %w", err)
	}
	result := SeedResult{
		Server: cfg.Server, Share: cfg.Share, RemoteSource: remotePath,
		Bytes: int64(seedSize), SHA256: wantHash,
	}
	err = withSMBShare(ctx, cfg, func(opCtx context.Context, fs share) error {
		var opErr error
		result, opErr = seedWithShare(opCtx, cfg, remotePath, fs)
		return opErr
	})
	return result, err
}

func withSMBShare(ctx context.Context, cfg Config, operation func(context.Context, share) error) error {
	password, err := readPasswordFile(cfg.PasswordFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	var nd = new(netDialer)
	conn, err := nd.dial(ctx, cfg.Server)
	if err != nil {
		return fmt.Errorf("connecting to SMB server %s: %w", cfg.Server, err)
	}
	defer conn.Close()

	d := &smb2.Dialer{
		Negotiator: smb2.Negotiator{RequireMessageSigning: true},
		Initiator: &smb2.NTLMInitiator{
			User: cfg.Username, Password: password, Domain: cfg.Domain,
		},
	}
	session, err := d.DialContext(ctx, conn)
	if err != nil {
		return fmt.Errorf("authenticating SMB session: %w", err)
	}
	defer session.Logoff()

	mounted, err := session.WithContext(ctx).Mount(cfg.Share)
	if err != nil {
		return fmt.Errorf("mounting SMB share %q in userspace: %w", cfg.Share, err)
	}
	defer mounted.Umount()
	return operation(ctx, smbShare{inner: mounted.WithContext(ctx)})
}
