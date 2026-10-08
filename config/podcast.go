package config

import (
	"fmt"
	"github.com/jakenesler/navigatorr/podcast"
	"path/filepath"
)

type PodcastConfig struct {
	Enabled         bool                       `yaml:"enabled"`
	ArtifactDir     string                     `yaml:"artifact_dir"`
	LocalCatalogDir string                     `yaml:"local_catalog_dir"`
	Podcasts        map[string]PodcastSettings `yaml:"podcasts"`
}
type PodcastSettings struct {
	KnownAdsFirstPass  bool     `yaml:"known_ads_first_pass"`
	Enabled            bool     `yaml:"enabled"`
	Language           string   `yaml:"language"`
	Remove             []string `yaml:"remove"`
	WindowMS           int64    `yaml:"window_ms"`
	OverlapMS          *int64   `yaml:"overlap_ms"`
	MaxRemovedFraction float64  `yaml:"max_removed_fraction"`
	ReviewRequired     *bool    `yaml:"review_required"`
}

func (c PodcastConfig) Policy(id string) (podcast.Policy, error) {
	p := podcast.DefaultPolicy()
	s, ok := c.Podcasts[id]
	p.KnownAdsFirstPass = s.KnownAdsFirstPass
	if !c.Enabled || !ok || !s.Enabled {
		return p, fmt.Errorf("podcast %q is not enabled", id)
	}
	if s.Language != "" {
		p.Language = s.Language
	}
	if s.Remove != nil {
		p.Remove = append([]string{}, s.Remove...)
	}
	if s.WindowMS != 0 {
		p.WindowMS = s.WindowMS
		p.OverlapMS = p.WindowMS / 8
	}
	if s.OverlapMS != nil {
		p.OverlapMS = *s.OverlapMS
	}
	if s.MaxRemovedFraction != 0 {
		p.MaxRemovedFraction = s.MaxRemovedFraction
	}
	if s.ReviewRequired != nil {
		p.ReviewRequired = *s.ReviewRequired
	}
	return p, p.Validate()
}
func (c PodcastConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !filepath.IsAbs(c.ArtifactDir) || c.ArtifactDir == "/" {
		return fmt.Errorf("podcasts.artifact_dir must be an absolute private checkpoint directory")
	}
	if c.LocalCatalogDir != "" && (!filepath.IsAbs(c.LocalCatalogDir) || c.LocalCatalogDir == "/") {
		return fmt.Errorf("podcasts.local_catalog_dir must be an absolute private shared directory")
	}
	for id, s := range c.Podcasts {
		if id == "" {
			return fmt.Errorf("podcast id is empty")
		}
		if s.Enabled {
			if _, err := c.Policy(id); err != nil {
				return err
			}
		}
	}
	return nil
}
