package smbprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"time"
)

func seedWithShare(ctx context.Context, cfg Config, remotePath string, fs share) (result SeedResult, retErr error) {
	wantHash := DeterministicFixtureSHA256()
	result = SeedResult{
		Server: cfg.Server, Share: cfg.Share, RemoteSource: remotePath,
		Bytes: DeterministicFixtureSize(), SHA256: wantHash,
	}

	createdBase := false
	if info, err := fs.Stat(cfg.BasePath); err == nil {
		if !info.IsDir() {
			return result, fmt.Errorf("dedicated base_path %q exists but is not a directory", cfg.BasePath)
		}
	} else if os.IsNotExist(err) {
		if err := fs.Mkdir(cfg.BasePath, 0o700); err != nil {
			return result, fmt.Errorf("creating dedicated fixture directory: %w", err)
		}
		createdBase = true
	} else {
		return result, fmt.Errorf("checking dedicated fixture directory: %w", err)
	}

	createdFixture := false
	cleanupOwned := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cleanupFS := fs.WithContext(cleanupCtx)
		var cleanupErrs []error
		if createdFixture {
			if err := cleanupFS.Remove(remotePath); err != nil && !os.IsNotExist(err) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("removing owned fixture %q: %w", remotePath, err))
			}
		}
		if createdBase {
			// Remove only the exact directory and only if it is empty. Never recurse.
			if err := cleanupFS.Remove(cfg.BasePath); err != nil && !os.IsNotExist(err) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("removing owned fixture directory %q: %w", cfg.BasePath, err))
			}
		}
		return errors.Join(cleanupErrs...)
	}

	dst, err := fs.OpenExclusive(remotePath)
	if err != nil {
		cleanupErr := cleanupOwned()
		if os.IsExist(err) {
			return result, errors.Join(fmt.Errorf("refusing to overwrite existing fixture %q: %w", remotePath, err), cleanupErr)
		}
		return result, errors.Join(fmt.Errorf("creating fixture exclusively: %w", err), cleanupErr)
	}
	createdFixture = true
	payload := deterministicFixture()
	_, copyErr := copyContext(ctx, dst, bytes.NewReader(payload))
	syncErr := dst.Sync()
	closeErr := dst.Close()
	if err := firstError(copyErr, syncErr, closeErr); err != nil {
		cleanupErr := cleanupOwned()
		return result, errors.Join(fmt.Errorf("writing deterministic fixture: %w", err), cleanupErr)
	}
	gotHash, gotBytes, err := hashRemote(ctx, fs, remotePath)
	if err != nil || gotBytes != int64(len(payload)) || gotHash != wantHash {
		cleanupErr := cleanupOwned()
		return result, errors.Join(fmt.Errorf("verifying deterministic fixture: bytes=%d sha256=%s err=%v", gotBytes, gotHash, err), cleanupErr)
	}
	result.Created = true
	result.OK = true
	return result, nil
}

var probeToken = randomToken

func runWithShare(ctx context.Context, cfg Config, source string, fs share) (res Result, retErr error) {
	res.Server, res.Share, res.RemoteSource = cfg.Server, cfg.Share, source
	info, err := fs.Stat(source)
	if err != nil {
		return res, fmt.Errorf("statting remote fixture %q: %w", source, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return res, fmt.Errorf("remote fixture %q must be a non-empty regular file", source)
	}
	if info.Size() > cfg.MaxSourceBytes {
		return res, fmt.Errorf("remote fixture is %d bytes, exceeding max_source_bytes=%d", info.Size(), cfg.MaxSourceBytes)
	}

	token, err := probeToken()
	if err != nil {
		return res, err
	}
	prefix := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token)
	partial := prefix + ".partial"
	collision := prefix + ".collision"
	final := prefix + ".final"
	artifacts := []string{partial, collision, final}
	owned := make(map[string]bool, len(artifacts))
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ownedArtifacts := make([]string, 0, len(artifacts))
		for _, name := range artifacts {
			if owned[name] {
				ownedArtifacts = append(ownedArtifacts, name)
			}
		}
		remaining := cleanup(fs.WithContext(cleanupCtx), ownedArtifacts)
		res.CleanupArtifacts = remaining
		res.CleanupComplete = len(remaining) == 0
		if len(remaining) > 0 && retErr == nil {
			retErr = fmt.Errorf("probe passed but cleanup left artifacts: %v", remaining)
			res.OK = false
		}
	}()

	local, err := os.CreateTemp("", "navigatorr-smb-probe-*.bin")
	if err != nil {
		return res, fmt.Errorf("creating local probe scratch: %w", err)
	}
	localName := local.Name()
	defer os.Remove(localName)

	remote, err := fs.Open(source)
	if err != nil {
		local.Close()
		return res, fmt.Errorf("opening remote fixture: %w", err)
	}
	h := sha256.New()
	n, copyErr := copyContext(ctx, io.MultiWriter(local, h), remote)
	closeRemoteErr := remote.Close()
	syncErr := local.Sync()
	closeLocalErr := local.Close()
	if err := firstError(copyErr, closeRemoteErr, syncErr, closeLocalErr); err != nil {
		return res, fmt.Errorf("downloading remote fixture: %w", err)
	}
	if n != info.Size() {
		return res, fmt.Errorf("downloaded %d bytes, remote stat reported %d", n, info.Size())
	}
	res.Bytes = n
	res.SHA256 = hex.EncodeToString(h.Sum(nil))
	if res.SHA256 != cfg.ExpectedSHA256 {
		return res, fmt.Errorf("downloaded fixture SHA-256 mismatch: got %s want %s", res.SHA256, cfg.ExpectedSHA256)
	}

	partialCreated, err := uploadExclusive(ctx, fs, partial, localName)
	if partialCreated {
		owned[partial] = true
	}
	if err != nil {
		return res, fmt.Errorf("uploading exclusive partial: %w", err)
	}
	partialHash, partialBytes, err := hashRemote(ctx, fs, partial)
	if err != nil {
		return res, fmt.Errorf("reading uploaded partial back: %w", err)
	}
	if partialBytes != n || partialHash != res.SHA256 {
		return res, fmt.Errorf("uploaded partial integrity mismatch: bytes=%d sha256=%s", partialBytes, partialHash)
	}
	res.ReadbackProven = true

	sentinel := []byte("navigatorr-no-clobber-sentinel\n")
	collisionCreated, err := writeExclusive(ctx, fs, collision, bytes.NewReader(sentinel))
	if collisionCreated {
		owned[collision] = true
	}
	if err != nil {
		return res, fmt.Errorf("creating collision sentinel: %w", err)
	}
	if err := fs.RenameNoReplace(partial, collision); err == nil {
		return res, fmt.Errorf("unsafe SMB rename overwrote an existing destination")
	} else if !os.IsExist(err) {
		return res, fmt.Errorf("no-clobber rename was inconclusive (expected destination-exists): %w", err)
	}
	collisionHash, collisionBytes, err := hashRemote(ctx, fs, collision)
	if err != nil {
		return res, fmt.Errorf("checking collision sentinel after rejected rename: %w", err)
	}
	wantCollision := sha256.Sum256(sentinel)
	if collisionBytes != int64(len(sentinel)) || collisionHash != hex.EncodeToString(wantCollision[:]) {
		return res, fmt.Errorf("collision destination changed during rejected rename")
	}
	if hash, size, err := hashRemote(ctx, fs, partial); err != nil || size != n || hash != res.SHA256 {
		return res, fmt.Errorf("partial changed during rejected rename: bytes=%d sha256=%s err=%v", size, hash, err)
	}
	res.NoClobberProven = true

	if err := fs.Remove(collision); err != nil {
		return res, fmt.Errorf("removing collision sentinel: %w", err)
	}
	owned[collision] = false
	if err := fs.RenameNoReplace(partial, final); err != nil {
		return res, fmt.Errorf("renaming verified partial to absent destination: %w", err)
	}
	owned[partial] = false
	owned[final] = true
	finalHash, finalBytes, err := hashRemote(ctx, fs, final)
	if err != nil {
		return res, fmt.Errorf("reading renamed final back: %w", err)
	}
	if finalBytes != n || finalHash != res.SHA256 {
		return res, fmt.Errorf("renamed final integrity mismatch: bytes=%d sha256=%s", finalBytes, finalHash)
	}
	res.RenameProven = true
	res.OK = true
	return res, nil
}

func uploadExclusive(ctx context.Context, fs share, remotePath, localPath string) (created bool, err error) {
	f, err := os.Open(localPath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	return writeExclusive(ctx, fs, remotePath, f)
}

func writeExclusive(ctx context.Context, fs share, remotePath string, src io.Reader) (created bool, err error) {
	dst, err := fs.OpenExclusive(remotePath)
	if err != nil {
		return false, err
	}
	_, copyErr := copyContext(ctx, dst, src)
	syncErr := dst.Sync()
	closeErr := dst.Close()
	if err := firstError(copyErr, syncErr, closeErr); err != nil {
		return true, err
	}
	return true, nil
}

func hashRemote(ctx context.Context, fs share, remotePath string) (string, int64, error) {
	f, err := fs.Open(remotePath)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := copyContext(ctx, h, f)
	closeErr := f.Close()
	if err := firstError(copyErr, closeErr); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 256*1024)
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

func cleanup(fs share, artifacts []string) []string {
	for _, name := range artifacts {
		if err := fs.Remove(name); err != nil && !os.IsNotExist(err) {
			continue
		}
	}
	var remaining []string
	for _, name := range artifacts {
		if _, err := fs.Stat(name); err == nil || !os.IsNotExist(err) {
			remaining = append(remaining, name)
		}
	}
	return remaining
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
