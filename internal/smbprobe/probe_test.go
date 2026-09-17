package smbprobe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

type localShare struct {
	root      string
	clobber   bool
	renameErr error
}

func (s localShare) local(name string) string {
	return filepath.Join(s.root, filepath.FromSlash(name))
}

func (s localShare) WithContext(context.Context) share  { return s }
func (s localShare) Open(name string) (readFile, error) { return os.Open(s.local(name)) }
func (s localShare) OpenExclusive(name string) (writeFile, error) {
	return os.OpenFile(s.local(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
func (s localShare) Stat(name string) (os.FileInfo, error) { return os.Stat(s.local(name)) }
func (s localShare) Mkdir(name string, mode os.FileMode) error {
	return os.Mkdir(s.local(name), mode)
}
func (s localShare) RenameNoReplace(oldName, newName string) error {
	oldPath, newPath := s.local(oldName), s.local(newName)
	if s.renameErr != nil {
		return s.renameErr
	}
	if s.clobber {
		return os.Rename(oldPath, newPath)
	}
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	return os.Remove(oldPath)
}
func (s localShare) Remove(name string) error { return os.Remove(s.local(name)) }

func payloadSHA(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func TestSeedWithShareCreatesOnlyDeterministicFixture(t *testing.T) {
	root := t.TempDir()
	cfg := Config{
		Server: "nas:445", Share: "media", BasePath: SeedBasePath,
		ExpectedSHA256: DeterministicFixtureSHA256(),
	}
	remote := SeedBasePath + "/" + SeedSourceName

	result, err := seedWithShare(context.Background(), cfg, remote, localShare{root: root})
	if err != nil {
		t.Fatalf("seedWithShare: %v", err)
	}
	if !result.OK || !result.Created || result.RemoteSource != remote || result.SHA256 != DeterministicFixtureSHA256() {
		t.Fatalf("unexpected seed result: %+v", result)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(remote)))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != DeterministicFixtureSize() || payloadSHA(string(data)) != DeterministicFixtureSHA256() {
		t.Fatalf("seeded fixture mismatch: bytes=%d sha256=%s", len(data), payloadSHA(string(data)))
	}
}

func TestSeedWithShareRefusesOverwriteAndPreservesExistingBytes(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, SeedBasePath)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := []byte("do not overwrite")
	fixture := filepath.Join(base, SeedSourceName)
	if err := os.WriteFile(fixture, existing, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: "nas:445", Share: "media", BasePath: SeedBasePath, ExpectedSHA256: DeterministicFixtureSHA256()}

	result, err := seedWithShare(context.Background(), cfg, SeedBasePath+"/"+SeedSourceName, localShare{root: root})
	if err == nil || !errors.Is(err, os.ErrExist) || result.OK || result.Created {
		t.Fatalf("expected exclusive-create refusal, result=%+v err=%v", result, err)
	}
	got, readErr := os.ReadFile(fixture)
	if readErr != nil || string(got) != string(existing) {
		t.Fatalf("existing fixture changed: got=%q err=%v", got, readErr)
	}
}

func TestSeedWithShareCancellationCleansOnlyOwnedPaths(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := Config{Server: "nas:445", Share: "media", BasePath: SeedBasePath, ExpectedSHA256: DeterministicFixtureSHA256()}
	remote := SeedBasePath + "/" + SeedSourceName

	result, err := seedWithShare(ctx, cfg, remote, localShare{root: root})
	if err == nil || result.OK || result.Created {
		t.Fatalf("expected cancelled seed, result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(remote))); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled seed left fixture: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, SeedBasePath)); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled seed left owned base directory: %v", statErr)
	}
}

func TestSeedWithShareFailurePreservesPreexistingBaseAndSibling(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, SeedBasePath)
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "E04-untouched.marker")
	wantSibling := []byte("must remain byte-for-byte intact")
	if err := os.WriteFile(sibling, wantSibling, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := Config{Server: "nas:445", Share: "media", BasePath: SeedBasePath, ExpectedSHA256: DeterministicFixtureSHA256()}
	remote := SeedBasePath + "/" + SeedSourceName

	result, err := seedWithShare(ctx, cfg, remote, localShare{root: root})
	if err == nil || result.OK || result.Created {
		t.Fatalf("expected cancelled seed, result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(remote))); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled seed left fixture: %v", statErr)
	}
	gotSibling, readErr := os.ReadFile(sibling)
	if readErr != nil || string(gotSibling) != string(wantSibling) {
		t.Fatalf("preexisting sibling changed: got=%q err=%v", gotSibling, readErr)
	}
}

func TestSeedRejectsAnyNonDedicatedTargetBeforeNetwork(t *testing.T) {
	base := Config{
		Server: "nas:445", Share: "media", Username: "probe",
		PasswordFile: "/not/read", BasePath: SeedBasePath,
		ExpectedSHA256: DeterministicFixtureSHA256(),
	}
	badBase := base
	badBase.BasePath = "Downloads"
	if _, err := Seed(context.Background(), badBase, SeedSourceName); err == nil || !strings.Contains(err.Error(), "requires base_path") {
		t.Fatalf("unsafe base_path reached network path: %v", err)
	}
	if _, err := Seed(context.Background(), base, "E04.mkv"); err == nil || !strings.Contains(err.Error(), "requires source") {
		t.Fatalf("unsafe source reached network path: %v", err)
	}
	badHash := base
	badHash.ExpectedSHA256 = strings.Repeat("0", 64)
	if _, err := Seed(context.Background(), badHash, SeedSourceName); err == nil || !strings.Contains(err.Error(), "does not match deterministic fixture") {
		t.Fatalf("wrong deterministic hash reached network path: %v", err)
	}
	tooSmall := base
	tooSmall.MaxSourceBytes = DeterministicFixtureSize() - 1
	if _, err := Seed(context.Background(), tooSmall, SeedSourceName); err == nil || !strings.Contains(err.Error(), "smaller than deterministic fixture") {
		t.Fatalf("undersized source limit reached network path: %v", err)
	}
}

func TestRunWithShareProvesIntegrityAndNoClobber(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "probe", "input.bin")
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("navigatorr-smb-probe\x00", 4096)
	if err := os.WriteFile(fixture, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: "nas:445", Share: "media", BasePath: "probe", ExpectedSHA256: payloadSHA(payload), MaxSourceBytes: int64(len(payload)) + 1}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root})
	if err != nil {
		t.Fatalf("runWithShare: %v", err)
	}
	if !result.OK || !result.ReadbackProven || !result.NoClobberProven || !result.RenameProven || !result.CleanupComplete {
		t.Fatalf("incomplete proof: %+v", result)
	}
	if result.Bytes != int64(len(payload)) || result.SHA256 == "" {
		t.Fatalf("unexpected payload result: %+v", result)
	}
	if got, err := os.ReadFile(fixture); err != nil || string(got) != payload {
		t.Fatalf("source fixture changed: len=%d err=%v", len(got), err)
	}
}

func TestRunWithShareRejectsClobberingRename(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "probe", "input.bin")
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, []byte("safe fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: "nas:445", Share: "media", BasePath: "probe", ExpectedSHA256: payloadSHA("safe fixture"), MaxSourceBytes: 1024}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root, clobber: true})
	if err == nil || !strings.Contains(err.Error(), "unsafe SMB rename") {
		t.Fatalf("expected unsafe rename rejection, result=%+v err=%v", result, err)
	}
	if result.NoClobberProven || result.OK {
		t.Fatalf("unsafe backend must not pass: %+v", result)
	}
}

func TestRunWithShareDoesNotTreatGenericRenameErrorAsNoClobberProof(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "probe", "input.bin")
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, []byte("safe fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: "nas:445", Share: "media", BasePath: "probe", ExpectedSHA256: payloadSHA("safe fixture"), MaxSourceBytes: 1024}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root, renameErr: os.ErrPermission})
	if err == nil || !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("expected inconclusive rename rejection, result=%+v err=%v", result, err)
	}
	if result.NoClobberProven || result.OK {
		t.Fatalf("generic rename error must not pass: %+v", result)
	}
}

func TestRunWithShareRejectsUnexpectedFixtureHash(t *testing.T) {
	root := t.TempDir()
	fixture := filepath.Join(root, "probe", "input.bin")
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, []byte("corrupt fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: "nas:445", Share: "media", BasePath: "probe", ExpectedSHA256: payloadSHA("expected fixture"), MaxSourceBytes: 1024}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("expected fixture mismatch, result=%+v err=%v", result, err)
	}
}

func TestCleanRelativeRejectsTraversalAndAmbiguousSeparators(t *testing.T) {
	for _, bad := range []string{"", "/absolute", "../escape", "a/../escape", `a\b`, "a\x00b"} {
		if _, err := cleanRelative(bad, false); err == nil {
			t.Errorf("cleanRelative(%q) unexpectedly succeeded", bad)
		}
	}
	if got, err := cleanRelative("shows/fixture.mkv", false); err != nil || got != "shows/fixture.mkv" {
		t.Fatalf("valid relative path = %q, %v", got, err)
	}
}

func TestReadPasswordFileRequiresPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte("correct horse\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readPasswordFile(path); err != nil || got != "correct horse" {
		t.Fatalf("readPasswordFile = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPasswordFile(path); err == nil {
		t.Fatal("expected broad permissions to fail closed")
	}
}

type failWriteFile struct {
	writeFile
	failOnWrite bool
	failOnSync  bool
	failOnClose bool
}

func (f *failWriteFile) Write(p []byte) (int, error) {
	if f.failOnWrite {
		return 0, errors.New("simulated write error")
	}
	return f.writeFile.Write(p)
}

func (f *failWriteFile) Sync() error {
	if f.failOnSync {
		return errors.New("simulated sync error")
	}
	return f.writeFile.Sync()
}

func (f *failWriteFile) Close() error {
	closeErr := f.writeFile.Close()
	if f.failOnClose {
		return errors.New("simulated close error")
	}
	return closeErr
}

type failStepShare struct {
	share
	targetPath  string
	failOnWrite bool
	failOnSync  bool
	failOnClose bool
}

func (s failStepShare) WithContext(ctx context.Context) share {
	return failStepShare{
		share:       s.share.WithContext(ctx),
		targetPath:  s.targetPath,
		failOnWrite: s.failOnWrite,
		failOnSync:  s.failOnSync,
		failOnClose: s.failOnClose,
	}
}

func (s failStepShare) OpenExclusive(name string) (writeFile, error) {
	w, err := s.share.OpenExclusive(name)
	if err != nil {
		return nil, err
	}
	if s.targetPath == "" || name == s.targetPath {
		return &failWriteFile{
			writeFile:   w,
			failOnWrite: s.failOnWrite,
			failOnSync:  s.failOnSync,
			failOnClose: s.failOnClose,
		}, nil
	}
	return w, nil
}

func setupProbeFixture(t *testing.T) (string, Config) {
	t.Helper()
	root := t.TempDir()
	fixture := filepath.Join(root, "probe", "input.bin")
	if err := os.MkdirAll(filepath.Dir(fixture), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("navigatorr-smb-probe\x00", 4096)
	if err := os.WriteFile(fixture, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Server:         "nas:445",
		Share:          "media",
		BasePath:       "probe",
		ExpectedSHA256: payloadSHA(payload),
		MaxSourceBytes: int64(len(payload)) + 1,
	}
	return root, cfg
}

func TestRunWithSharePreservesPreexistingPartial(t *testing.T) {
	root, cfg := setupProbeFixture(t)
	const token = "fixedtokenforpartial0001"
	origToken := probeToken
	probeToken = func() (string, error) { return token, nil }
	t.Cleanup(func() { probeToken = origToken })

	partial := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token+".partial")
	partialPath := filepath.Join(root, filepath.FromSlash(partial))
	preexisting := []byte("preexisting partial data must not be touched")
	if err := os.WriteFile(partialPath, preexisting, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root})
	if err == nil {
		t.Fatal("expected runWithShare to fail on pre-existing partial file")
	}
	if result.OK {
		t.Fatalf("expected result.OK to be false, got %+v", result)
	}

	got, readErr := os.ReadFile(partialPath)
	if readErr != nil {
		t.Fatalf("pre-existing partial was deleted or unreadable: %v", readErr)
	}
	if !bytes.Equal(got, preexisting) {
		t.Fatalf("pre-existing partial corrupted: got %q, want %q", got, preexisting)
	}
}

func TestRunWithSharePreservesPreexistingCollision(t *testing.T) {
	root, cfg := setupProbeFixture(t)
	const token = "fixedtokenforcollision01"
	origToken := probeToken
	probeToken = func() (string, error) { return token, nil }
	t.Cleanup(func() { probeToken = origToken })

	collision := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token+".collision")
	collisionPath := filepath.Join(root, filepath.FromSlash(collision))
	preexisting := []byte("preexisting collision data must not be touched")
	if err := os.WriteFile(collisionPath, preexisting, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root})
	if err == nil {
		t.Fatal("expected runWithShare to fail on pre-existing collision sentinel")
	}
	if result.OK {
		t.Fatalf("expected result.OK to be false, got %+v", result)
	}

	got, readErr := os.ReadFile(collisionPath)
	if readErr != nil {
		t.Fatalf("pre-existing collision was deleted or unreadable: %v", readErr)
	}
	if !bytes.Equal(got, preexisting) {
		t.Fatalf("pre-existing collision corrupted: got %q, want %q", got, preexisting)
	}

	partial := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token+".partial")
	partialPath := filepath.Join(root, filepath.FromSlash(partial))
	if _, statErr := os.Stat(partialPath); !os.IsNotExist(statErr) {
		t.Fatalf("owned partial was not cleaned up after collision failure: %v", statErr)
	}
}

func TestRunWithSharePreservesPreexistingFinal(t *testing.T) {
	root, cfg := setupProbeFixture(t)
	const token = "fixedtokenforfinal000001"
	origToken := probeToken
	probeToken = func() (string, error) { return token, nil }
	t.Cleanup(func() { probeToken = origToken })

	final := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token+".final")
	finalPath := filepath.Join(root, filepath.FromSlash(final))
	preexisting := []byte("preexisting final data must not be touched")
	if err := os.WriteFile(finalPath, preexisting, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := runWithShare(context.Background(), cfg, "probe/input.bin", localShare{root: root})
	if err == nil {
		t.Fatal("expected runWithShare to fail when destination final already exists")
	}
	if result.OK {
		t.Fatalf("expected result.OK to be false, got %+v", result)
	}

	got, readErr := os.ReadFile(finalPath)
	if readErr != nil {
		t.Fatalf("pre-existing final was deleted or unreadable: %v", readErr)
	}
	if !bytes.Equal(got, preexisting) {
		t.Fatalf("pre-existing final corrupted: got %q, want %q", got, preexisting)
	}

	partial := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token+".partial")
	partialPath := filepath.Join(root, filepath.FromSlash(partial))
	if _, statErr := os.Stat(partialPath); !os.IsNotExist(statErr) {
		t.Fatalf("owned partial was not cleaned up after final rename failure: %v", statErr)
	}
}

func TestRunWithShareCleansCreatedArtifactOnFailure(t *testing.T) {
	cases := []struct {
		name         string
		failOnWrite  bool
		failOnSync   bool
		failOnClose  bool
		errSubstring string
	}{
		{
			name:         "copy write failure",
			failOnWrite:  true,
			errSubstring: "simulated write error",
		},
		{
			name:         "sync failure",
			failOnSync:   true,
			errSubstring: "simulated sync error",
		},
		{
			name:         "close failure",
			failOnClose:  true,
			errSubstring: "simulated close error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, cfg := setupProbeFixture(t)
			const token = "failtesttoken12345678901"
			origToken := probeToken
			probeToken = func() (string, error) { return token, nil }
			t.Cleanup(func() { probeToken = origToken })

			partial := path.Join(cfg.BasePath, ".navigatorr-smb-probe-"+token+".partial")
			partialPath := filepath.Join(root, filepath.FromSlash(partial))

			fs := failStepShare{
				share:       localShare{root: root},
				targetPath:  partial,
				failOnWrite: tc.failOnWrite,
				failOnSync:  tc.failOnSync,
				failOnClose: tc.failOnClose,
			}

			result, err := runWithShare(context.Background(), cfg, "probe/input.bin", fs)
			if err == nil || !strings.Contains(err.Error(), tc.errSubstring) {
				t.Fatalf("expected error containing %q, got err=%v", tc.errSubstring, err)
			}
			if result.OK {
				t.Fatalf("expected result.OK to be false, got %+v", result)
			}
			if !result.CleanupComplete || len(result.CleanupArtifacts) > 0 {
				t.Fatalf("expected cleanup to be complete, got %+v", result)
			}
			if _, statErr := os.Stat(partialPath); !os.IsNotExist(statErr) {
				t.Fatalf("failed partial file was not cleaned up: %v", statErr)
			}
		})
	}
}

func TestWriteExclusiveOwnershipContract(t *testing.T) {
	root := t.TempDir()
	fs := localShare{root: root}
	ctx := context.Background()

	created, err := writeExclusive(ctx, fs, "success.bin", strings.NewReader("content"))
	if !created || err != nil {
		t.Fatalf("expected (true, nil), got (%v, %v)", created, err)
	}

	created, err = writeExclusive(ctx, fs, "success.bin", strings.NewReader("content"))
	if created || err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected (false, os.ErrExist), got (%v, %v)", created, err)
	}

	failFs := failStepShare{share: localShare{root: root}, failOnWrite: true}
	created, err = writeExclusive(ctx, failFs, "fail-write.bin", strings.NewReader("content"))
	if !created || err == nil || !strings.Contains(err.Error(), "simulated write error") {
		t.Fatalf("expected (true, simulated write error), got (%v, %v)", created, err)
	}

	failSyncFs := failStepShare{share: localShare{root: root}, failOnSync: true}
	created, err = writeExclusive(ctx, failSyncFs, "fail-sync.bin", strings.NewReader("content"))
	if !created || err == nil || !strings.Contains(err.Error(), "simulated sync error") {
		t.Fatalf("expected (true, simulated sync error), got (%v, %v)", created, err)
	}

	failCloseFs := failStepShare{share: localShare{root: root}, failOnClose: true}
	created, err = writeExclusive(ctx, failCloseFs, "fail-close.bin", strings.NewReader("content"))
	if !created || err == nil || !strings.Contains(err.Error(), "simulated close error") {
		t.Fatalf("expected (true, simulated close error), got (%v, %v)", created, err)
	}
}

func TestUploadExclusiveLocalSourceOpenFailure(t *testing.T) {
	root := t.TempDir()
	fs := localShare{root: root}
	created, err := uploadExclusive(context.Background(), fs, "dest.bin", filepath.Join(root, "nonexistent.bin"))
	if created || err == nil || !os.IsNotExist(err) {
		t.Fatalf("expected (false, os.ErrNotExist), got (%v, %v)", created, err)
	}
}
