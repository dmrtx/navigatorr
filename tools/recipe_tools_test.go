package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/mark3labs/mcp-go/server"
)

func structuredRecipeProfileMap(t *testing.T) map[string]any {
	t.Helper()
	p := recipe.Profile{
		Container: "mkv",
		Video: recipe.VideoProfile{
			Codec:       "libx265",
			Quality:     24,
			Preset:      "slow",
			Tune:        "animation",
			Profile:     "main",
			PixelFormat: "yuv420p",
		},
		Audio:     recipe.AudioProfile{Mode: "copy"},
		Subtitles: recipe.SubtitleProfile{Mode: "preserve", ConvertIncompatible: true},
		Preserve:  recipe.PreserveProfile{Metadata: true, Chapters: true, Attachments: true},
		Resilience: recipe.ResilienceProfile{
			MaxAttempts:  1,
			MaxFallbacks: 1,
			Fallbacks:    []recipe.FallbackRule{{When: "container_subtitle_incompatible", Action: "apply_container_conversion"}},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecipeSaveMCPProfileIsStructuredObject(t *testing.T) {
	cfg := &config.Config{}
	cfg.Transcode.Recipes.CacheDir = t.TempDir()
	if err := cfg.Transcode.InitializeRecipes(context.Background()); err != nil {
		t.Fatal(err)
	}

	s := server.NewMCPServer("test", "0.0.0")
	registerRecipeTools(s, cfg)

	tool := s.GetTool("recipe_save")
	if tool == nil {
		t.Fatal("recipe_save was not registered")
	}
	if len(tool.Tool.RawInputSchema) == 0 {
		t.Fatalf("recipe_save should publish a typed raw input schema, got %+v", tool.Tool.InputSchema)
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Tool.RawInputSchema, &schema); err != nil {
		t.Fatalf("decoding recipe_save schema: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("recipe_save schema missing properties: %+v", schema)
	}
	profile, ok := props["profile"].(map[string]any)
	if !ok {
		t.Fatalf("recipe_save profile schema missing or not an object schema: %+v", props["profile"])
	}
	if profile["type"] != "object" {
		t.Fatalf("recipe_save profile must be an MCP object, got type=%v schema=%+v", profile["type"], profile)
	}
	profileProps, ok := profile["properties"].(map[string]any)
	if !ok {
		t.Fatalf("recipe_save profile object should expose nested fields: %+v", profile)
	}
	for _, field := range []string{"container", "video", "audio", "subtitles", "preserve", "resilience"} {
		if _, ok := profileProps[field]; !ok {
			t.Fatalf("recipe_save profile schema missing %q: %+v", field, profileProps)
		}
	}

	required, _ := schema["required"].([]any)
	requiredSet := map[string]bool{}
	for _, raw := range required {
		if name, ok := raw.(string); ok {
			requiredSet[name] = true
		}
	}
	if !requiredSet["name"] || !requiredSet["profile"] {
		t.Fatalf("recipe_save name/profile must be required: %+v", required)
	}
	if requiredSet["expected_generation"] || requiredSet["expected_digest"] {
		t.Fatalf("expected_generation/expected_digest are conditional: they must remain optional for create and be enforced at runtime for update")
	}
	generationSchema, _ := props["expected_generation"].(map[string]any)
	if generationSchema["type"] != "integer" {
		t.Fatalf("recipe_save expected_generation should be integer when supplied, got %+v", generationSchema)
	}
}

func TestRecipeDeleteMCPRequiresGenerationAndDigest(t *testing.T) {
	cfg := &config.Config{}
	cfg.Transcode.Recipes.CacheDir = t.TempDir()
	if err := cfg.Transcode.InitializeRecipes(context.Background()); err != nil {
		t.Fatal(err)
	}

	s := server.NewMCPServer("test", "0.0.0")
	registerRecipeTools(s, cfg)

	tool := s.GetTool("recipe_delete")
	if tool == nil {
		t.Fatal("recipe_delete was not registered")
	}
	if len(tool.Tool.RawInputSchema) == 0 {
		t.Fatalf("recipe_delete should publish a typed raw input schema, got %+v", tool.Tool.InputSchema)
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Tool.RawInputSchema, &schema); err != nil {
		t.Fatalf("decoding recipe_delete schema: %v", err)
	}
	requiredRaw, _ := schema["required"].([]any)
	required := map[string]bool{}
	for _, raw := range requiredRaw {
		if name, ok := raw.(string); ok {
			required[name] = true
		}
	}
	if !required["name"] || !required["expected_generation"] || !required["expected_digest"] {
		t.Fatalf("recipe_delete must require name, expected_generation and expected_digest: %+v", requiredRaw)
	}
	props, _ := schema["properties"].(map[string]any)
	generation, _ := props["expected_generation"].(map[string]any)
	if generation["type"] != "integer" {
		t.Fatalf("expected_generation should be exposed as integer, got %+v", generation)
	}
}

func TestRecipeSaveAcceptsStructuredObjectAndKeepsStrictDecoding(t *testing.T) {
	cfg := &config.Config{}
	cfg.Transcode.Recipes.CacheDir = t.TempDir()
	if err := cfg.Transcode.InitializeRecipes(context.Background()); err != nil {
		t.Fatal(err)
	}

	s := server.NewMCPServer("test", "0.0.0")
	registerRecipeTools(s, cfg)

	profile := structuredRecipeProfileMap(t)
	res := callTool(t, s, "recipe_save", map[string]any{
		"name":    "structured-x265",
		"profile": profile,
	})
	txt := resultText(t, res)
	if !strings.Contains(txt, `"saved": true`) {
		t.Fatalf("structured recipe_save failed: %s", txt)
	}

	rec, ok, err := cfg.Transcode.RecipeManager().GetManagedProfile("structured-x265")
	if err != nil || !ok {
		t.Fatalf("saved profile missing: ok=%v err=%v", ok, err)
	}
	if rec.Profile.Video.Tune != "animation" {
		t.Fatalf("structured nested profile was not preserved: %+v", rec.Profile.Video)
	}

	bad := structuredRecipeProfileMap(t)
	video, ok := bad["video"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected video shape: %+v", bad["video"])
	}
	video["unknown_encoder_knob"] = true
	badRes := callTool(t, s, "recipe_save", map[string]any{
		"name":    "bad-structured-x265",
		"profile": bad,
	})
	badTxt := resultText(t, badRes)
	if !strings.Contains(badTxt, "unknown field") {
		t.Fatalf("structured schema path must retain strict unknown-field rejection: %s", badTxt)
	}

	update := structuredRecipeProfileMap(t)
	update["video"].(map[string]any)["quality"] = float64(22)
	updateRes := callTool(t, s, "recipe_save", map[string]any{
		"name":            "structured-x265",
		"profile":         update,
		"expected_digest": rec.Digest,
	})
	updateTxt := resultText(t, updateRes)
	if !strings.Contains(updateTxt, "expected_generation is required") {
		t.Fatalf("update without expected_generation must fail closed: %s", updateTxt)
	}

	updateRes = callTool(t, s, "recipe_save", map[string]any{
		"name":                "structured-x265",
		"profile":             update,
		"expected_generation": rec.Generation,
	})
	updateTxt = resultText(t, updateRes)
	if !strings.Contains(updateTxt, "expected_digest is required") {
		t.Fatalf("update without expected_digest must fail closed: %s", updateTxt)
	}
}
