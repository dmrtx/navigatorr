package transcode

// Availability is based on a real, bounded ASR fixture and audio filter/encoder
// execution. Installed locale names alone do not prove usable native timing.
type PodcastCapabilities struct {
	AdAlgorithm          string `json:"ad_algorithm,omitempty"`
	AutomaticKnownAds    bool   `json:"automatic_known_ads,omitempty"`
	Available            bool   `json:"available"`
	Provider             string `json:"provider"`
	ProviderVersion      string `json:"provider_version,omitempty"`
	VerifiedLanguage     string `json:"verified_language,omitempty"`
	NativeTimingVerified bool   `json:"native_timing_verified"`
	MP3RenderVerified    bool   `json:"mp3_render_verified"`
	Reason               string `json:"reason,omitempty"`
}
