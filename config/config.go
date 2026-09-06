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

	"gopkg.in/yaml.v3"
)

type Config struct {
	Services          map[string]ServiceConfig `yaml:"services"`
	Transmission      TransmissionConfig       `yaml:"transmission"`
	QBittorrent       QBittorrentConfig        `yaml:"qbittorrent"`
	SABnzbd           SABnzbdConfig            `yaml:"sabnzbd"`
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
	"config.MediaConfig":        "media",
	"MediaConfig":               "media",
	"config.MaintenanceConfig":  "maintenance",
	"MaintenanceConfig":         "maintenance",
	"config.ConcurrencyConfig":  "concurrency",
	"ConcurrencyConfig":         "concurrency",
	"config.DatabaseConfig":     "database",
	"DatabaseConfig":            "database",
	"config.QueueConfig":        "queue",
	"QueueConfig":               "queue",
	"config.TransmissionConfig": "transmission",
	"TransmissionConfig":        "transmission",
	"config.QBittorrentConfig":  "qbittorrent",
	"QBittorrentConfig":         "qbittorrent",
	"config.SABnzbdConfig":      "sabnzbd",
	"SABnzbdConfig":             "sabnzbd",
	"config.ServiceConfig":      "services",
	"ServiceConfig":             "services",
}

var topLevelKeys = map[string]bool{
	"services":             true,
	"transmission":         true,
	"qbittorrent":          true,
	"sabnzbd":              true,
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
