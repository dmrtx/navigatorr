package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{"bare host gets default port", "sonarr", "http://192.0.2.100", "http://192.0.2.100:8989", false},
		{"missing scheme is filled in", "radarr", "192.0.2.100", "http://192.0.2.100:7878", false},
		{"explicit port is kept", "sonarr", "http://192.0.2.100:9999", "http://192.0.2.100:9999", false},
		{"https is preserved", "seerr", "https://seerr.example.com", "https://seerr.example.com:5055", false},
		{"trailing slash is trimmed", "sonarr", "http://192.0.2.100:8989/", "http://192.0.2.100:8989", false},
		{"subpath is kept", "sonarr", "http://192.0.2.100:8989/sonarr", "http://192.0.2.100:8989/sonarr", false},
		{"unknown service keeps url as given", "custom", "http://192.0.2.100:1234", "http://192.0.2.100:1234", false},
		{"unknown service without port is left alone", "custom", "http://192.0.2.100", "http://192.0.2.100", false},
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

func TestStrictConfigParsing(t *testing.T) {
	tempDir := t.TempDir()

	writeConfig := func(t *testing.T, filename, content string) string {
		p := filepath.Join(tempDir, filename)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("allow_destructive at top-level parses successfully", func(t *testing.T) {
		p := writeConfig(t, "valid_top.yaml", `
allow_destructive: true
media:
  allowed_read_roots: ["/media/test"]
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("expected valid load, got error: %v", err)
		}
		if !cfg.AllowDestructive {
			t.Errorf("expected AllowDestructive=true, got false")
		}
	})

	t.Run("allow_destructive incorrectly nested in media fails with top-level hint", func(t *testing.T) {
		p := writeConfig(t, "nested_media.yaml", `
media:
  allow_destructive: true
  allowed_read_roots: ["/media/test"]
`)
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected strict parsing error for media.allow_destructive, got nil")
		}
		errMsg := err.Error()
		if !strings.Contains(errMsg, `unknown configuration key "media.allow_destructive"`) {
			t.Errorf("expected error to mention unknown key media.allow_destructive, got: %s", errMsg)
		}
		if !strings.Contains(errMsg, `"allow_destructive" is a top-level option`) {
			t.Errorf("expected error to provide top-level hint, got: %s", errMsg)
		}
	})

	t.Run("typo allow_destuctive fails with correction hint", func(t *testing.T) {
		p := writeConfig(t, "typo.yaml", `
allow_destuctive: true
`)
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected strict parsing error for allow_destuctive typo, got nil")
		}
		errMsg := err.Error()
		if !strings.Contains(errMsg, `unknown configuration key "allow_destuctive"`) {
			t.Errorf("expected error to mention allow_destuctive, got: %s", errMsg)
		}
		if !strings.Contains(errMsg, `did you mean "allow_destructive"?`) {
			t.Errorf("expected error to offer correction hint, got: %s", errMsg)
		}
	})

	t.Run("completely unknown key fails immediately", func(t *testing.T) {
		p := writeConfig(t, "unknown.yaml", `
completely_unknown_key: 123
`)
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected strict parsing error for completely_unknown_key, got nil")
		}
		errMsg := err.Error()
		if !strings.Contains(errMsg, `unknown configuration key "completely_unknown_key"`) {
			t.Errorf("expected error to mention completely_unknown_key, got: %s", errMsg)
		}
	})

	t.Run("config.yaml.example continues to validate cleanly", func(t *testing.T) {
		examplePath := filepath.Join("..", "config.yaml.example")
		cfg, err := Load(examplePath)
		if err != nil {
			t.Fatalf("config.yaml.example failed strict parsing: %v", err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
	})
}

func TestTranscodeConfig(t *testing.T) {
	tempDir := t.TempDir()
	writeCfg := func(filename, content string) string {
		p := filepath.Join(tempDir, filename)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("parses valid transcode configuration", func(t *testing.T) {
		p := writeCfg("transcode_valid.yaml", `
transcode:
  enabled: true
  executor: "ssh"
  ssh:
    host: "192.0.2.10"
    user: "transcoder"
    command: "/Users/transcoder/.local/bin/navigatorr-transcode"
    identity_file: "/run/secrets/navigatorr_transcode_ssh"
    connect_timeout: "10s"
    path_mappings:
      - local: "/media"
        remote: "/Volumes/media"
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("unexpected error loading transcode config: %v", err)
		}
		if !cfg.Transcode.Enabled {
			t.Error("expected Transcode.Enabled=true")
		}
		if cfg.Transcode.Executor != "ssh" {
			t.Errorf("unexpected Executor: %s", cfg.Transcode.Executor)
		}
		if cfg.Transcode.SSH.Host != "192.0.2.10" {
			t.Errorf("unexpected Host: %s", cfg.Transcode.SSH.Host)
		}
		if cfg.Transcode.SSH.User != "transcoder" {
			t.Errorf("unexpected User: %s", cfg.Transcode.SSH.User)
		}
		if cfg.Transcode.SSH.Command != "/Users/transcoder/.local/bin/navigatorr-transcode" {
			t.Errorf("unexpected Command: %s", cfg.Transcode.SSH.Command)
		}
		if cfg.Transcode.SSH.TimeoutDuration() != 10*time.Second {
			t.Errorf("expected 10s timeout, got %v", cfg.Transcode.SSH.TimeoutDuration())
		}
		if len(cfg.Transcode.SSH.PathMappings) != 1 {
			t.Fatalf("expected 1 path mapping, got %d", len(cfg.Transcode.SSH.PathMappings))
		}
		mapping := cfg.Transcode.SSH.PathMappings[0]
		if mapping.Local != "/media" || mapping.Remote != "/Volumes/media" {
			t.Errorf("unexpected mapping: %+v", mapping)
		}
	})

	t.Run("parses transcode configuration with user aliases and secrets", func(t *testing.T) {
		p := writeCfg("transcode_user_aliases.yaml", `
transcode:
  enabled: true
  executor: "ssh"
  default_action: "manual_approval"
  min_savings_percent: 15.0
  max_parallel_jobs: 1
  ssh:
    host: "192.0.2.10"
    port: 22
    user: "transcoder"
    ssh_key_path: "/run/secrets/navigatorr_transcode_ssh"
    known_hosts_path: "/run/secrets/navigatorr_known_hosts"
    remote_binary: "/usr/local/bin/navigatorr-transcode"
    connect_timeout_sec: 5
    command_timeout_sec: 60
    path_mappings:
      - local_prefix: "/media"
        remote_prefix: "/Volumes/media"
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("unexpected error loading transcode config with aliases: %v", err)
		}
		if cfg.Transcode.DefaultAction != "manual_approval" {
			t.Errorf("expected default_action=manual_approval, got %s", cfg.Transcode.DefaultAction)
		}
		if cfg.Transcode.MinSavingsPercent != 15.0 {
			t.Errorf("expected min_savings_percent=15.0, got %f", cfg.Transcode.MinSavingsPercent)
		}
		if cfg.Transcode.MaxParallelJobs != 1 {
			t.Errorf("expected max_parallel_jobs=1, got %d", cfg.Transcode.MaxParallelJobs)
		}
		ssh := cfg.Transcode.SSH
		if ssh.Port != 22 {
			t.Errorf("expected port=22, got %d", ssh.Port)
		}
		if ssh.KeyFile() != "/run/secrets/navigatorr_transcode_ssh" {
			t.Errorf("expected key file /run/secrets/navigatorr_transcode_ssh, got %s", ssh.KeyFile())
		}
		if ssh.KnownHostsPath != "/run/secrets/navigatorr_known_hosts" {
			t.Errorf("expected known hosts /run/secrets/navigatorr_known_hosts, got %s", ssh.KnownHostsPath)
		}
		if ssh.RemoteCommand() != "/usr/local/bin/navigatorr-transcode" {
			t.Errorf("expected remote command /usr/local/bin/navigatorr-transcode, got %s", ssh.RemoteCommand())
		}
		if ssh.TimeoutDuration() != 5*time.Second {
			t.Errorf("expected timeout 5s, got %v", ssh.TimeoutDuration())
		}
		if ssh.CommandTimeoutDuration() != 60*time.Second {
			t.Errorf("expected command timeout 60s, got %v", ssh.CommandTimeoutDuration())
		}
		if len(ssh.PathMappings) != 1 {
			t.Fatalf("expected 1 path mapping, got %d", len(ssh.PathMappings))
		}
		m := ssh.PathMappings[0]
		if m.GetLocal() != "/media" || m.GetRemote() != "/Volumes/media" {
			t.Errorf("unexpected mapping: local=%s, remote=%s", m.GetLocal(), m.GetRemote())
		}
	})

	t.Run("default timeout when empty", func(t *testing.T) {
		tc := SSHExecutorConfig{}
		if tc.TimeoutDuration() != 5*time.Second {
			t.Errorf("expected default 5s timeout, got %v", tc.TimeoutDuration())
		}
	})

	t.Run("misnested transcode key fails strict parsing", func(t *testing.T) {
		p := writeCfg("transcode_nested.yaml", `
media:
  transcode:
    enabled: true
`)
		_, err := Load(p)
		if err == nil {
			t.Fatal("expected strict parsing error for nested transcode, got nil")
		}
		if !strings.Contains(err.Error(), "transcode") {
			t.Errorf("expected error mentioning transcode, got %v", err)
		}
	})
}

func TestTranscode_Profiles(t *testing.T) {
	tempDir := t.TempDir()

	writeCfg := func(name, content string) string {
		p := filepath.Join(tempDir, name)
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return p
	}

	t.Run("legacy hevc-vt profile resolves without profiles configured", func(t *testing.T) {
		p := writeCfg("transcode_builtin.yaml", `
transcode:
  enabled: true
  executor: "ssh"
  ssh:
    host: "192.0.2.10"
    user: "transcoder"
    command: "/opt/homebrew/bin/navigatorr-transcode"
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("failed to load config: %v", err)
		}

		plan, err := cfg.Transcode.ResolvePlan("hevc-vt")
		if err != nil {
			t.Fatalf("failed to resolve legacy profile hevc-vt: %v", err)
		}
		if plan.Container != "mkv" {
			t.Errorf("expected container mkv, got %s", plan.Container)
		}
		if plan.VideoCodec != "hevc_videotoolbox" {
			t.Errorf("expected video codec hevc_videotoolbox, got %s", plan.VideoCodec)
		}
		if plan.Quality != 65 {
			t.Errorf("expected default quality 65, got %d", plan.Quality)
		}
		if plan.AudioMode != "copy" {
			t.Errorf("expected audio mode copy, got %s", plan.AudioMode)
		}
		if plan.SubtitleMode != "preserve" {
			t.Errorf("expected subtitle mode preserve, got %s", plan.SubtitleMode)
		}
		if !plan.ConvertIncompatibleSubtitles {
			t.Errorf("expected ConvertIncompatibleSubtitles=true")
		}
		if !plan.PreserveMetadata || !plan.PreserveChapters || !plan.PreserveAttachments {
			t.Errorf("expected metadata, chapters, attachments to be preserved")
		}
	})

	t.Run("two profiles with different quality generate different plans", func(t *testing.T) {
		p := writeCfg("transcode_diff_quality.yaml", `
transcode:
  enabled: true
  executor: "ssh"
  default_profile: "hevc-vt-quality"
  profiles:
    hevc-vt-quality:
      container: mkv
      video:
        codec: hevc_videotoolbox
        quality: 55
      audio:
        mode: copy
      subtitles:
        mode: preserve
        convert_incompatible: true
      preserve:
        metadata: true
        chapters: true
        attachments: true
    hevc-vt-space:
      container: mkv
      video:
        codec: hevc_videotoolbox
        quality: 75
      audio:
        mode: copy
      subtitles:
        mode: preserve
        convert_incompatible: true
      preserve:
        metadata: true
        chapters: true
        attachments: true
  ssh:
    host: "192.0.2.10"
    user: "transcoder"
    command: "/opt/homebrew/bin/navigatorr-transcode"
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("failed to load config: %v", err)
		}

		planQual, err := cfg.Transcode.ResolvePlan("hevc-vt-quality")
		if err != nil {
			t.Fatalf("resolving hevc-vt-quality: %v", err)
		}
		planSpace, err := cfg.Transcode.ResolvePlan("hevc-vt-space")
		if err != nil {
			t.Fatalf("resolving hevc-vt-space: %v", err)
		}

		if planQual.Quality != 55 {
			t.Errorf("expected quality 55, got %d", planQual.Quality)
		}
		if planSpace.Quality != 75 {
			t.Errorf("expected quality 75, got %d", planSpace.Quality)
		}
		if planQual.Quality == planSpace.Quality {
			t.Errorf("expected different qualities for different profiles")
		}

		// Empty string resolves to default_profile (hevc-vt-quality)
		planDef, err := cfg.Transcode.ResolvePlan("")
		if err != nil {
			t.Fatalf("resolving default profile: %v", err)
		}
		if planDef.Quality != 55 {
			t.Errorf("expected default profile quality 55, got %d", planDef.Quality)
		}
	})

	t.Run("default auto profile is case-insensitive and trimmed", func(t *testing.T) {
		for _, value := range []string{"auto", "AUTO", "Auto", " auto "} {
			tc := TranscodeConfig{DefaultProfile: value}
			tc.Recipes.CacheDir = t.TempDir()
			if err := tc.InitializeRecipes(context.Background()); err != nil {
				t.Fatalf("default_profile %q should be accepted as automatic selector pseudo-profile: %v", value, err)
			}
		}
	})

	t.Run("unknown profile returns error", func(t *testing.T) {
		tc := TranscodeConfig{}
		_, err := tc.ResolvePlan("nonexistent-profile")
		if err == nil {
			t.Fatal("expected error resolving nonexistent profile, got nil")
		}
		if !strings.Contains(err.Error(), "unknown transcode profile") {
			t.Errorf("expected unknown profile error, got %v", err)
		}
	})

	t.Run("invalid profile config rejected at load time", func(t *testing.T) {
		cases := []struct {
			name        string
			yamlSnippet string
			errSubstr   string
		}{
			{
				name: "disallowed container",
				yamlSnippet: `
transcode:
  profiles:
    bad-container:
      container: avi
      video: {codec: hevc_videotoolbox, quality: 65}
      audio: {mode: copy}
      subtitles: {mode: preserve}
`,
				errSubstr: "unsupported container",
			},
			{
				name: "disallowed video codec",
				yamlSnippet: `
transcode:
  profiles:
    bad-codec:
      container: mkv
      video: {codec: libx264, quality: 65}
      audio: {mode: copy}
      subtitles: {mode: preserve}
`,
				errSubstr: "unsupported video codec",
			},
			{
				name: "quality out of range low",
				yamlSnippet: `
transcode:
  profiles:
    low-qual:
      container: mkv
      video: {codec: hevc_videotoolbox, quality: 0}
      audio: {mode: copy}
      subtitles: {mode: preserve}
`,
				errSubstr: "quality",
			},
			{
				name: "quality out of range high",
				yamlSnippet: `
transcode:
  profiles:
    high-qual:
      container: mkv
      video: {codec: hevc_videotoolbox, quality: 101}
      audio: {mode: copy}
      subtitles: {mode: preserve}
`,
				errSubstr: "quality",
			},
			{
				name: "disallowed audio mode",
				yamlSnippet: `
transcode:
  profiles:
    bad-audio:
      container: mkv
      video: {codec: hevc_videotoolbox, quality: 65}
      audio: {mode: transcode}
      subtitles: {mode: preserve}
`,
				errSubstr: "unsupported audio mode",
			},
			{
				name: "disallowed subtitle mode",
				yamlSnippet: `
transcode:
  profiles:
    bad-subs:
      container: mkv
      video: {codec: hevc_videotoolbox, quality: 65}
      audio: {mode: copy}
      subtitles: {mode: strip}
`,
				errSubstr: "unsupported subtitles mode",
			},
			{
				name: "command injection in profile name",
				yamlSnippet: `
transcode:
  profiles:
    "hevc; rm -rf /":
      container: mkv
      video: {codec: hevc_videotoolbox, quality: 65}
      audio: {mode: copy}
      subtitles: {mode: preserve}
`,
				errSubstr: "invalid profile name",
			},
			{
				name: "unknown default_profile",
				yamlSnippet: `
transcode:
  default_profile: "unregistered_profile"
`,
				errSubstr: "default_profile",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				p := writeCfg("test_"+tc.name+".yaml", tc.yamlSnippet)
				_, err := Load(p)
				if err == nil {
					t.Fatalf("expected error loading invalid config, got nil")
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error containing %q, got: %v", tc.errSubstr, err)
				}
			})
		}
	})

	t.Run("local profile overrides preserve all VideoToolbox and optimization fields", func(t *testing.T) {
		p := writeCfg("override_fields.yaml", `
transcode:
  enabled: true
  executor: "ssh"
  ssh:
    host: "192.0.2.10"
    user: "transcoder"
    command: "/opt/homebrew/bin/navigatorr-transcode"
  profiles:
    custom-main10:
      container: mkv
      video:
        codec: hevc_videotoolbox
        quality: 68
        profile: main10
        pixel_format: p010le
        prioritize_speed: false
        spatial_aq: true
        realtime: false
      audio: {mode: copy}
      subtitles: {mode: preserve, convert_incompatible: true}
      preserve: {metadata: true, chapters: true, attachments: true}
      resilience:
        max_attempts: 2
      optimization:
        enabled: true
        sampling:
          strategy: uniform
          sample_count: 4
          sample_seconds: 12.0
          positions: [0.1, 0.4, 0.7, 0.9]
        quality:
          preferred_metric: vmaf
          vmaf:
            target: 96.0
            minimum: 93.0
            marginal_tolerance: 0.5
        search:
          max_candidates: 5
          quality_values: [60, 65, 70]
        size:
          preferred_total_bitrate_kbps:
            min: 2000
            max: 5000
          soft_max_total_bitrate_kbps: 6000
`)
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("failed loading config with local profile overrides: %v", err)
		}

		overrides := cfg.Transcode.recipeOverrides()
		recProfile, ok := overrides["custom-main10"]
		if !ok {
			t.Fatalf("missing custom-main10 in recipe overrides")
		}

		if recProfile.Video.Profile != "main10" {
			t.Errorf("expected Video.Profile=main10, got %s", recProfile.Video.Profile)
		}
		if recProfile.Video.PixelFormat != "p010le" {
			t.Errorf("expected Video.PixelFormat=p010le, got %s", recProfile.Video.PixelFormat)
		}
		if recProfile.Video.PrioritizeSpeed == nil || *recProfile.Video.PrioritizeSpeed != false {
			t.Errorf("expected PrioritizeSpeed=false, got %v", recProfile.Video.PrioritizeSpeed)
		}
		if recProfile.Video.SpatialAQ == nil || *recProfile.Video.SpatialAQ != true {
			t.Errorf("expected SpatialAQ=true, got %v", recProfile.Video.SpatialAQ)
		}
		if recProfile.Video.Realtime == nil || *recProfile.Video.Realtime != false {
			t.Errorf("expected Realtime=false, got %v", recProfile.Video.Realtime)
		}

		if recProfile.Optimization == nil {
			t.Fatalf("expected Optimization to be preserved, got nil")
		}
		if recProfile.Optimization.Sampling == nil || recProfile.Optimization.Sampling.SampleSeconds != 12.0 {
			t.Errorf("unexpected sampling policy: %+v", recProfile.Optimization.Sampling)
		}
		if recProfile.Optimization.Quality == nil || recProfile.Optimization.Quality.VMAF.Target != 96.0 || recProfile.Optimization.Quality.VMAF.MarginalTolerance == nil || *recProfile.Optimization.Quality.VMAF.MarginalTolerance != 0.5 {
			t.Errorf("unexpected quality policy: %+v", recProfile.Optimization.Quality)
		}
		if recProfile.Optimization.Search == nil || len(recProfile.Optimization.Search.QualityValues) != 3 || recProfile.Optimization.Search.QualityValues[1] != 65 {
			t.Errorf("unexpected search policy: %+v", recProfile.Optimization.Search)
		}
		if recProfile.Optimization.Size == nil || recProfile.Optimization.Size.PreferredTotalBitrateKbps.Max != 5000 {
			t.Errorf("unexpected size policy: %+v", recProfile.Optimization.Size)
		}

		// Verify deep cloning at mapping boundaries: mutating recProfile.Optimization
		// does NOT affect cfg.Transcode.Profiles["custom-main10"].Optimization
		recProfile.Optimization.Sampling.Positions[0] = 0.999
		if cfg.Transcode.Profiles["custom-main10"].Optimization.Sampling.Positions[0] == 0.999 {
			t.Errorf("aliasing detected: mutating recipe override mutated config profile")
		}

		plan, err := cfg.Transcode.ResolvePlan("custom-main10")
		if err != nil {
			t.Fatalf("failed to resolve custom-main10 plan: %v", err)
		}
		if plan.VideoProfile != "main10" || plan.PixelFormat != "p010le" || plan.ExpectedBitDepth != 10 {
			t.Errorf("plan missing resolved knobs: %+v", plan)
		}
	})
}
