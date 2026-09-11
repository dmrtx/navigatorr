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

// Plan specifies structured, safe parameters for media transcoding.
// Values are strictly enumerated and validated; arbitrary FFmpeg arguments are never permitted.
type Plan struct {
	Container                    string `json:"container" yaml:"container"`
	VideoCodec                   string `json:"video_codec" yaml:"video_codec"`
	Quality                      int    `json:"quality" yaml:"quality"`
	AudioMode                    string `json:"audio_mode" yaml:"audio_mode"`
	SubtitleMode                 string `json:"subtitle_mode" yaml:"subtitle_mode"`
	ConvertIncompatibleSubtitles bool   `json:"convert_incompatible_subtitles" yaml:"convert_incompatible_subtitles"`
	PreserveMetadata             bool   `json:"preserve_metadata" yaml:"preserve_metadata"`
	PreserveChapters             bool   `json:"preserve_chapters" yaml:"preserve_chapters"`
	PreserveAttachments          bool   `json:"preserve_attachments" yaml:"preserve_attachments"`
}

// ConversionRecord captures an explicit format conversion performed on a stream for container compatibility.
type ConversionRecord struct {
	StreamType  string `json:"stream_type"`
	StreamIndex int    `json:"stream_index"`
	FromCodec   string `json:"from_codec"`
	ToCodec     string `json:"to_codec"`
	Reason      string `json:"reason"`
}

// ContainerExtension returns the canonical filesystem extension (including dot) for a container format.
func ContainerExtension(container string) string {
	switch container {
	case "mkv", "matroska":
		return ".mkv"
	case "mp4":
		return ".mp4"
	default:
		return ".mkv"
	}
}

// Request defines the input for submitting a transcode job.
type Request struct {
	ID            string `json:"id"`
	SourcePath    string `json:"source_path"`
	CandidatePath string `json:"candidate_path"`
	Profile       string `json:"profile"`
	Plan          *Plan  `json:"plan,omitempty"`
}

// Job represents a submitted transcode job.
type Job struct {
	ID string `json:"id"`
}

// JobStatus describes the current status and metrics of a transcode job.
type JobStatus struct {
	ID            string             `json:"id"`
	Status        string             `json:"status"`
	Progress      float64            `json:"progress"`
	FPS           float64            `json:"fps"`
	Speed         float64            `json:"speed"`
	CandidatePath string             `json:"candidate_path"`
	Error         string             `json:"error,omitempty"`
	Profile       string             `json:"profile,omitempty"`
	Container     string             `json:"container,omitempty"`
	VideoCodec    string             `json:"video_codec,omitempty"`
	Quality       int                `json:"quality,omitempty"`
	Conversions   []ConversionRecord `json:"conversions,omitempty"`
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
