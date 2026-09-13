package recipe

import "time"

const SupportedSchemaVersion = 1

type Bundle struct {
	SchemaVersion int                      `json:"schema_version" yaml:"schema_version"`
	BundleVersion string                   `json:"bundle_version" yaml:"bundle_version"`
	Containers    map[string]ContainerRule `json:"containers" yaml:"containers"`
	Profiles      map[string]Profile       `json:"profiles" yaml:"profiles"`
}

type ContainerRule struct {
	SubtitleCopy        []string                  `json:"subtitle_copy" yaml:"subtitle_copy"`
	SubtitleConversions map[string]ConversionRule `json:"subtitle_conversions" yaml:"subtitle_conversions"`
}

type ConversionRule struct {
	TargetCodec string `json:"target_codec" yaml:"target_codec"`
	Reason      string `json:"reason" yaml:"reason"`
}

type Profile struct {
	Container  string            `json:"container" yaml:"container"`
	Video      VideoProfile      `json:"video" yaml:"video"`
	Audio      AudioProfile      `json:"audio" yaml:"audio"`
	Subtitles  SubtitleProfile   `json:"subtitles" yaml:"subtitles"`
	Preserve   PreserveProfile   `json:"preserve" yaml:"preserve"`
	Resilience ResilienceProfile `json:"resilience" yaml:"resilience"`
}

type VideoProfile struct {
	Codec           string `json:"codec" yaml:"codec"`
	Quality         int    `json:"quality" yaml:"quality"`
	Profile         string `json:"profile,omitempty" yaml:"profile,omitempty"`
	PixelFormat     string `json:"pixel_format,omitempty" yaml:"pixel_format,omitempty"`
	PrioritizeSpeed *bool  `json:"prioritize_speed,omitempty" yaml:"prioritize_speed,omitempty"`
	SpatialAQ       *bool  `json:"spatial_aq,omitempty" yaml:"spatial_aq,omitempty"`
	Realtime        *bool  `json:"realtime,omitempty" yaml:"realtime,omitempty"`
}

type AudioProfile struct {
	Mode string `json:"mode" yaml:"mode"`
}
type SubtitleProfile struct {
	Mode                string `json:"mode" yaml:"mode"`
	ConvertIncompatible bool   `json:"convert_incompatible" yaml:"convert_incompatible"`
}
type PreserveProfile struct {
	Metadata    bool `json:"metadata" yaml:"metadata"`
	Chapters    bool `json:"chapters" yaml:"chapters"`
	Attachments bool `json:"attachments" yaml:"attachments"`
}

type ResilienceProfile struct {
	MaxAttempts         int            `json:"max_attempts" yaml:"max_attempts"`
	TransientRetries    int            `json:"transient_retries" yaml:"transient_retries"`
	RetryBackoffSeconds []int          `json:"retry_backoff_seconds" yaml:"retry_backoff_seconds"`
	MaxFallbacks        int            `json:"max_fallbacks" yaml:"max_fallbacks"`
	Fallbacks           []FallbackRule `json:"fallbacks" yaml:"fallbacks"`
}

type FallbackRule struct {
	When   string `json:"when" yaml:"when"`
	Action string `json:"action" yaml:"action"`
}

type SourceSubtitle struct {
	SourceStreamIndex int
	TypeIndex         int
	Codec             string
}

type Identity struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type Snapshot struct {
	Bundle   Bundle   `json:"-"`
	Identity Identity `json:"identity"`
	Raw      []byte   `json:"-"`
}

type Status struct {
	Source          string    `json:"source"`
	Channel         string    `json:"channel,omitempty"`
	Revision        string    `json:"revision,omitempty"`
	ActiveVersion   string    `json:"active_version"`
	ActiveDigest    string    `json:"active_digest"`
	LastKnownGood   string    `json:"last_known_good"`
	LastCheckedAt   time.Time `json:"last_checked_at,omitempty"`
	LastUpdateError string    `json:"last_update_error,omitempty"`
}

type Manifest struct {
	SchemaVersion            int    `json:"schema_version"`
	BundleVersion            string `json:"bundle_version"`
	BundleSHA256             string `json:"bundle_sha256"`
	DownloadURL              string `json:"download_url"`
	MinimumNavigatorrVersion string `json:"minimum_navigatorr_version,omitempty"`
	PublishedAt              string `json:"published_at,omitempty"`
}
