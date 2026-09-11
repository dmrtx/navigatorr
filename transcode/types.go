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
	Port           int           `json:"port,omitempty" yaml:"port,omitempty"`
	User           string        `json:"user" yaml:"user"`
	Command        string        `json:"command" yaml:"command"`
	IdentityFile   string        `json:"identity_file" yaml:"identity_file"`
	KnownHostsFile string        `json:"known_hosts_file,omitempty" yaml:"known_hosts_file,omitempty"`
	ConnectTimeout time.Duration `json:"connect_timeout" yaml:"connect_timeout"`
	CommandTimeout time.Duration `json:"command_timeout,omitempty" yaml:"command_timeout,omitempty"`
	PathMappings   []PathMapping `json:"path_mappings" yaml:"path_mappings"`
}
