package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var validJobIDRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// SSHExecutorOption allows configuring the SSHExecutor.
type SSHExecutorOption func(*SSHExecutor)

// WithSSHBinary overrides the OpenSSH binary path (useful for testing).
func WithSSHBinary(bin string) SSHExecutorOption {
	return func(e *SSHExecutor) {
		e.sshBinary = bin
	}
}

// SSHExecutor implements Executor using OpenSSH client and remote navigatorr-transcode.
type SSHExecutor struct {
	cfg       SSHConfig
	sshBinary string
}

// NewSSHExecutor creates a new SSH transcode executor.
func NewSSHExecutor(cfg SSHConfig, opts ...SSHExecutorOption) (*SSHExecutor, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("ssh host is required")
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("remote navigatorr-transcode command path is required")
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 5 * time.Second
	}

	exec := &SSHExecutor{
		cfg:       cfg,
		sshBinary: "ssh",
	}
	for _, opt := range opts {
		opt(exec)
	}
	return exec, nil
}

// TranslateLocalToRemote translates a local filesystem path to the remote worker's path.
// It uses longest-prefix matching and fails closed if no matching mapping is found.
func (e *SSHExecutor) TranslateLocalToRemote(localPath string) (string, error) {
	if len(e.cfg.PathMappings) == 0 {
		return "", fmt.Errorf("no path mappings configured for transcode executor (fail closed)")
	}

	cleanLocal := filepath.Clean(localPath)

	// Sort mappings by local prefix length descending for longest-prefix match
	mappings := make([]PathMapping, len(e.cfg.PathMappings))
	copy(mappings, e.cfg.PathMappings)
	sort.Slice(mappings, func(i, j int) bool {
		return len(filepath.Clean(mappings[i].Local)) > len(filepath.Clean(mappings[j].Local))
	})

	for _, m := range mappings {
		mappedLocal := filepath.Clean(m.Local)
		if mappedLocal == "" || m.Remote == "" {
			continue
		}

		if cleanLocal == mappedLocal {
			return filepath.Clean(m.Remote), nil
		}

		prefixWithSep := mappedLocal + string(filepath.Separator)
		if strings.HasPrefix(cleanLocal, prefixWithSep) {
			rel := strings.TrimPrefix(cleanLocal, mappedLocal)
			rel = strings.TrimPrefix(rel, string(filepath.Separator))
			return filepath.ToSlash(filepath.Join(m.Remote, filepath.ToSlash(rel))), nil
		}
	}

	return "", fmt.Errorf("path %q does not match any configured transcode path mappings (fail closed)", localPath)
}

// TranslateRemoteToLocal translates a remote worker path back to the local filesystem path.
// It uses longest-prefix matching and fails closed if no matching mapping is found.
func (e *SSHExecutor) TranslateRemoteToLocal(remotePath string) (string, error) {
	if len(e.cfg.PathMappings) == 0 {
		return "", fmt.Errorf("no path mappings configured for transcode executor (fail closed)")
	}

	cleanRemote := filepath.ToSlash(filepath.Clean(remotePath))

	// Sort mappings by remote prefix length descending for longest-prefix match
	mappings := make([]PathMapping, len(e.cfg.PathMappings))
	copy(mappings, e.cfg.PathMappings)
	sort.Slice(mappings, func(i, j int) bool {
		return len(filepath.ToSlash(filepath.Clean(mappings[i].Remote))) > len(filepath.ToSlash(filepath.Clean(mappings[j].Remote)))
	})

	for _, m := range mappings {
		mappedRemote := filepath.ToSlash(filepath.Clean(m.Remote))
		if mappedRemote == "" || m.Local == "" {
			continue
		}

		if cleanRemote == mappedRemote {
			return filepath.Clean(m.Local), nil
		}

		prefixWithSlash := mappedRemote + "/"
		if strings.HasPrefix(cleanRemote, prefixWithSlash) {
			rel := strings.TrimPrefix(cleanRemote, mappedRemote)
			rel = strings.TrimPrefix(rel, "/")
			return filepath.Clean(filepath.Join(m.Local, rel)), nil
		}
	}

	return "", fmt.Errorf("remote path %q does not match any configured transcode path mappings (fail closed)", remotePath)
}

func (e *SSHExecutor) buildSSHArgs() []string {
	timeoutSec := int(e.cfg.ConnectTimeout.Seconds())
	if timeoutSec <= 0 {
		timeoutSec = 5
	}

	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeoutSec),
	}
	if e.cfg.Port > 0 {
		args = append(args, "-p", strconv.Itoa(e.cfg.Port))
	}
	if e.cfg.KnownHostsFile != "" {
		args = append(args, "-o", fmt.Sprintf("UserKnownHostsFile=%s", e.cfg.KnownHostsFile))
	}
	if e.cfg.IdentityFile != "" {
		args = append(args, "-i", e.cfg.IdentityFile)
	}
	if e.cfg.User != "" {
		args = append(args, "-l", e.cfg.User)
	}
	args = append(args, e.cfg.Host)
	return args
}

// Doctor executes the remote diagnostic check.
func (e *SSHExecutor) Doctor(ctx context.Context) error {
	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "doctor")

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		outStr := strings.TrimSpace(stdout.String())
		errStr := strings.TrimSpace(stderr.String())
		return fmt.Errorf("ssh doctor failed: %w (stderr: %s, stdout: %s)", err, errStr, outStr)
	}

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("failed to parse doctor response: %w (output: %s)", err, stdout.String())
	}
	if !resp.OK {
		return fmt.Errorf("remote doctor check failed: %s", resp.Error)
	}
	return nil
}

// Capabilities fetches and verifies the versioned capability report from the remote worker node.
func (e *SSHExecutor) Capabilities(ctx context.Context) (WorkerCapabilities, error) {
	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "capabilities")

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		outStr := strings.TrimSpace(stdout.String())
		errStr := strings.TrimSpace(stderr.String())
		return WorkerCapabilities{}, fmt.Errorf("ssh capabilities failed: %w (stderr: %s, stdout: %s)", err, errStr, outStr)
	}

	var caps WorkerCapabilities
	if err := json.Unmarshal(stdout.Bytes(), &caps); err != nil {
		return WorkerCapabilities{}, fmt.Errorf("failed to parse capabilities response: %w (output: %s)", err, stdout.String())
	}

	if caps.ProtocolVersion != WorkerProtocolVersion {
		return WorkerCapabilities{}, fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)", caps.ProtocolVersion, WorkerProtocolVersion)
	}

	if err := VerifyCapabilityFingerprint(caps); err != nil {
		return WorkerCapabilities{}, err
	}

	return caps, nil
}

// Submit sends a transcode request to the remote worker.
func (e *SSHExecutor) Submit(ctx context.Context, req Request) (Job, error) {
	if strings.TrimSpace(req.ID) == "" {
		return Job{}, errors.New("request id is required")
	}
	if !validJobIDRegex.MatchString(req.ID) {
		return Job{}, fmt.Errorf("invalid request id %q (allowed: alphanumeric, underscore, dash, dot)", req.ID)
	}

	remoteSource, err := e.TranslateLocalToRemote(req.SourcePath)
	if err != nil {
		return Job{}, fmt.Errorf("translating source path: %w", err)
	}

	remoteCandidate, err := e.TranslateLocalToRemote(req.CandidatePath)
	if err != nil {
		return Job{}, fmt.Errorf("translating candidate path: %w", err)
	}

	profile := req.Profile
	if strings.TrimSpace(profile) == "" {
		profile = "hevc-vt"
	}

	payload := map[string]any{
		"id":             req.ID,
		"source_path":    remoteSource,
		"candidate_path": remoteCandidate,
		"profile":        profile,
	}
	if req.Plan != nil {
		payload["plan"] = req.Plan
	}
	inputData, err := json.Marshal(payload)
	if err != nil {
		return Job{}, fmt.Errorf("serializing submit payload: %w", err)
	}

	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "submit")

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	cmd.Stdin = bytes.NewReader(inputData)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return Job{}, fmt.Errorf("ssh submit failed: %w (stderr: %s, stdout: %s)", err, stderr.String(), stdout.String())
	}

	var resp struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return Job{}, fmt.Errorf("failed to parse submit response: %w (output: %s)", err, stdout.String())
	}
	if resp.Error != "" {
		return Job{}, fmt.Errorf("worker rejected submit: %s", resp.Error)
	}
	if resp.ID == "" {
		resp.ID = req.ID
	}

	return Job{ID: resp.ID}, nil
}

// Status checks the status of a transcode job.
func (e *SSHExecutor) Status(ctx context.Context, jobID string) (JobStatus, error) {
	if strings.TrimSpace(jobID) == "" {
		return JobStatus{}, errors.New("jobID is required")
	}
	if !validJobIDRegex.MatchString(jobID) {
		return JobStatus{}, fmt.Errorf("invalid jobID %q", jobID)
	}

	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "status", jobID)

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return JobStatus{}, fmt.Errorf("ssh status failed: %w (stderr: %s, stdout: %s)", err, stderr.String(), stdout.String())
	}

	var st JobStatus
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		return JobStatus{}, fmt.Errorf("failed to parse status response: %w (output: %s)", err, stdout.String())
	}

	// Translate remote candidate path back to local path if present
	if st.CandidatePath != "" {
		if localCandidate, err := e.TranslateRemoteToLocal(st.CandidatePath); err == nil {
			st.CandidatePath = localCandidate
		}
	}

	return st, nil
}

// Cancel terminates a transcode job on the remote worker.
func (e *SSHExecutor) Cancel(ctx context.Context, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("jobID is required")
	}
	if !validJobIDRegex.MatchString(jobID) {
		return fmt.Errorf("invalid jobID %q", jobID)
	}

	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "cancel", jobID)

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh cancel failed: %w (stderr: %s, stdout: %s)", err, stderr.String(), stdout.String())
	}

	var resp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("failed to parse cancel response: %w (output: %s)", err, stdout.String())
	}
	if resp.Error != "" {
		return fmt.Errorf("remote cancel error: %s", resp.Error)
	}
	return nil
}

// BenchmarkSubmit sends a benchmark request to the remote worker.
func (e *SSHExecutor) BenchmarkSubmit(ctx context.Context, req BenchmarkRequest) (BenchmarkJob, error) {
	if err := ValidateBenchmarkRequest(&req); err != nil {
		return BenchmarkJob{}, fmt.Errorf("validating benchmark request: %w", err)
	}

	remoteSource, err := e.TranslateLocalToRemote(req.SourcePath)
	if err != nil {
		return BenchmarkJob{}, fmt.Errorf("translating source path: %w", err)
	}

	cp := req
	cp.SourcePath = remoteSource

	inputData, err := json.Marshal(cp)
	if err != nil {
		return BenchmarkJob{}, fmt.Errorf("serializing benchmark submit payload: %w", err)
	}

	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "benchmark_submit")

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	cmd.Stdin = bytes.NewReader(inputData)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		outStr := strings.TrimSpace(stdout.String())
		errStr := strings.TrimSpace(stderr.String())
		return BenchmarkJob{}, fmt.Errorf("ssh benchmark_submit failed: %w (stderr: %s, stdout: %s)", err, errStr, outStr)
	}

	var resp BenchmarkSubmitResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return BenchmarkJob{}, fmt.Errorf("failed to parse benchmark submit response: %w (output: %s)", err, stdout.String())
	}
	if resp.ProtocolVersion != WorkerProtocolVersion {
		return BenchmarkJob{}, fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)",
			resp.ProtocolVersion, WorkerProtocolVersion)
	}
	if resp.Error != "" {
		return BenchmarkJob{}, fmt.Errorf("worker rejected benchmark submit: %s", resp.Error)
	}
	if resp.ID == "" {
		resp.ID = req.ID
	}

	return BenchmarkJob{ID: resp.ID}, nil
}

// BenchmarkStatus queries the status of a benchmark job on the remote worker.
func (e *SSHExecutor) BenchmarkStatus(ctx context.Context, jobID string) (BenchmarkStatus, error) {
	trimmedID := strings.TrimSpace(jobID)
	if trimmedID == "" {
		return BenchmarkStatus{}, errors.New("jobID is required")
	}
	if !validBenchmarkJobIDRegex.MatchString(trimmedID) {
		return BenchmarkStatus{}, fmt.Errorf("invalid benchmark jobID %q", jobID)
	}

	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "benchmark_status", trimmedID)

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		outStr := strings.TrimSpace(stdout.String())
		errStr := strings.TrimSpace(stderr.String())
		return BenchmarkStatus{}, fmt.Errorf("ssh benchmark_status failed: %w (stderr: %s, stdout: %s)", err, errStr, outStr)
	}

	var st BenchmarkStatus
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		return BenchmarkStatus{}, fmt.Errorf("failed to parse benchmark status response: %w (output: %s)", err, stdout.String())
	}
	if st.ProtocolVersion != WorkerProtocolVersion {
		return BenchmarkStatus{}, fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)",
			st.ProtocolVersion, WorkerProtocolVersion)
	}

	if st.SourcePath != "" {
		if localSource, err := e.TranslateRemoteToLocal(st.SourcePath); err == nil {
			st.SourcePath = localSource
		}
	}

	return st, nil
}

// BenchmarkCancel cancels an active benchmark job on the remote worker.
func (e *SSHExecutor) BenchmarkCancel(ctx context.Context, jobID string) error {
	trimmedID := strings.TrimSpace(jobID)
	if trimmedID == "" {
		return errors.New("jobID is required")
	}
	if !validBenchmarkJobIDRegex.MatchString(trimmedID) {
		return fmt.Errorf("invalid benchmark jobID %q", jobID)
	}

	args := e.buildSSHArgs()
	args = append(args, e.cfg.Command, "benchmark_cancel", trimmedID)

	cmd := exec.CommandContext(ctx, e.sshBinary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		outStr := strings.TrimSpace(stdout.String())
		errStr := strings.TrimSpace(stderr.String())
		return fmt.Errorf("ssh benchmark_cancel failed: %w (stderr: %s, stdout: %s)", err, errStr, outStr)
	}

	var resp BenchmarkCancelResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("failed to parse benchmark cancel response: %w (output: %s)", err, stdout.String())
	}
	if resp.ProtocolVersion != WorkerProtocolVersion {
		return fmt.Errorf("worker returned unsupported protocol version %d (expected %d) (fail closed)",
			resp.ProtocolVersion, WorkerProtocolVersion)
	}
	if resp.Error != "" {
		return fmt.Errorf("remote benchmark cancel error: %s", resp.Error)
	}
	return nil
}
