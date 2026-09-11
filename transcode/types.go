package transcode

import (
	"time"
)

// Status constants for transcode jobs.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Request defines the input for submitting a transcode job.
type Request struct {
	ID            string `json:"id"`
	SourcePath    string `json:"source_path"`
	CandidatePath string `json:"candidate_path"`
	Profile       string `json:"profile"`
}

// Job represents a submitted transcode job.
type Job struct {
	ID string `json:"id"`
}

// JobStatus describes the current status and metrics of a transcode job.
type JobStatus struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	Progress      float64 `json:"progress"`
	FPS           float64 `json:"fps"`
	Speed         float64 `json:"speed"`
	CandidatePath string  `json:"candidate_path"`
	Error         string  `json:"error,omitempty"`
}

// PathMapping specifies a pair of local and remote directory paths.
type PathMapping struct {
	Local  string `json:"local" yaml:"local"`
	Remote string `json:"remote" yaml:"remote"`
}

// SSHConfig holds configuration for the SSH transcode executor.
type SSHConfig struct {
	Host           string        `json:"host" yaml:"host"`
	User           string        `json:"user" yaml:"user"`
	Command        string        `json:"command" yaml:"command"`
	IdentityFile   string        `json:"identity_file" yaml:"identity_file"`
	ConnectTimeout time.Duration `json:"connect_timeout" yaml:"connect_timeout"`
	PathMappings   []PathMapping `json:"path_mappings" yaml:"path_mappings"`
}
