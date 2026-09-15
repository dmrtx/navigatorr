package transcodeworker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
)

// JobRecord represents the persistent state stored in job.json on the worker.
type JobRecord struct {
	ID                  string                       `json:"id"`
	Status              string                       `json:"status"` // queued, running, completed, failed, cancelled
	Source              string                       `json:"source"`
	Candidate           string                       `json:"candidate"`
	Profile             string                       `json:"profile"`
	Plan                *transcode.Plan              `json:"plan,omitempty"`
	IdempotencyKey      string                       `json:"idempotency_key,omitempty"`
	ExecutionSpecDigest string                       `json:"execution_spec_digest,omitempty"`
	Conversions         []transcode.ConversionRecord `json:"conversions,omitempty"`
	PID                 int                          `json:"pid"`
	ProcessStartTime    string                       `json:"process_start_time,omitempty"`
	CreatedAt           time.Time                    `json:"created_at"`
	StartedAt           time.Time                    `json:"started_at,omitempty"`
	FinishedAt          time.Time                    `json:"finished_at,omitempty"`
	ExitCode            int                          `json:"exit_code,omitempty"`
	Error               string                       `json:"error,omitempty"`
	DurationSec         float64                      `json:"duration_sec,omitempty"`
	// Minimal transport metadata (no retry engine): mirrors transcode.JobStatus.
	Attempt               int      `json:"attempt,omitempty"`
	RetryCount            int      `json:"retry_count,omitempty"`
	FallbackCount         int      `json:"fallback_count,omitempty"`
	AppliedFallbacks      []string `json:"applied_fallbacks,omitempty"`
	FailureClassification string   `json:"failure_classification,omitempty"`

	// Operational storage metadata (Phase 6B1). These fields are purely
	// operational: they never participate in the transcode Plan or the
	// execution-spec digest, are omitempty/backward compatible, and are
	// initialized only when a NEW job is persisted. Blanks on legacy records
	// mean unspecified; helper methods infer safe defaults without mutating
	// the semantic Source/Candidate.
	StagingPolicy       string `json:"staging_policy,omitempty"`
	StagingState        string `json:"staging_state,omitempty"`
	EffectiveInputPath  string `json:"effective_input_path,omitempty"`
	StagedInputPath     string `json:"staged_input_path,omitempty"`
	LocalCandidatePath  string `json:"local_candidate_path,omitempty"`
	IntendedDestination string `json:"intended_destination,omitempty"`
	EncodeComplete      bool   `json:"encode_complete,omitempty"`
	FinalizationState   string `json:"finalization_state,omitempty"`
	PartialPath         string `json:"partial_path,omitempty"`
}

// LoadJob loads a JobRecord from job.json.
func LoadJob(path string) (*JobRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading job file %s: %w", path, err)
	}
	var job JobRecord
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshaling job file %s: %w", path, err)
	}
	return &job, nil
}

// SaveJobAtomic saves a JobRecord to job.json atomically using a temp file and rename.
func SaveJobAtomic(path string, job *JobRecord) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating job directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling job %s: %w", job.ID, err)
	}

	tmpFile, err := os.CreateTemp(dir, "job-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp job file in %s: %w", dir, err)
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp job file %s: %w", tmpName, err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp job file %s: %w", tmpName, err)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp job file %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp file %s to %s: %w", tmpName, path, err)
	}

	return nil
}

// TerminalMarker is a durable completion/failure attestation written after a
// job reaches a terminal state. It is used at startup reconciliation to tell a
// genuine terminal transition apart from an interrupted runner.
type TerminalMarker struct {
	JobID                 string    `json:"job_id"`
	ExecutionSpecDigest   string    `json:"execution_spec_digest"`
	Status                string    `json:"status"`
	FinishedAt            time.Time `json:"finished_at"`
	ExitCode              int       `json:"exit_code,omitempty"`
	Error                 string    `json:"error,omitempty"`
	FailureClassification string    `json:"failure_classification,omitempty"`
}

// isTerminalStatus reports whether status is a terminal job status.
func isTerminalStatus(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// SaveTerminalMarkerAtomic validates and atomically persists a TerminalMarker
// using a temp file + rename. Rejects nil markers, blank job ID/digest,
// nonterminal status, and zero FinishedAt.
func SaveTerminalMarkerAtomic(path string, marker *TerminalMarker) error {
	if marker == nil {
		return fmt.Errorf("saving terminal marker: nil marker")
	}
	if strings.TrimSpace(marker.JobID) == "" {
		return fmt.Errorf("saving terminal marker: blank job_id")
	}
	if strings.TrimSpace(marker.ExecutionSpecDigest) == "" {
		return fmt.Errorf("saving terminal marker %s: blank execution_spec_digest", marker.JobID)
	}
	if !isTerminalStatus(marker.Status) {
		return fmt.Errorf("saving terminal marker %s: nonterminal status %q", marker.JobID, marker.Status)
	}
	if marker.FinishedAt.IsZero() {
		return fmt.Errorf("saving terminal marker %s: zero finished_at", marker.JobID)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating terminal marker directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling terminal marker %s: %w", marker.JobID, err)
	}

	tmpFile, err := os.CreateTemp(dir, "terminal-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp terminal marker in %s: %w", dir, err)
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing temp terminal marker %s: %w", tmpName, err)
	}

	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing temp terminal marker %s: %w", tmpName, err)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temp terminal marker %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming temp terminal marker %s to %s: %w", tmpName, path, err)
	}

	return nil
}

// LoadTerminalMarker reads and validates a TerminalMarker, rejecting blank job
// ID/digest, nonterminal status, and zero or missing FinishedAt.
func LoadTerminalMarker(path string) (*TerminalMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading terminal marker %s: %w", path, err)
	}
	var marker TerminalMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil, fmt.Errorf("unmarshaling terminal marker %s: %w", path, err)
	}
	if strings.TrimSpace(marker.JobID) == "" {
		return nil, fmt.Errorf("terminal marker %s: blank job_id", path)
	}
	if strings.TrimSpace(marker.ExecutionSpecDigest) == "" {
		return nil, fmt.Errorf("terminal marker %s: blank execution_spec_digest", path)
	}
	if !isTerminalStatus(marker.Status) {
		return nil, fmt.Errorf("terminal marker %s: nonterminal status %q", path, marker.Status)
	}
	if marker.FinishedAt.IsZero() {
		return nil, fmt.Errorf("terminal marker %s: zero finished_at", path)
	}
	return &marker, nil
}

// IsProcessAlive checks whether a process with the given PID is running.
func IsProcessAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests if the process exists without actually sending a signal
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// GetProcessIdentity returns the command line and start time (lstart) for a PID on Unix/macOS.
func GetProcessIdentity(pid int) (command string, startTime string, err error) {
	if pid <= 1 {
		return "", "", fmt.Errorf("invalid pid %d", pid)
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=,command=").Output()
	if err != nil {
		return "", "", err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", "", fmt.Errorf("no process found for pid %d", pid)
	}
	parts := strings.Fields(raw)
	if len(parts) >= 5 {
		// "Fri Sep 11 13:48:47 2026 /path/to/cmd ..."
		startTime = strings.Join(parts[:5], " ")
		if len(parts) > 5 {
			command = strings.Join(parts[5:], " ")
		}
	} else {
		command = raw
	}
	return command, startTime, nil
}

// GetProcessTokens returns the whitespace-tokenized argument fields and start time (lstart) for a PID on Unix/macOS.
func GetProcessTokens(pid int) (tokens []string, startTime string, err error) {
	if pid <= 1 {
		return nil, "", fmt.Errorf("invalid pid %d", pid)
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=,command=").Output()
	if err != nil {
		return nil, "", err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, "", fmt.Errorf("no process found for pid %d", pid)
	}
	parts := strings.Fields(raw)
	if len(parts) >= 5 {
		startTime = strings.Join(parts[:5], " ")
		if len(parts) > 5 {
			tokens = parts[5:]
		}
	} else {
		tokens = parts
	}
	return tokens, startTime, nil
}

// IsJobProcessAlive checks whether the process for a specific job is alive and matches the job's identity.
// It protects against PID recycling by verifying:
// 1. The OS signal check passes.
// 2. If ProcessStartTime was recorded, the current process's start time must match.
// 3. If the process command line contains "_internal_run", it must match the specific job ID.
func IsJobProcessAlive(job *JobRecord) bool {
	if job == nil || job.PID <= 1 {
		return false
	}
	if !IsProcessAlive(job.PID) {
		return false
	}
	cmd, startTime, err := GetProcessIdentity(job.PID)
	if err != nil {
		return false
	}
	if job.ProcessStartTime != "" && startTime != "" && job.ProcessStartTime != startTime {
		return false
	}
	if strings.Contains(cmd, "_internal_run") && !strings.Contains(cmd, job.ID) {
		return false
	}
	return true
}

// ProgressMetrics contains parsed progress metrics from FFmpeg's progress.txt.
type ProgressMetrics struct {
	Progress float64
	FPS      float64
	Speed    float64
}

// ParseProgress parses FFmpeg's key=value progress output.
func ParseProgress(progressPath string, durationSec float64) ProgressMetrics {
	f, err := os.Open(progressPath)
	if err != nil {
		return ProgressMetrics{}
	}
	defer f.Close()

	var (
		fps         float64
		speed       float64
		outTimeSec  float64
		progressEnd bool
	)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "fps":
			if v, err := strconv.ParseFloat(val, 64); err == nil {
				fps = v
			}
		case "speed":
			// E.g. "  5.4x" or "5.4x" or "N/A"
			valClean := strings.TrimSpace(strings.TrimSuffix(val, "x"))
			if v, err := strconv.ParseFloat(valClean, 64); err == nil {
				speed = v
			}
		case "out_time_us":
			// out_time_us is microseconds
			if v, err := strconv.ParseInt(val, 10, 64); err == nil && v > 0 {
				outTimeSec = float64(v) / 1000000.0
			}
		case "out_time_ms":
			// In FFmpeg, out_time_ms is historically microseconds as well
			if outTimeSec == 0 {
				if v, err := strconv.ParseInt(val, 10, 64); err == nil && v > 0 {
					outTimeSec = float64(v) / 1000000.0
				}
			}
		case "progress":
			if val == "end" {
				progressEnd = true
			}
		}
	}

	var pct float64
	if progressEnd {
		pct = 100.0
	} else if durationSec > 0 && outTimeSec > 0 {
		pct = math.Min(99.9, (outTimeSec/durationSec)*100.0)
	}

	return ProgressMetrics{
		Progress: math.Round(pct*10) / 10,
		FPS:      fps,
		Speed:    speed,
	}
}
