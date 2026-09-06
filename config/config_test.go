package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnownServicesHaveCompleteDefaults(t *testing.T) {
	for name := range DefaultPorts {
		if DefaultAPIVersions[name] == "" {
			t.Errorf("service %q has no default API version", name)
		}
		if DefaultAuthMethods[name] == "" {
			t.Errorf("service %q has no default auth method", name)
		}
		if DefaultStatusPaths[name] == "" {
			t.Errorf("service %q has no default status path", name)
		}
		// A service needs a spec from somewhere, but not necessarily from
		// GitHub: Bazarr publishes none and serves its own, so it is covered by
		// DefaultSelfHostedSpecPaths instead.
		if DefaultOpenAPIURLs[name] == "" && DefaultSelfHostedSpecPaths[name] == "" {
			t.Errorf("service %q has no spec source: no GitHub URL and no self-hosted path", name)
		}
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name    string
		service string
		raw     string
		want    string
		wantErr bool
	}{
		{"omitted url falls back to localhost", "sonarr", "", "http://localhost:8989", false},
		{"bare host gets default port", "sonarr", "http://10.0.0.100", "http://10.0.0.100:8989", false},
		{"missing scheme is filled in", "radarr", "10.0.0.100", "http://10.0.0.100:7878", false},
		{"explicit port is kept", "sonarr", "http://10.0.0.100:9999", "http://10.0.0.100:9999", false},
		{"https is preserved", "seerr", "https://seerr.example.com", "https://seerr.example.com:5055", false},
		{"trailing slash is trimmed", "sonarr", "http://10.0.0.100:8989/", "http://10.0.0.100:8989", false},
		{"subpath is kept", "sonarr", "http://10.0.0.100:8989/sonarr", "http://10.0.0.100:8989/sonarr", false},
		{"unknown service keeps url as given", "custom", "http://10.0.0.100:1234", "http://10.0.0.100:1234", false},
		{"unknown service without port is left alone", "custom", "http://10.0.0.100", "http://10.0.0.100", false},
		{"unknown service without url is an error", "custom", "", "", true},
		{"url without host is an error", "sonarr", "http://", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveURL(tt.service, tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveURL(%q, %q) = %q, want error", tt.service, tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveURL(%q, %q): unexpected error: %v", tt.service, tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.service, tt.raw, got, tt.want)
			}
		})
	}
}

func TestConfigExampleParsesAndValidates(t *testing.T) {
	examplePath := filepath.Join("..", "config.yaml.example")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("failed to load %s: %v", examplePath, err)
	}

	if cfg.LoadedPath != examplePath {
		t.Errorf("expected LoadedPath=%s, got %s", examplePath, cfg.LoadedPath)
	}

	// 1. Validate top-level flags and guards
	if cfg.AllowDestructive != false {
		t.Errorf("expected AllowDestructive=false by default, got %v", cfg.AllowDestructive)
	}
	if cfg.MaxResponseSizeKB != 500 {
		t.Errorf("expected MaxResponseSizeKB=500, got %d", cfg.MaxResponseSizeKB)
	}
	if cfg.Concurrency.MaxAPISimultaneous != 3 {
		t.Errorf("expected MaxAPISimultaneous=3, got %d", cfg.Concurrency.MaxAPISimultaneous)
	}
	if cfg.Concurrency.MaxInspectSimultaneous != 2 {
		t.Errorf("expected MaxInspectSimultaneous=2, got %d", cfg.Concurrency.MaxInspectSimultaneous)
	}

	// 2. Validate services
	expectedServices := []string{"sonarr", "radarr", "lidarr", "readarr", "chaptarr", "prowlarr", "profilarr", "bazarr", "seerr", "audiobookshelf"}
	for _, sName := range expectedServices {
		sCfg, ok := cfg.Services[sName]
		if !ok {
			t.Errorf("expected service %q in config.yaml.example", sName)
			continue
		}
		if !strings.Contains(sCfg.APIKey, "imaginary-") {
			t.Errorf("service %q has potentially non-dummy API key: %q", sName, sCfg.APIKey)
		}
	}

	// 3. Validate download clients
	if !strings.Contains(cfg.Transmission.Password, "imaginary-") {
		t.Errorf("Transmission has potentially non-dummy password: %q", cfg.Transmission.Password)
	}
	if !strings.Contains(cfg.QBittorrent.Password, "imaginary-") {
		t.Errorf("QBittorrent has potentially non-dummy password: %q", cfg.QBittorrent.Password)
	}
	if !strings.Contains(cfg.SABnzbd.APIKey, "imaginary-") {
		t.Errorf("SABnzbd has potentially non-dummy API key: %q", cfg.SABnzbd.APIKey)
	}

	// 4. Validate queue
	if cfg.Queue.Listen != "127.0.0.1:8099" {
		t.Errorf("expected queue listen 127.0.0.1:8099, got %s", cfg.Queue.Listen)
	}
	if !strings.Contains(cfg.Queue.Token, "imaginary-") {
		t.Errorf("Queue token is potentially non-dummy: %q", cfg.Queue.Token)
	}

	// 5. Validate media roots
	if len(cfg.Media.AllowedReadRoots) == 0 {
		t.Errorf("expected allowed_read_roots in example")
	}
	if len(cfg.Media.AllowedWriteRoots) == 0 {
		t.Errorf("expected allowed_write_roots in example")
	}

	// 6. Validate maintenance heuristics
	if cfg.Maintenance.OversizedPerEpisodeMB != 900 {
		t.Errorf("expected OversizedPerEpisodeMB=900, got %d", cfg.Maintenance.OversizedPerEpisodeMB)
	}
	if len(cfg.Maintenance.PreferredGroups) == 0 {
		t.Errorf("expected PreferredGroups in example")
	}
}

func TestValidateRoots(t *testing.T) {
	tempDir := t.TempDir()
	validRoot := filepath.Join(tempDir, "valid_media")
	if err := os.Mkdir(validRoot, 0755); err != nil {
		t.Fatal(err)
	}
	missingRoot := filepath.Join(tempDir, "missing_media")

	cfg := &Config{
		Media: MediaConfig{
			AllowedReadRoots:  []string{validRoot, missingRoot},
			AllowedWriteRoots: []string{missingRoot},
		},
	}

	warnings := cfg.ValidateRoots()
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings for missing roots (1 read, 1 write), got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "read root") || !strings.Contains(warnings[0], missingRoot) {
		t.Errorf("unexpected read warning: %s", warnings[0])
	}
	if !strings.Contains(warnings[1], "write root") || !strings.Contains(warnings[1], missingRoot) {
		t.Errorf("unexpected write warning: %s", warnings[1])
	}
}

