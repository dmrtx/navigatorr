package config

import (
	"testing"

	"github.com/jakenesler/navigatorr/transcode/recipe"
)

func controlPlaneTestProfile(quality int) recipe.Profile {
	return recipe.Profile{
		Container: "mkv",
		Video: recipe.VideoProfile{
			Codec:       "libx265",
			Quality:     quality,
			Preset:      "slow",
			Tune:        "animation",
			Profile:     "main",
			PixelFormat: "yuv420p",
		},
		Audio:     recipe.AudioProfile{Mode: "copy"},
		Subtitles: recipe.SubtitleProfile{Mode: "copy", ConvertIncompatible: true},
		Preserve:  recipe.PreserveProfile{Metadata: true, Chapters: true, Attachments: true},
		Resilience: recipe.ResilienceProfile{
			MaxAttempts: 1,
			MaxFallbacks: 1,
			Fallbacks: []recipe.FallbackRule{{When: "container_subtitle_incompatible", Action: "apply_container_conversion"}},
		},
		Optimization: &recipe.OptimizationPolicy{
			Enabled: true,
			Search:  &recipe.SearchPolicy{QualityValues: []int{20, 22, 24, 26}},
		},
	}
}

func TestManagedProfileOverridesStaticAndCarriesManagedIdentity(t *testing.T) {
	mgr, err := recipe.NewManager(recipe.BuiltinProvider{}, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	tc := TranscodeConfig{
		Profiles: map[string]TranscodeProfileConfig{
			"central-test": recipeToProfile(controlPlaneTestProfile(26)),
		},
		recipeManager: mgr,
	}
	rec, err := mgr.SaveManagedProfile("central-test", controlPlaneTestProfile(22), "validated", "act-benchmark-1", "")
	if err != nil {
		t.Fatal(err)
	}

	p, err := tc.ResolveProfile("central-test")
	if err != nil {
		t.Fatal(err)
	}
	if p.Video.Quality != 22 || p.Video.Tune != "animation" {
		t.Fatalf("managed profile did not win precedence: %+v", p.Video)
	}
	plan, err := tc.ResolvePlanForSource("central-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Quality != 22 || plan.Tune != "animation" {
		t.Fatalf("managed plan mismatch: %+v", plan)
	}
	if plan.RecipeDigest != rec.Digest || plan.RecipeVersion != "managed:central-test:1" || plan.PlanDigest == "" {
		t.Fatalf("managed identity not stamped: version=%q digest=%q plan=%q", plan.RecipeVersion, plan.RecipeDigest, plan.PlanDigest)
	}
}

func TestResolveEphemeralPlanDoesNotPersistAndHasOwnDigest(t *testing.T) {
	mgr, err := recipe.NewManager(recipe.BuiltinProvider{}, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	tc := TranscodeConfig{recipeManager: mgr}
	plan, normalized, digest, err := tc.ResolveEphemeralPlanForSource(controlPlaneTestProfile(24), nil)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Video.Tune != "animation" || plan.Tune != "animation" {
		t.Fatalf("ephemeral tune not resolved: profile=%+v plan=%+v", normalized.Video, plan)
	}
	if plan.RecipeVersion != "ephemeral" || plan.RecipeDigest != digest || digest == "" || plan.PlanDigest == "" {
		t.Fatalf("bad ephemeral identity: version=%q digest=%q plan=%q", plan.RecipeVersion, plan.RecipeDigest, plan.PlanDigest)
	}
	managed, err := mgr.ListManagedProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 0 {
		t.Fatalf("ephemeral resolution persisted profile unexpectedly: %+v", managed)
	}
}
