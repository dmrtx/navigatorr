package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/transcode"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Services          map[string]ServiceConfig `yaml:"services"`
	Transmission      TransmissionConfig       `yaml:"transmission"`
	QBittorrent       QBittorrentConfig        `yaml:"qbittorrent"`
	SABnzbd           SABnzbdConfig            `yaml:"sabnzbd"`
	Transcode         TranscodeConfig          `yaml:"transcode"`
	Queue             QueueConfig              `yaml:"queue"`
	Database          DatabaseConfig           `yaml:"database"`
	Media             MediaConfig              `yaml:"media"`
	Maintenance       MaintenanceConfig        `yaml:"maintenance"`
	Concurrency       ConcurrencyConfig        `yaml:"concurrency"`
	MaxResponseSizeKB int                      `yaml:"max_response_size_kb"`
	AllowDestructive  bool                     `yaml:"allow_destructive"`
	LoadedPath        string                   `yaml:"-"`
}

// ConcurrencyConfig controls simultaneous upstream operations.
type ConcurrencyConfig struct {
	MaxAPISimultaneous     int `yaml:"max_api_simultaneous"`     // default: 3
	MaxInspectSimultaneous int `yaml:"max_inspect_simultaneous"` // default: 2
}

// DatabaseConfig locates the SQLite state file. It defaults into the same
// cache directory as the OpenAPI specs, which deployments already persist.
type DatabaseConfig struct {
	Path string `yaml:"path"` // defaults to ~/.cache/navigatorr/navigatorr.db
}

// MediaConfig bounds filesystem access and media inspection.
type MediaConfig struct {
	AllowedReadRoots  []string `yaml:"allowed_read_roots"`
	AllowedWriteRoots []string `yaml:"allowed_write_roots"`
	FfprobePath       string   `yaml:"ffprobe_path"` // empty = look up "ffprobe" on PATH
}

// MaintenanceConfig carries the global defaults that stored preferences
// may override per scope.
type MaintenanceConfig struct {
	OversizedPerEpisodeMB int64    `yaml:"oversized_per_episode_mb"`
	PreferredGroups       []string `yaml:"preferred_groups"`
	PreferredResolution   string   `yaml:"preferred_resolution"`
}

type ServiceConfig struct {
	URL        string `yaml:"url"`
	APIKey     string `yaml:"api_key"`
	AuthMethod string `yaml:"auth_method"` // "header", "query", "basic"
	AuthHeader string `yaml:"auth_header"` // custom header name, defaults to X-Api-Key
	AuthPrefix string `yaml:"auth_prefix"` // prefix for the key value, e.g. "Bearer"
	APIVersion string `yaml:"api_version"` // e.g. "/api/v3"
	OpenAPIURL string `yaml:"openapi_url"` // override spec URL
}

type TransmissionConfig struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type QBittorrentConfig struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type SABnzbdConfig struct {
	URL     string `yaml:"url"`
	APIKey  string `yaml:"api_key"`
	URLBase string `yaml:"url_base"` // SABnzbd's own url_base, "/sabnzbd" by default
}

type TranscodeConfig struct {
	Enabled           bool                              `yaml:"enabled"`
	Executor          string                            `yaml:"executor"` // "ssh"
	DefaultAction     string                            `yaml:"default_action"`
	DefaultProfile    string                            `yaml:"default_profile"`
	MinSavingsPercent float64                           `yaml:"min_savings_percent"`
	MaxParallelJobs   int                               `yaml:"max_parallel_jobs"`
	SSH               SSHExecutorConfig                 `yaml:"ssh"`
	Profiles          map[string]TranscodeProfileConfig `yaml:"profiles"`
}

type TranscodeProfileConfig struct {
	Container string                `yaml:"container"`
	Video     VideoProfileConfig    `yaml:"video"`
	Audio     AudioProfileConfig    `yaml:"audio"`
	Subtitles SubtitleProfileConfig `yaml:"subtitles"`
	Preserve  PreserveProfileConfig `yaml:"preserve"`
}

type VideoProfileConfig struct {
	Codec   string `yaml:"codec"`
	Quality int    `yaml:"quality"`
}

type AudioProfileConfig struct {
	Mode string `yaml:"mode"`
}

type SubtitleProfileConfig struct {
	Mode                string `yaml:"mode"`
	ConvertIncompatible bool   `yaml:"convert_incompatible"`
}

type PreserveProfileConfig struct {
	Metadata    bool `yaml:"metadata"`
	Chapters    bool `yaml:"chapters"`
	Attachments bool `yaml:"attachments"`
}

var validProfileNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// BuiltinTranscodeProfiles returns the default, out-of-the-box transcode profiles.
func BuiltinTranscodeProfiles() map[string]TranscodeProfileConfig {
	return map[string]TranscodeProfileConfig{
		"hevc-vt": {
			Container: "mkv",
			Video: VideoProfileConfig{
				Codec:   "hevc_videotoolbox",
				Quality: 65,
			},
			Audio: AudioProfileConfig{
				Mode: "copy",
			},
			Subtitles: SubtitleProfileConfig{
				Mode:                "preserve",
				ConvertIncompatible: true,
			},
			Preserve: PreserveProfileConfig{
				Metadata:    true,
				Chapters:    true,
				Attachments: true,
			},
		},
		"hevc-vt-balanced": {
			Container: "mkv",
			Video: VideoProfileConfig{
				Codec:   "hevc_videotoolbox",
				Quality: 65,
			},
			Audio: AudioProfileConfig{
				Mode: "copy",
			},
			Subtitles: SubtitleProfileConfig{
				Mode:                "preserve",
				ConvertIncompatible: true,
			},
			Preserve: PreserveProfileConfig{
				Metadata:    true,
				Chapters:    true,
				Attachments: true,
			},
		},
		"hevc-vt-quality": {
			Container: "mkv",
			Video: VideoProfileConfig{
				Codec:   "hevc_videotoolbox",
				Quality: 55,
			},
			Audio: AudioProfileConfig{
				Mode: "copy",
			},
			Subtitles: SubtitleProfileConfig{
				Mode:                "preserve",
				ConvertIncompatible: true,
			},
			Preserve: PreserveProfileConfig{
				Metadata:    true,
				Chapters:    true,
				Attachments: true,
			},
		},
		"hevc-vt-space": {
			Container: "mkv",
			Video: VideoProfileConfig{
				Codec:   "hevc_videotoolbox",
				Quality: 75,
			},
			Audio: AudioProfileConfig{
				Mode: "copy",
			},
			Subtitles: SubtitleProfileConfig{
				Mode:                "preserve",
				ConvertIncompatible: true,
			},
			Preserve: PreserveProfileConfig{
				Metadata:    true,
				Chapters:    true,
				Attachments: true,
			},
		},
	}
}

// ResolvePlan resolves a profile name into a validated, structured execution plan.
// If profileName is empty, it falls back to DefaultProfile, then to "hevc-vt".
func (t *TranscodeConfig) ResolvePlan(profileName string) (*transcode.Plan, error) {
	name := strings.TrimSpace(profileName)
	if name == "" {
		if t.DefaultProfile != "" {
			name = t.DefaultProfile
		} else {
			name = "hevc-vt"
		}
	}

	var prof TranscodeProfileConfig
	var found bool
	if t.Profiles != nil {
		prof, found = t.Profiles[name]
	}
	if !found {
		builtins := BuiltinTranscodeProfiles()
		prof, found = builtins[name]
	}
	if !found {
		return nil, fmt.Errorf("unknown transcode profile %q", name)
	}

	return &transcode.Plan{
		Container:                    prof.Container,
		VideoCodec:                   prof.Video.Codec,
		Quality:                      prof.Video.Quality,
		AudioMode:                    prof.Audio.Mode,
		SubtitleMode:                 prof.Subtitles.Mode,
		ConvertIncompatibleSubtitles: prof.Subtitles.ConvertIncompatible,
		PreserveMetadata:             prof.Preserve.Metadata,
		PreserveChapters:             prof.Preserve.Chapters,
		PreserveAttachments:          prof.Preserve.Attachments,
	}, nil
}

// Validate verifies transcode profile definitions fail-closed at load time.
func (t *TranscodeConfig) Validate() error {
	if t.DefaultProfile != "" {
		if _, err := t.ResolvePlan(t.DefaultProfile); err != nil {
			return fmt.Errorf("transcode: default_profile %q is not defined or invalid: %w", t.DefaultProfile, err)
		}
	}

	for name, p := range t.Profiles {
		if !validProfileNameRegex.MatchString(name) {
			return fmt.Errorf("transcode: invalid profile name %q (allowed characters: letters, numbers, dash, underscore)", name)
		}

		// Container validation
		cont := strings.ToLower(strings.TrimSpace(p.Container))
		if cont != "mkv" && cont != "matroska" {
			return fmt.Errorf("transcode profile %q: unsupported container %q (supported: mkv)", name, p.Container)
		}

		// Video codec validation
		vCodec := strings.ToLower(strings.TrimSpace(p.Video.Codec))
		if vCodec != "hevc_videotoolbox" {
			return fmt.Errorf("transcode profile %q: unsupported video codec %q (supported: hevc_videotoolbox)", name, p.Video.Codec)
		}

		// Quality validation
		if p.Video.Quality < 1 || p.Video.Quality > 100 {
			return fmt.Errorf("transcode profile %q: video quality %d out of range (allowed: 1-100)", name, p.Video.Quality)
		}

		// Audio mode validation
		aMode := strings.ToLower(strings.TrimSpace(p.Audio.Mode))
		if aMode != "copy" {
			return fmt.Errorf("transcode profile %q: unsupported audio mode %q (supported: copy)", name, p.Audio.Mode)
		}

		// Subtitle mode validation
		sMode := strings.ToLower(strings.TrimSpace(p.Subtitles.Mode))
		if sMode != "preserve" {
			return fmt.Errorf("transcode profile %q: unsupported subtitles mode %q (supported: preserve)", name, p.Subtitles.Mode)
		}
	}

	return nil
}

type SSHExecutorConfig struct {
	Host              string                 `yaml:"host"`
	Port              int                    `yaml:"port"`
	User              string                 `yaml:"user"`
	Command           string                 `yaml:"command"`
	RemoteBinary      string                 `yaml:"remote_binary"`
	IdentityFile      string                 `yaml:"identity_file"`
	SSHKeyPath        string                 `yaml:"ssh_key_path"`
	KnownHostsPath    string                 `yaml:"known_hosts_path"`
	ConnectTimeout    string                 `yaml:"connect_timeout"`
	ConnectTimeoutSec int                    `yaml:"connect_timeout_sec"`
	CommandTimeoutSec int                    `yaml:"command_timeout_sec"`
	PathMappings      []TranscodePathMapping `yaml:"path_mappings"`
}

func (s SSHExecutorConfig) RemoteCommand() string {
	if s.RemoteBinary != "" {
		return s.RemoteBinary
	}
	return s.Command
}

func (s SSHExecutorConfig) KeyFile() string {
	if s.SSHKeyPath != "" {
		return s.SSHKeyPath
	}
	return s.IdentityFile
}

// TimeoutDuration returns the parsed connect_timeout duration or defaults to 5s.
func (s SSHExecutorConfig) TimeoutDuration() time.Duration {
	if s.ConnectTimeoutSec > 0 {
		return time.Duration(s.ConnectTimeoutSec) * time.Second
	}
	if s.ConnectTimeout != "" {
		if d, err := time.ParseDuration(s.ConnectTimeout); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Second
}

func (s SSHExecutorConfig) CommandTimeoutDuration() time.Duration {
	if s.CommandTimeoutSec > 0 {
		return time.Duration(s.CommandTimeoutSec) * time.Second
	}
	return 60 * time.Second
}

type TranscodePathMapping struct {
	Local        string `yaml:"local"`
	Remote       string `yaml:"remote"`
	LocalPrefix  string `yaml:"local_prefix"`
	RemotePrefix string `yaml:"remote_prefix"`
}

func (m TranscodePathMapping) GetLocal() string {
	if m.LocalPrefix != "" {
		return m.LocalPrefix
	}
	return m.Local
}

func (m TranscodePathMapping) GetRemote() string {
	if m.RemotePrefix != "" {
		return m.RemotePrefix
	}
	return m.Remote
}

func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "navigatorr", "config.yaml")
}

// QueueConfig controls the HTTP request queue. Listen is required to serve the
// HTTP endpoint; the MCP queue tools work regardless so an agent can always
// drain whatever has accumulated.
type QueueConfig struct {
	Listen string `yaml:"listen"` // e.g. ":8099"; empty disables the HTTP endpoint
	Token  string `yaml:"token"`  // bearer token; empty disables auth
	Path   string `yaml:"path"`   // queue file; defaults to ~/.config/navigatorr/queue.json
}

func DefaultQueuePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "navigatorr", "queue.json")
}

// DefaultDatabasePath is the persistent SQLite file. Under Docker it lands
// in /root/.cache/navigatorr, which compose.yaml already mounts as a volume.
func DefaultDatabasePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "navigatorr", "navigatorr.db")
}

var notFoundFieldRegex = regexp.MustCompile(`field\s+([a-zA-Z0-9_-]+)\s+not\s+found\s+in\s+type\s+([a-zA-Z0-9_.]+)`)

var structTypeToSection = map[string]string{
	"config.MediaConfig":            "media",
	"MediaConfig":                   "media",
	"config.MaintenanceConfig":      "maintenance",
	"MaintenanceConfig":             "maintenance",
	"config.ConcurrencyConfig":      "concurrency",
	"ConcurrencyConfig":             "concurrency",
	"config.DatabaseConfig":         "database",
	"DatabaseConfig":                "database",
	"config.QueueConfig":            "queue",
	"QueueConfig":                   "queue",
	"config.TransmissionConfig":     "transmission",
	"TransmissionConfig":            "transmission",
	"config.QBittorrentConfig":      "qbittorrent",
	"QBittorrentConfig":             "qbittorrent",
	"config.SABnzbdConfig":          "sabnzbd",
	"SABnzbdConfig":                 "sabnzbd",
	"config.TranscodeConfig":        "transcode",
	"TranscodeConfig":               "transcode",
	"config.SSHExecutorConfig":      "transcode",
	"SSHExecutorConfig":             "transcode",
	"config.TranscodePathMapping":   "transcode",
	"TranscodePathMapping":          "transcode",
	"config.TranscodeProfileConfig": "transcode.profiles",
	"TranscodeProfileConfig":        "transcode.profiles",
	"config.VideoProfileConfig":     "transcode.profiles.video",
	"VideoProfileConfig":            "transcode.profiles.video",
	"config.AudioProfileConfig":     "transcode.profiles.audio",
	"AudioProfileConfig":            "transcode.profiles.audio",
	"config.SubtitleProfileConfig":  "transcode.profiles.subtitles",
	"SubtitleProfileConfig":         "transcode.profiles.subtitles",
	"config.PreserveProfileConfig":  "transcode.profiles.preserve",
	"PreserveProfileConfig":         "transcode.profiles.preserve",
	"config.ServiceConfig":          "services",
	"ServiceConfig":                 "services",
}

var topLevelKeys = map[string]bool{
	"services":             true,
	"transmission":         true,
	"qbittorrent":          true,
	"sabnzbd":              true,
	"transcode":            true,
	"queue":                true,
	"database":             true,
	"media":                true,
	"maintenance":          true,
	"concurrency":          true,
	"max_response_size_kb": true,
	"allow_destructive":    true,
}

func formatConfigError(path string, err error) error {
	errMsg := err.Error()
	matches := notFoundFieldRegex.FindStringSubmatch(errMsg)
	if len(matches) == 3 {
		field := matches[1]
		typeName := matches[2]

		if section, ok := structTypeToSection[typeName]; ok {
			keyPath := fmt.Sprintf("%s.%s", section, field)
			if topLevelKeys[field] {
				return fmt.Errorf("parsing config %s: unknown configuration key %q; %q is a top-level option: %w", path, keyPath, field, err)
			}
			return fmt.Errorf("parsing config %s: unknown configuration key %q: %w", path, keyPath, err)
		}

		if typeName == "config.Config" || typeName == "Config" {
			if field == "allow_destuctive" {
				return fmt.Errorf("parsing config %s: unknown configuration key %q; did you mean %q?: %w", path, field, "allow_destructive", err)
			}
			return fmt.Errorf("parsing config %s: unknown configuration key %q: %w", path, field, err)
		}
	}
	return fmt.Errorf("parsing config %s: %w", path, err)
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg := &Config{
		Services: make(map[string]ServiceConfig),
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if !errors.Is(err, io.EOF) {
			return nil, formatConfigError(path, err)
		}
	}

	// Apply defaults for known service types.
	for name, svc := range cfg.Services {
		if svc.AuthMethod == "" {
			if m, ok := DefaultAuthMethods[name]; ok {
				svc.AuthMethod = m
			} else {
				svc.AuthMethod = "header"
			}
		}
		if svc.AuthHeader == "" {
			if h, ok := DefaultAuthHeaders[name]; ok {
				svc.AuthHeader = h
			} else if svc.AuthMethod == "header" {
				svc.AuthHeader = "X-Api-Key"
			}
		}
		if svc.AuthPrefix == "" {
			if p, ok := DefaultAuthPrefixes[name]; ok {
				svc.AuthPrefix = p
			}
		}
		if svc.APIVersion == "" {
			if v, ok := DefaultAPIVersions[name]; ok {
				svc.APIVersion = v
			}
		}
		resolved, err := resolveURL(name, svc.URL)
		if err != nil {
			return nil, err
		}
		svc.URL = resolved
		if svc.OpenAPIURL == "" {
			if u, ok := DefaultOpenAPIURLs[name]; ok {
				svc.OpenAPIURL = u
			} else if p, ok := DefaultSelfHostedSpecPaths[name]; ok {
				// Runs after resolveURL so the spec URL inherits the scheme and
				// port that were filled in, rather than whatever partial host
				// the config happened to carry.
				svc.OpenAPIURL = strings.TrimSuffix(svc.URL, "/") + p
			}
		}
		cfg.Services[name] = svc
	}

	// Default response size guard to 50KB if not set.
	if cfg.MaxResponseSizeKB <= 0 {
		cfg.MaxResponseSizeKB = 50
	}

	if cfg.Maintenance.OversizedPerEpisodeMB <= 0 {
		cfg.Maintenance.OversizedPerEpisodeMB = 900
	}
	if len(cfg.Maintenance.PreferredGroups) == 0 {
		cfg.Maintenance.PreferredGroups = []string{"Judas", "EMBER", "ASW"}
	}
	if cfg.Maintenance.PreferredResolution == "" {
		cfg.Maintenance.PreferredResolution = "1080p"
	}

	if cfg.Concurrency.MaxAPISimultaneous <= 0 {
		cfg.Concurrency.MaxAPISimultaneous = 3
	}
	if cfg.Concurrency.MaxInspectSimultaneous <= 0 {
		cfg.Concurrency.MaxInspectSimultaneous = 2
	}

	if err := cfg.Transcode.Validate(); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg.LoadedPath = path

	return cfg, nil
}

// ValidateRoots checks whether the configured read and write filesystem roots exist on disk.
func (c *Config) ValidateRoots() []string {
	var warnings []string
	for _, r := range c.Media.AllowedReadRoots {
		if r == "" {
			continue
		}
		if _, err := os.Stat(r); os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("configured read root %q does not exist inside container (verify Docker volume mounts)", r))
		}
	}
	for _, r := range c.Media.AllowedWriteRoots {
		if r == "" {
			continue
		}
		if _, err := os.Stat(r); os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("configured write root %q does not exist inside container (verify Docker volume mounts)", r))
		}
	}
	return warnings
}

// resolveURL normalizes a service URL, filling in the scheme and the service's
// default port when they are absent. An omitted URL falls back to localhost;
// list_services reports the resolved URL, so a wrong guess is visible rather
// than a confusing failure on the first API call.
func resolveURL(name, raw string) (string, error) {
	port, known := DefaultPorts[name]

	if raw == "" {
		if !known {
			return "", fmt.Errorf("service %q: url is required (no default port for this service)", name)
		}
		return fmt.Sprintf("http://localhost:%d", port), nil
	}

	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("service %q: parsing url %q: %w", name, raw, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("service %q: url %q has no host", name, raw)
	}
	if u.Port() == "" && known {
		u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(port))
	}

	return strings.TrimRight(u.String(), "/"), nil
}
