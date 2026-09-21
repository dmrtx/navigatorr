package config

import (
	"bytes"
	"context"
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
	"github.com/jakenesler/navigatorr/transcode/recipe"
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
	MCP               MCPConfig                `yaml:"mcp"`
	MaxResponseSizeKB int                      `yaml:"max_response_size_kb"`
	AllowDestructive  bool                     `yaml:"allow_destructive"`
	LoadedPath        string                   `yaml:"-"`
}

type MCPConfig struct {
	Transport string `yaml:"transport"`
	Listen    string `yaml:"listen"`
}

type ConcurrencyConfig struct {
	MaxAPISimultaneous     int `yaml:"max_api_simultaneous"`
	MaxInspectSimultaneous int `yaml:"max_inspect_simultaneous"`
}
type DatabaseConfig struct {
	Path string `yaml:"path"`
}
type MediaConfig struct {
	AllowedReadRoots  []string `yaml:"allowed_read_roots"`
	AllowedWriteRoots []string `yaml:"allowed_write_roots"`
	FfprobePath       string   `yaml:"ffprobe_path"`
}
type MaintenanceConfig struct {
	OversizedPerEpisodeMB int64    `yaml:"oversized_per_episode_mb"`
	PreferredGroups       []string `yaml:"preferred_groups"`
	PreferredResolution   string   `yaml:"preferred_resolution"`
}
type ServiceConfig struct {
	URL        string `yaml:"url"`
	APIKey     string `yaml:"api_key"`
	AuthMethod string `yaml:"auth_method"`
	AuthHeader string `yaml:"auth_header"`
	AuthPrefix string `yaml:"auth_prefix"`
	APIVersion string `yaml:"api_version"`
	OpenAPIURL string `yaml:"openapi_url"`
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
	URLBase string `yaml:"url_base"`
}

type TranscodeConfig struct {
	Enabled           bool              `yaml:"enabled"`
	Executor          string            `yaml:"executor"`
	DefaultAction     string            `yaml:"default_action"`
	DefaultProfile    string            `yaml:"default_profile"`
	MinSavingsPercent float64           `yaml:"min_savings_percent"`
	MaxParallelJobs   int               `yaml:"max_parallel_jobs"`
	SSH               SSHExecutorConfig `yaml:"ssh"`
	// HTTP configures the dark/non-default persistent-daemon transport.
	// Canonical key is `http` (consistent with `ssh`); `worker_http` is
	// accepted as an alias and merged (explicit `http` wins).
	HTTP          HTTPExecutorConfig                `yaml:"http"`
	WorkerHTTP    HTTPExecutorConfig                `yaml:"worker_http"`
	Recipes       TranscodeRecipeConfig             `yaml:"recipes"`
	Profiles      map[string]TranscodeProfileConfig `yaml:"profiles"`
	recipeManager *recipe.Manager                   `yaml:"-"`
}

type TranscodeRecipeConfig struct {
	Source          string `yaml:"source"`
	Path            string `yaml:"path"`
	ManifestURL     string `yaml:"manifest_url"`
	Repository      string `yaml:"repository"`
	Channel         string `yaml:"channel"`
	Revision        string `yaml:"revision"`
	RefreshInterval string `yaml:"refresh_interval"`
	CacheDir        string `yaml:"cache_dir"`
}

type TranscodeProfileConfig struct {
	Container    string                     `yaml:"container"`
	Video        VideoProfileConfig         `yaml:"video"`
	Audio        AudioProfileConfig         `yaml:"audio"`
	Subtitles    SubtitleProfileConfig      `yaml:"subtitles"`
	Preserve     PreserveProfileConfig      `yaml:"preserve"`
	Resilience   ResilienceProfileConfig    `yaml:"resilience,omitempty"`
	Optimization *recipe.OptimizationPolicy `yaml:"optimization,omitempty"`
}
type VideoProfileConfig struct {
	Codec              string `yaml:"codec"`
	Quality            int    `yaml:"quality"`
	Preset             string `yaml:"preset,omitempty"`
	Tune               string `yaml:"tune,omitempty"`
	Profile            string `yaml:"profile,omitempty"`
	PixelFormat        string `yaml:"pixel_format,omitempty"`
	PrioritizeSpeed    *bool  `yaml:"prioritize_speed,omitempty"`
	SpatialAQ          *bool  `yaml:"spatial_aq,omitempty"`
	Realtime           *bool  `yaml:"realtime,omitempty"`
	AverageBitrateKbps int    `yaml:"average_bitrate_kbps,omitempty"`
	MaxBitrateKbps     int    `yaml:"max_bitrate_kbps,omitempty"`
	ConstantBitrate    *bool  `yaml:"constant_bitrate,omitempty"`
	QMin               *int   `yaml:"qmin,omitempty"`
	QMax               *int   `yaml:"qmax,omitempty"`
	GOPSize            *int   `yaml:"gop_size,omitempty"`
	BFrames            *int   `yaml:"b_frames,omitempty"`
	ClosedGOP          *bool  `yaml:"closed_gop,omitempty"`
	PowerEfficient     *bool  `yaml:"power_efficient,omitempty"`
	MaxRefFrames       *int   `yaml:"max_ref_frames,omitempty"`
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
type ResilienceProfileConfig struct {
	MaxAttempts         int                   `yaml:"max_attempts"`
	TransientRetries    int                   `yaml:"transient_retries"`
	RetryBackoffSeconds []int                 `yaml:"retry_backoff_seconds"`
	MaxFallbacks        int                   `yaml:"max_fallbacks"`
	Fallbacks           []recipe.FallbackRule `yaml:"fallbacks"`
}

var validProfileNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
var validRepositoryRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var validRevisionRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func DefaultRecipeCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "navigatorr", "transcode-recipes")
}

func (t *TranscodeConfig) RecipeManager() *recipe.Manager { return t.recipeManager }

func (t *TranscodeConfig) recipeOverrides() map[string]recipe.Profile {
	if len(t.Profiles) == 0 {
		return nil
	}
	out := make(map[string]recipe.Profile, len(t.Profiles))
	for name, p := range t.Profiles {
		out[name] = profileToRecipe(name, p)
	}
	return out
}

// resolvedRecipeOverrides composes the three recipe layers used by new jobs:
// active bundle < static config overrides < centrally managed profiles.
// Managed profiles intentionally win so day-to-day recipe changes do not
// require editing deployment configuration. Running jobs are unaffected
// because they already hold an immutable resolved transcode.Plan.
func (t *TranscodeConfig) resolvedRecipeOverrides() (map[string]recipe.Profile, map[string]recipe.ManagedProfileRecord, error) {
	out := t.recipeOverrides()
	if out == nil {
		out = make(map[string]recipe.Profile)
	}
	managed := make(map[string]recipe.ManagedProfileRecord)
	if t.recipeManager == nil {
		return out, managed, nil
	}
	records, err := t.recipeManager.ListManagedProfiles()
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range records {
		out[rec.Name] = rec.Profile
		managed[rec.Name] = rec
	}
	return out, managed, nil
}

func profileToRecipe(name string, p TranscodeProfileConfig) recipe.Profile {
	r := recipe.ResilienceProfile{MaxAttempts: p.Resilience.MaxAttempts, TransientRetries: p.Resilience.TransientRetries, RetryBackoffSeconds: append([]int(nil), p.Resilience.RetryBackoffSeconds...), MaxFallbacks: p.Resilience.MaxFallbacks, Fallbacks: append([]recipe.FallbackRule(nil), p.Resilience.Fallbacks...)}
	// Backward-compatible local profiles from PR #16 did not contain a resilience block.
	// Preserve their explicit convert_incompatible intent with one bounded conversion fallback,
	// while keeping retries disabled unless the local profile opts in.
	if r.MaxAttempts == 0 {
		r.MaxAttempts = 1
	}
	if p.Subtitles.ConvertIncompatible && r.MaxFallbacks == 0 && len(r.Fallbacks) == 0 {
		r.MaxFallbacks = 1
		r.Fallbacks = []recipe.FallbackRule{{When: "container_subtitle_incompatible", Action: "apply_container_conversion"}}
	}
	return recipe.Profile{
		Container: p.Container,
		Video: recipe.VideoProfile{
			Codec:              p.Video.Codec,
			Quality:            p.Video.Quality,
			Preset:             p.Video.Preset,
			Tune:               p.Video.Tune,
			Profile:            p.Video.Profile,
			PixelFormat:        p.Video.PixelFormat,
			PrioritizeSpeed:    p.Video.PrioritizeSpeed,
			SpatialAQ:          p.Video.SpatialAQ,
			Realtime:           p.Video.Realtime,
			AverageBitrateKbps: p.Video.AverageBitrateKbps,
			MaxBitrateKbps:     p.Video.MaxBitrateKbps,
			ConstantBitrate:    p.Video.ConstantBitrate,
			QMin:               p.Video.QMin,
			QMax:               p.Video.QMax,
			GOPSize:            p.Video.GOPSize,
			BFrames:            p.Video.BFrames,
			ClosedGOP:          p.Video.ClosedGOP,
			PowerEfficient:     p.Video.PowerEfficient,
			MaxRefFrames:       p.Video.MaxRefFrames,
		},
		Audio:      recipe.AudioProfile{Mode: p.Audio.Mode},
		Subtitles:  recipe.SubtitleProfile{Mode: p.Subtitles.Mode, ConvertIncompatible: p.Subtitles.ConvertIncompatible},
		Preserve:   recipe.PreserveProfile{Metadata: p.Preserve.Metadata, Chapters: p.Preserve.Chapters, Attachments: p.Preserve.Attachments},
		Resilience: r,
		Optimization: func() *recipe.OptimizationPolicy {
			if p.Optimization == nil {
				return nil
			}
			opt := p.Optimization.Clone()
			if opt.Enabled {
				recipe.NormalizeOptimizationPolicy(opt)
			}
			return opt
		}(),
	}
}

func recipeToProfile(p recipe.Profile) TranscodeProfileConfig {
	return TranscodeProfileConfig{
		Container: p.Container,
		Video: VideoProfileConfig{
			Codec:              p.Video.Codec,
			Quality:            p.Video.Quality,
			Preset:             p.Video.Preset,
			Tune:               p.Video.Tune,
			Profile:            p.Video.Profile,
			PixelFormat:        p.Video.PixelFormat,
			PrioritizeSpeed:    p.Video.PrioritizeSpeed,
			SpatialAQ:          p.Video.SpatialAQ,
			Realtime:           p.Video.Realtime,
			AverageBitrateKbps: p.Video.AverageBitrateKbps,
			MaxBitrateKbps:     p.Video.MaxBitrateKbps,
			ConstantBitrate:    p.Video.ConstantBitrate,
			QMin:               p.Video.QMin,
			QMax:               p.Video.QMax,
			GOPSize:            p.Video.GOPSize,
			BFrames:            p.Video.BFrames,
			ClosedGOP:          p.Video.ClosedGOP,
			PowerEfficient:     p.Video.PowerEfficient,
			MaxRefFrames:       p.Video.MaxRefFrames,
		},
		Audio:        AudioProfileConfig{Mode: p.Audio.Mode},
		Subtitles:    SubtitleProfileConfig{Mode: p.Subtitles.Mode, ConvertIncompatible: p.Subtitles.ConvertIncompatible},
		Preserve:     PreserveProfileConfig{Metadata: p.Preserve.Metadata, Chapters: p.Preserve.Chapters, Attachments: p.Preserve.Attachments},
		Resilience:   ResilienceProfileConfig{MaxAttempts: p.Resilience.MaxAttempts, TransientRetries: p.Resilience.TransientRetries, RetryBackoffSeconds: append([]int(nil), p.Resilience.RetryBackoffSeconds...), MaxFallbacks: p.Resilience.MaxFallbacks, Fallbacks: append([]recipe.FallbackRule(nil), p.Resilience.Fallbacks...)},
		Optimization: p.Optimization.Clone(),
	}
}

// BuiltinTranscodeProfiles is retained for API compatibility, but its single source of truth is the embedded recipe bundle.
func BuiltinTranscodeProfiles() map[string]TranscodeProfileConfig {
	snap, err := recipe.Parse(recipe.EmbeddedBytes())
	if err != nil {
		return map[string]TranscodeProfileConfig{}
	}
	out := make(map[string]TranscodeProfileConfig, len(snap.Bundle.Profiles))
	for name, p := range snap.Bundle.Profiles {
		out[name] = recipeToProfile(p)
	}
	return out
}

func (t *TranscodeConfig) activeSnapshot() (*recipe.Snapshot, error) {
	if t.recipeManager != nil {
		if s := t.recipeManager.Snapshot(); s != nil {
			return s, nil
		}
	}
	return recipe.Parse(recipe.EmbeddedBytes())
}

func (t *TranscodeConfig) ResolvePlan(profileName string) (*transcode.Plan, error) {
	return t.ResolvePlanForSource(profileName, nil)
}

func (t *TranscodeConfig) resolvedProfileContext(profileName string) (string, *recipe.Snapshot, map[string]recipe.Profile, map[string]recipe.ManagedProfileRecord, recipe.Profile, error) {
	name := strings.TrimSpace(profileName)
	if name == "" {
		name = strings.TrimSpace(t.DefaultProfile)
	}
	if name == "" {
		name = "hevc-vt"
	}
	snap, err := t.activeSnapshot()
	if err != nil {
		return "", nil, nil, nil, recipe.Profile{}, err
	}
	overrides, managed, err := t.resolvedRecipeOverrides()
	if err != nil {
		return "", nil, nil, nil, recipe.Profile{}, fmt.Errorf("reading managed transcode profiles: %w", err)
	}
	p, ok := snap.Bundle.Profiles[name]
	if op, exists := overrides[name]; exists {
		p = op
		ok = true
	}
	if !ok {
		return "", nil, nil, nil, recipe.Profile{}, fmt.Errorf("unknown transcode profile %q", name)
	}
	if err := recipe.ValidateProfile(name, p); err != nil {
		return "", nil, nil, nil, recipe.Profile{}, err
	}
	return name, snap, overrides, managed, p, nil
}

// ResolvePlanAndProfileForSource returns the exact validated profile and the
// immutable plan derived from the same recipe snapshot/managed-registry read.
// This prevents a managed profile update between plan resolution and
// optimization-policy lookup from mixing two generations inside one action.
func (t *TranscodeConfig) ResolvePlanAndProfileForSource(profileName string, subtitles []recipe.SourceSubtitle) (*transcode.Plan, recipe.Profile, error) {
	name, snap, overrides, managed, p, err := t.resolvedProfileContext(profileName)
	if err != nil {
		return nil, recipe.Profile{}, err
	}
	plan, err := recipe.Resolve(snap, name, overrides, subtitles)
	if err != nil {
		return nil, recipe.Profile{}, err
	}
	if rec, ok := managed[name]; ok {
		plan.RecipeVersion = fmt.Sprintf("managed:%s:%d", name, rec.Generation)
		plan.RecipeDigest = rec.Digest
		plan.PlanDigest = ""
		digest, err := transcode.DigestPlan(plan)
		if err != nil {
			return nil, recipe.Profile{}, err
		}
		plan.PlanDigest = digest
	}
	return plan, p, nil
}

func (t *TranscodeConfig) ResolvePlanForSource(profileName string, subtitles []recipe.SourceSubtitle) (*transcode.Plan, error) {
	plan, _, err := t.ResolvePlanAndProfileForSource(profileName, subtitles)
	return plan, err
}

// ResolveEphemeralPlanForSource validates a complete one-action profile,
// resolves it through the same container/subtitle compatibility machinery as
// durable recipes, and stamps a content identity that can be tracked or later
// persisted through recipe_save. It never mutates the active recipe registry.
func (t *TranscodeConfig) ResolveEphemeralPlanForSource(profile recipe.Profile, subtitles []recipe.SourceSubtitle) (*transcode.Plan, recipe.Profile, string, error) {
	const name = "ephemeral"
	normalized, digest, err := recipe.NormalizeAndDigestProfile(name, profile)
	if err != nil {
		return nil, recipe.Profile{}, "", err
	}
	snap, err := t.activeSnapshot()
	if err != nil {
		return nil, recipe.Profile{}, "", err
	}
	plan, err := recipe.Resolve(snap, name, map[string]recipe.Profile{name: normalized}, subtitles)
	if err != nil {
		return nil, recipe.Profile{}, "", err
	}
	plan.RecipeVersion = "ephemeral"
	plan.RecipeDigest = digest
	plan.PlanDigest = ""
	planDigest, err := transcode.DigestPlan(plan)
	if err != nil {
		return nil, recipe.Profile{}, "", err
	}
	plan.PlanDigest = planDigest
	return plan, normalized, digest, nil
}

// ResolveProfile resolves and validates a recipe Profile for the given profileName.
// Precedence is active bundle < static config override < centrally managed profile.
func (t *TranscodeConfig) ResolveProfile(profileName string) (recipe.Profile, error) {
	_, _, _, _, p, err := t.resolvedProfileContext(profileName)
	return p, err
}

func (t *TranscodeConfig) validateRecipeSource() error {
	src := strings.ToLower(strings.TrimSpace(t.Recipes.Source))
	if src == "" {
		src = "builtin"
		t.Recipes.Source = src
	}
	if t.Recipes.CacheDir == "" {
		t.Recipes.CacheDir = DefaultRecipeCacheDir()
	}
	if t.Recipes.RefreshInterval != "" {
		d, err := time.ParseDuration(t.Recipes.RefreshInterval)
		if err != nil || d < time.Minute {
			return fmt.Errorf("transcode recipes: refresh_interval must be a valid duration >= 1m")
		}
	}
	switch src {
	case "builtin":
		if t.Recipes.Path != "" || t.Recipes.ManifestURL != "" || t.Recipes.Repository != "" {
			return fmt.Errorf("transcode recipes: builtin source does not accept path/manifest_url/repository")
		}
	case "file":
		if strings.TrimSpace(t.Recipes.Path) == "" {
			return fmt.Errorf("transcode recipes: file source requires path")
		}
	case "https":
		u, err := url.Parse(strings.TrimSpace(t.Recipes.ManifestURL))
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("transcode recipes: https source requires an absolute https manifest_url without userinfo")
		}
	case "github":
		if !validRepositoryRegex.MatchString(strings.TrimSpace(t.Recipes.Repository)) {
			return fmt.Errorf("transcode recipes: github repository must be owner/name")
		}
		ch, rev := strings.TrimSpace(t.Recipes.Channel), strings.TrimSpace(t.Recipes.Revision)
		if ch != "" && rev != "" {
			return fmt.Errorf("transcode recipes: github channel and revision are mutually exclusive")
		}
		if ch == "" && rev == "" {
			ch = "stable"
			t.Recipes.Channel = ch
		}
		if ch != "" && ch != "stable" && ch != "beta" {
			return fmt.Errorf("transcode recipes: github channel must be stable or beta")
		}
		if rev != "" {
			if !validRevisionRegex.MatchString(rev) || strings.EqualFold(rev, "main") || strings.EqualFold(rev, "master") {
				return fmt.Errorf("transcode recipes: revision must be a pinned tag/sha-like token, not main/master")
			}
		}
	default:
		return fmt.Errorf("transcode recipes: unsupported source %q", src)
	}
	return nil
}

func (t *TranscodeConfig) InitializeRecipes(ctx context.Context) error {
	if err := t.validateRecipeSource(); err != nil {
		return err
	}
	src := strings.ToLower(strings.TrimSpace(t.Recipes.Source))
	var provider recipe.Provider
	switch src {
	case "builtin":
		provider = recipe.BuiltinProvider{}
	case "file":
		provider = recipe.FileProvider{Path: t.Recipes.Path}
	case "https":
		provider = &recipe.HTTPManifestProvider{ManifestURL: t.Recipes.ManifestURL, NavigatorrVersion: "1.0.0"}
	case "github":
		provider = recipe.NewGitHubProvider(t.Recipes.Repository, t.Recipes.Channel, t.Recipes.Revision, "1.0.0")
	}
	mgr, err := recipe.NewManager(provider, t.Recipes.CacheDir, t.Recipes.Channel, t.Recipes.Revision)
	if err != nil {
		return err
	}
	t.recipeManager = mgr
	if src != "builtin" {
		_, _ = mgr.Update(ctx)
	}
	if t.Recipes.RefreshInterval != "" && src != "builtin" {
		if d, err := time.ParseDuration(t.Recipes.RefreshInterval); err == nil && d > 0 {
			mgr.StartAutoRefresh(context.Background(), d)
		}
	}
	if t.DefaultProfile != "" && t.DefaultProfile != "auto" {
		if _, err := t.ResolvePlan(t.DefaultProfile); err != nil {
			return fmt.Errorf("transcode: default_profile %q is not defined in the active recipe/local overrides: %w", t.DefaultProfile, err)
		}
	}
	return nil
}

func (t *TranscodeConfig) Validate() error {
	if err := t.validateRecipeSource(); err != nil {
		return err
	}
	if strings.EqualFold(strings.TrimSpace(t.Executor), "http") {
		if err := t.EffectiveHTTPConfig().ValidateHTTPConfig(); err != nil {
			return err
		}
	} else if hc := t.EffectiveHTTPConfig(); strings.TrimSpace(hc.BaseURL) != "" {
		// A configured-but-dormant http block must still be well-formed so
		// enabling it later cannot surprise production.
		if err := hc.ValidateHTTPConfig(); err != nil {
			return err
		}
	}
	for name, p := range t.Profiles {
		if !validProfileNameRegex.MatchString(name) {
			return fmt.Errorf("transcode: invalid profile name %q (allowed characters: letters, numbers, dash, underscore)", name)
		}
		if err := recipe.ValidateProfile(name, profileToRecipe(name, p)); err != nil {
			return fmt.Errorf("transcode profile %q: %w", name, err)
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

type HTTPExecutorConfig struct {
	// BaseURL is the daemon origin, e.g. "http://192.0.2.10:8097".
	// The "/v1" prefix is appended by the executor; do not include it.
	BaseURL string `yaml:"base_url"`
	// Token is the bearer credential. Empty means no Authorization header
	// (only appropriate for loopback daemons).
	Token string `yaml:"token"`
	// TokenFile is read once at executor construction when Token is empty.
	TokenFile string `yaml:"token_file"`
	// RequestTimeoutSec bounds Doctor/Capabilities/Status/Cancel (default 15s).
	RequestTimeoutSec int `yaml:"request_timeout_sec"`
	// SubmitTimeoutSec bounds Submit/benchmark-submit POSTs (default 60s).
	// It bounds transport only, never the encode itself.
	SubmitTimeoutSec int                    `yaml:"submit_timeout_sec"`
	PathMappings     []TranscodePathMapping `yaml:"path_mappings"`
}

// EffectiveHTTPConfig merges the `http` (canonical) and `worker_http`
// (alias) blocks. Explicit `http.base_url` wins; otherwise the alias is
// used verbatim.
func (t *TranscodeConfig) EffectiveHTTPConfig() HTTPExecutorConfig {
	if strings.TrimSpace(t.HTTP.BaseURL) != "" {
		return t.HTTP
	}
	return t.WorkerHTTP
}

func (h HTTPExecutorConfig) RequestTimeoutDuration() time.Duration {
	if h.RequestTimeoutSec > 0 {
		return time.Duration(h.RequestTimeoutSec) * time.Second
	}
	return 15 * time.Second
}

func (h HTTPExecutorConfig) SubmitTimeoutDuration() time.Duration {
	if h.SubmitTimeoutSec > 0 {
		return time.Duration(h.SubmitTimeoutSec) * time.Second
	}
	return 60 * time.Second
}

// BuildHTTPExecutorConfig maps the effective http/worker_http block onto
// the transcode.HTTPConfig used by NewHTTPExecutor. It is the single
// construction site shared by production wiring (main.go) and tests.
func (t *TranscodeConfig) BuildHTTPExecutorConfig() (transcode.HTTPConfig, error) {
	hc := t.EffectiveHTTPConfig()
	if err := hc.ValidateHTTPConfig(); err != nil {
		return transcode.HTTPConfig{}, err
	}
	mappings := make([]transcode.PathMapping, len(hc.PathMappings))
	for i, m := range hc.PathMappings {
		mappings[i] = transcode.PathMapping{Local: m.GetLocal(), Remote: m.GetRemote()}
	}
	return transcode.HTTPConfig{
		BaseURL:        strings.TrimSpace(hc.BaseURL),
		Token:          strings.TrimSpace(hc.Token),
		TokenFile:      strings.TrimSpace(hc.TokenFile),
		RequestTimeout: hc.RequestTimeoutDuration(),
		SubmitTimeout:  hc.SubmitTimeoutDuration(),
		PathMappings:   mappings,
	}, nil
}

// ValidateHTTPConfig fail-closes on malformed daemon configuration. It is
// enforced at load time only when the http executor is selected, so
// existing ssh-only configs are unaffected.
func (h HTTPExecutorConfig) ValidateHTTPConfig() error {
	base := strings.TrimSpace(h.BaseURL)
	if base == "" {
		return fmt.Errorf("transcode http: base_url is required when executor is http")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("transcode http: base_url %q must be an absolute http(s) URL", h.BaseURL)
	}
	if h.RequestTimeoutSec < 0 || h.SubmitTimeoutSec < 0 {
		return fmt.Errorf("transcode http: timeouts must not be negative")
	}
	return nil
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

type QueueConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
	Path   string `yaml:"path"`
}

func DefaultQueuePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "navigatorr", "queue.json")
}
func DefaultDatabasePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "navigatorr", "navigatorr.db")
}

var notFoundFieldRegex = regexp.MustCompile(`field\s+([a-zA-Z0-9_-]+)\s+not\s+found\s+in\s+type\s+([a-zA-Z0-9_.]+)`)
var structTypeToSection = map[string]string{
	"config.MediaConfig": "media", "MediaConfig": "media", "config.MaintenanceConfig": "maintenance", "MaintenanceConfig": "maintenance", "config.ConcurrencyConfig": "concurrency", "ConcurrencyConfig": "concurrency", "config.MCPConfig": "mcp", "MCPConfig": "mcp", "config.DatabaseConfig": "database", "DatabaseConfig": "database", "config.QueueConfig": "queue", "QueueConfig": "queue", "config.TransmissionConfig": "transmission", "TransmissionConfig": "transmission", "config.QBittorrentConfig": "qbittorrent", "QBittorrentConfig": "qbittorrent", "config.SABnzbdConfig": "sabnzbd", "SABnzbdConfig": "sabnzbd", "config.TranscodeConfig": "transcode", "TranscodeConfig": "transcode", "config.TranscodeRecipeConfig": "transcode.recipes", "TranscodeRecipeConfig": "transcode.recipes", "config.SSHExecutorConfig": "transcode", "SSHExecutorConfig": "transcode", "config.HTTPExecutorConfig": "transcode", "HTTPExecutorConfig": "transcode", "config.TranscodePathMapping": "transcode", "TranscodePathMapping": "transcode", "config.TranscodeProfileConfig": "transcode.profiles", "TranscodeProfileConfig": "transcode.profiles", "config.VideoProfileConfig": "transcode.profiles.video", "VideoProfileConfig": "transcode.profiles.video", "config.AudioProfileConfig": "transcode.profiles.audio", "AudioProfileConfig": "transcode.profiles.audio", "config.SubtitleProfileConfig": "transcode.profiles.subtitles", "SubtitleProfileConfig": "transcode.profiles.subtitles", "config.PreserveProfileConfig": "transcode.profiles.preserve", "PreserveProfileConfig": "transcode.profiles.preserve", "config.ResilienceProfileConfig": "transcode.profiles.resilience", "ResilienceProfileConfig": "transcode.profiles.resilience", "config.ServiceConfig": "services", "ServiceConfig": "services",
}
var topLevelKeys = map[string]bool{"services": true, "transmission": true, "qbittorrent": true, "sabnzbd": true, "transcode": true, "queue": true, "database": true, "media": true, "maintenance": true, "concurrency": true, "mcp": true, "max_response_size_kb": true, "allow_destructive": true}

type TransportOptions struct {
	Transport string // "stdio" or "streamable-http"
	Listen    string // e.g. "127.0.0.1:8098"
}

// ResolveTransport resolves MCP transport options from CLI flags and configuration.
// Precedence: CLI flag > Config value > Defaults.
// Defaults: Transport="stdio", Listen="127.0.0.1:8098".
func ResolveTransport(cfg *Config, flagTransport, flagListen string) (TransportOptions, error) {
	opts := TransportOptions{
		Transport: "stdio",
		Listen:    "127.0.0.1:8098",
	}
	if cfg != nil {
		if t := strings.TrimSpace(cfg.MCP.Transport); t != "" {
			opts.Transport = strings.ToLower(t)
		}
		if l := strings.TrimSpace(cfg.MCP.Listen); l != "" {
			opts.Listen = l
		}
	}
	if t := strings.TrimSpace(flagTransport); t != "" {
		opts.Transport = strings.ToLower(t)
	}
	if l := strings.TrimSpace(flagListen); l != "" {
		opts.Listen = l
	}

	switch opts.Transport {
	case "stdio", "streamable-http":
		// valid
	default:
		return opts, fmt.Errorf("invalid transport %q: must be \"stdio\" or \"streamable-http\"", opts.Transport)
	}

	if opts.Transport == "streamable-http" {
		if opts.Listen == "" {
			opts.Listen = "127.0.0.1:8098"
		}
	}

	return opts, nil
}

func formatConfigError(path string, err error) error {
	errMsg := err.Error()
	matches := notFoundFieldRegex.FindStringSubmatch(errMsg)
	if len(matches) == 3 {
		field, typeName := matches[1], matches[2]
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
	cfg := &Config{Services: make(map[string]ServiceConfig)}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, formatConfigError(path, err)
	}
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
				svc.OpenAPIURL = strings.TrimSuffix(svc.URL, "/") + p
			}
		}
		cfg.Services[name] = svc
	}
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
	// Automatic transcode execution is HTTP-only. When transcode is enabled but
	// no executor is given, fail over to the HTTP daemon rather than the retired
	// SSH transport. SSH remains parseable for admin/backward compatibility but
	// is never selected implicitly. Validation below then enforces the HTTP
	// block, so a missing/unusable base_url still fails closed at load time.
	if cfg.Transcode.Enabled && strings.TrimSpace(cfg.Transcode.Executor) == "" {
		cfg.Transcode.Executor = DefaultTranscodeExecutor
	}
	if err := cfg.Transcode.Validate(); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if err := cfg.Transcode.InitializeRecipes(context.Background()); err != nil {
		return nil, fmt.Errorf("initializing transcode recipes: %w", err)
	}
	cfg.LoadedPath = path
	return cfg, nil
}

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
