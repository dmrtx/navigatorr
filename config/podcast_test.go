package config

import (
	"path/filepath"
	"testing"
)

func TestPodcastOptInDefaultsAndInvalidPolicy(t *testing.T) {
	c := PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(t.TempDir(), "artifacts"), Podcasts: map[string]PodcastSettings{"genwhy": {Enabled: true}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := c.Policy("genwhy")
	if err != nil || p.Language != "en_US" || !p.ReviewRequired || p.WindowMS != 240000 || p.OverlapMS != 30000 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := c.Policy("unknown"); err == nil {
		t.Fatal("unknown podcast enabled")
	}
	c.Podcasts["genwhy"] = PodcastSettings{Enabled: true, WindowMS: 30000}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Podcasts["genwhy"] = PodcastSettings{Enabled: true, Remove: []string{"content"}}
	if c.Validate() == nil {
		t.Fatal("removing episode content allowed")
	}
	c.Enabled = false
	if _, err := c.Policy("genwhy"); err == nil {
		t.Fatal("global opt-out ignored")
	}
}
