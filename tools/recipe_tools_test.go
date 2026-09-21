package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jakenesler/navigatorr/config"
	"github.com/mark3labs/mcp-go/server"
)

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
	if requiredSet["expected_digest"] {
		t.Fatalf("expected_digest is conditional: it must remain optional for create and be enforced at runtime for update")
	}
}

func TestRecipeDeleteMCPRequiresExpectedDigest(t *testing.T) {
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
	required := map[string]bool{}
	for _, name := range tool.Tool.InputSchema.Required {
		required[name] = true
	}
	if !required["name"] || !required["expected_digest"] {
		t.Fatalf("recipe_delete must require name and expected_digest in MCP schema: %+v", tool.Tool.InputSchema.Required)
	}
}
