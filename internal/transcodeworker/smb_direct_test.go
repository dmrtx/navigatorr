package transcodeworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeDirectStore struct {
	root      string
	source    []byte
	published int
	publishTo string
}

func (s *fakeDirectStore) Maps(name string) bool {
	clean := filepath.Clean(name)
	return clean != s.root && strings.HasPrefix(clean, s.root+string(filepath.Separator))
}

func (s *fakeDirectStore) Stat(context.Context, string) (os.FileInfo, error) {
	return directInfo{size: int64(len(s.source))}, nil
}

func (s *fakeDirectStore) DownloadAtomic(_ context.Context, _ string, local string) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	return os.WriteFile(local, s.source, 0o600)
}

func (s *fakeDirectStore) Publish(_ context.Context, local, destination, _ string) error {
	if _, err := os.ReadFile(local); err != nil {
		return err
	}
	s.published++
	s.publishTo = destination
	return nil
}

func (s *fakeDirectStore) CheckRoot(context.Context) error { return nil }

type directInfo struct{ size int64 }

func (directInfo) Name() string       { return "source.mkv" }
func (i directInfo) Size() int64      { return i.size }
func (directInfo) Mode() os.FileMode  { return 0o600 }
func (directInfo) ModTime() time.Time { return time.Time{} }
func (directInfo) IsDir() bool        { return false }
func (directInfo) Sys() any           { return nil }

func TestSMBDirectConfigLoadsFailClosed(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	configPath := filepath.Join(dir, "config.yaml")
	yaml := fmt.Sprintf(`state_dir: %q
allowed_roots: [%q]
external_roots: [%q]
local_work_dir: %q
smb_direct:
  enabled: true
  local_root: %q
  server: 192.168.70.72
  share: media
  username: tdarr
  password_file: %q
  timeout: 20m
`, filepath.Join(dir, "state"), root, root, filepath.Join(dir, "work"), root, filepath.Join(dir, "secret"))
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWorkerConfig(configPath)
	if err != nil {
		t.Fatalf("LoadWorkerConfig: %v", err)
	}
	if !cfg.SMBDirect.Enabled || cfg.SMBDirect.Server != "192.168.70.72:445" || cfg.SMBDirect.Timeout != 20*time.Minute {
		t.Fatalf("SMB config not normalized: %#v", cfg.SMBDirect)
	}

	yaml = strings.Replace(yaml, fmt.Sprintf("local_root: %q", root), `local_root: "/outside"`, 1)
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkerConfig(configPath); err == nil {
		t.Fatal("LoadWorkerConfig accepted smb_direct.local_root outside allowed_roots")
	}
}

func TestSMBDirectStagesAndFinalizesThroughMediaStore(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "unmounted-media")
	stateDir := filepath.Join(dir, "state")
	workDir := filepath.Join(dir, "work")
	store := &fakeDirectStore{root: root, source: []byte("source-from-smb")}
	w := NewWorker(&WorkerConfig{
		StateDir: stateDir, AllowedRoots: []string{root}, ExternalRoots: []string{root},
		LocalWorkDir: workDir, StagingPolicy: StagingPolicyAuto,
	})
	w.mediaStore = store

	job := &JobRecord{
		ID: "job-smb", Status: "running", Source: filepath.Join(root, "TV", "source.mkv"),
		Candidate: filepath.Join(root, "TV", "candidate.mkv"), EncodeComplete: true,
		ExecutionSpecDigest: pr5Digest,
		StagingState:        string(StagingStatePending), FinalizationState: string(FinalizationStatePending),
		StagedInputPath:     filepath.Join(workDir, "job-smb", "input.mkv"),
		EffectiveInputPath:  filepath.Join(workDir, "job-smb", "input.mkv"),
		LocalCandidatePath:  filepath.Join(workDir, "job-smb", "candidate.mkv"),
		IntendedDestination: filepath.Join(root, "TV", "candidate.mkv"),
		PartialPath:         PartialPathFor(filepath.Join(root, "TV", "candidate.mkv"), "job-smb"),
	}
	jobDir := filepath.Join(stateDir, job.ID)
	jobFile := filepath.Join(jobDir, "job.json")
	if err := SaveJobAtomic(jobFile, job); err != nil {
		t.Fatal(err)
	}
	r, err := resolveOperationalForExecution(job)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, err := w.ensureStaged(context.Background(), jobDir, jobFile, job, r); err != nil || cancelled {
		t.Fatalf("ensureStaged: cancelled=%v err=%v", cancelled, err)
	}
	if got, err := os.ReadFile(r.stagedInput); err != nil || string(got) != "source-from-smb" {
		t.Fatalf("staged input = %q, %v", got, err)
	}
	if err := os.WriteFile(r.localCandidate, []byte("encoded-candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.finalizeOperational(context.Background(), jobDir, jobFile, job, r); err != nil {
		t.Fatalf("finalizeOperational: %v", err)
	}
	if store.published != 1 || store.publishTo != job.IntendedDestination {
		t.Fatalf("publish calls=%d destination=%q", store.published, store.publishTo)
	}
	got, err := LoadJob(jobFile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.FinalizationState != string(FinalizationStateCompleted) {
		t.Fatalf("final job = %#v", got)
	}
	if _, err := os.Stat(r.localCandidate); !os.IsNotExist(err) {
		t.Fatalf("local candidate was not cleaned: %v", err)
	}
}
