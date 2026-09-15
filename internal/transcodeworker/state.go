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
