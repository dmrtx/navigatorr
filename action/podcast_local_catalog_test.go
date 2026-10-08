package action

import (
	"context"
	"fmt"
	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/podcast"
	"path/filepath"
	"testing"
	"time"
)

type offlineAdExecutor struct {
	*adTestExecutor
	offline bool
}

func (x *offlineAdExecutor) AdCatalog(ctx context.Context, scope string) (podcast.AdCatalog, error) {
	if x.offline {
		return podcast.AdCatalog{}, fmt.Errorf("offline")
	}
	return x.adTestExecutor.AdCatalog(ctx, scope)
}
func (x *offlineAdExecutor) RevokeAd(ctx context.Context, scope, id string) error {
	if x.offline {
		return fmt.Errorf("offline")
	}
	return x.adTestExecutor.RevokeAd(ctx, scope, id)
}

func TestLocalCatalogSurvivesOfflineRestartRevocationAndPolicyChange(t *testing.T) {
	dir := t.TempDir()
	scope := "show&friends"
	ref := podcast.AdReference{ID: podcast.Digest("ad"), Digest: podcast.Digest("ad"), TextDigest: podcast.Digest("text"), SourceHash: podcast.Digest("source"), CutsDigest: podcast.Digest("cuts"), DurationMS: 12000, Label: "paid_ad"}
	tc := &offlineAdExecutor{adTestExecutor: &adTestExecutor{catalog: podcast.AdCatalog{Algorithm: podcast.AdAlgorithm, Scope: scope, References: []podcast.AdReference{ref}}}}
	e, store := setupTranscodeEngine(t, tc, "", []string{dir}, []string{dir}, false)
	defer store.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{Enabled: true, ArtifactDir: filepath.Join(dir, "private"), LocalCatalogDir: filepath.Join(dir, "shared"), Podcasts: map[string]config.PodcastSettings{scope: {Enabled: true, KnownAdsFirstPass: true}}}
	if _, err := e.PodcastLocalLibrary(scope); err == nil {
		t.Fatal("missing mirror accepted")
	}
	if _, err := e.PodcastAdLibrary(context.Background(), scope, ""); err != nil {
		t.Fatal(err)
	}
	tc.offline = true
	e = NewEngine(e.Deps())
	if _, err := e.PodcastLocalLibrary(scope); err != nil {
		t.Fatalf("offline mirror unavailable: %v", err)
	}
	if _, err := e.PodcastAdLibrary(context.Background(), scope, ref.ID); err == nil {
		t.Fatal("offline revoke acknowledged")
	}
	e = NewEngine(e.Deps())
	v, err := e.PodcastLocalLibrary(scope)
	if err != nil || !v["catalog"].(podcast.AdCatalog).References[0].Revoked {
		t.Fatalf("tombstone lost %v %v", v, err)
	}
	tc.offline = false
	if c, err := e.PodcastAdLibrary(context.Background(), scope, ""); err != nil || !c.References[0].Revoked {
		t.Fatalf("stale worker resurrected ad: %v %v", c, err)
	}
	e.deps.Config.Podcasts.Podcasts[scope] = config.PodcastSettings{Enabled: true, KnownAdsFirstPass: true, Remove: []string{"cross_promo"}}
	v, err = e.PodcastLocalLibrary(scope)
	if err != nil || v["policy"].(podcast.Policy).Remove[0] != "cross_promo" {
		t.Fatal("stale policy")
	}
	e.deps.Config.Podcasts.Enabled = false
	if _, err := e.PodcastLocalLibrary(scope); err == nil {
		t.Fatal("disabled cleaning admitted")
	}
}

func TestLocalPublicationLockSerializesRevocationAndReleasesOnCancel(t *testing.T) {
	dir := t.TempDir()
	e, store := setupTranscodeEngine(t, &mockTranscodeExecutor{}, "", []string{dir}, []string{dir}, false)
	defer store.Close()
	e.deps.Config.Podcasts = config.PodcastConfig{ArtifactDir: dir}
	release, err := e.lockPodcastCatalog(context.Background(), "show")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := e.lockPodcastCatalog(ctx, "show"); err == nil {
		t.Fatal("second holder bypassed publication lock")
	}
	release()
	release, err = e.lockPodcastCatalog(context.Background(), "show")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
