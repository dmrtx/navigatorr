package tools

import (
	"context"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type recipeSaveInput struct {
	Name           string         `json:"name" jsonschema:"description=Managed profile name"`
	Profile        recipe.Profile `json:"profile" jsonschema:"description=Complete typed transcode recipe profile"`
	Description    string         `json:"description,omitempty" jsonschema:"description=Optional human readable purpose or validation note"`
	SourceActionID     string         `json:"source_action_id,omitempty" jsonschema:"description=Optional unverified action reference metadata; recipe_save does not validate provenance"`
	ExpectedGeneration int64          `json:"expected_generation,omitempty" jsonschema:"description=Required when replacing an existing managed profile; omit only when creating a new name"`
	ExpectedDigest     string         `json:"expected_digest,omitempty" jsonschema:"description=Required when replacing an existing managed profile; omit only when creating a new name"`
}

type recipeDeleteInput struct {
	Name               string `json:"name" jsonschema:"description=Managed profile name"`
	ExpectedGeneration int64  `json:"expected_generation" jsonschema:"description=Current managed profile generation; required for optimistic concurrency"`
	ExpectedDigest     string `json:"expected_digest" jsonschema:"description=Current managed profile digest; required for optimistic concurrency"`
}

func registerRecipeTools(s *server.MCPServer, cfg *config.Config) {
	s.AddTool(mcp.NewTool("recipe_status", mcp.WithDescription("Show the active transcode recipe source, version, digest, last-known-good version, last check time, and sanitized update error.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		managed, err := mgr.ListManagedProfiles()
		if err != nil {
			return toolErr("reading managed recipe registry: %v", err), nil
		}
		return toolJSON(map[string]any{"bundle": mgr.Status(), "managed_profile_count": len(managed)}), nil
	})

	// recipe_list/get/save/delete/history are the Navigatorr-owned control plane
	// for day-to-day recipes. Workers never install these objects: every action
	// resolves a validated immutable Plan and sends that plan to the selected worker.
	s.AddTool(mcp.NewTool("recipe_list",
		mcp.WithDescription("List available transcode recipe profiles. Managed profiles are centrally stored by Navigatorr and override static config/bundle profiles for new jobs."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		managed, err := mgr.ListManagedProfiles()
		if err != nil {
			return toolErr("reading managed recipe registry: %v", err), nil
		}
		bundleNames := []string{}
		if snap := mgr.Snapshot(); snap != nil {
			for name := range snap.Bundle.Profiles {
				bundleNames = append(bundleNames, name)
			}
		}
		sort.Strings(bundleNames)
		staticNames := make([]string, 0, len(cfg.Transcode.Profiles))
		for name := range cfg.Transcode.Profiles {
			staticNames = append(staticNames, name)
		}
		sort.Strings(staticNames)
		return toolJSON(map[string]any{
			"precedence":             []string{"managed", "static_config", "active_bundle"},
			"managed_profiles":       managed,
			"static_config_profiles": staticNames,
			"active_bundle_profiles": bundleNames,
		}), nil
	})

	s.AddTool(mcp.NewTool("recipe_get",
		mcp.WithDescription("Get the effective typed profile by name and identify which recipe layer supplies it."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Recipe profile name")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strings.TrimSpace(argString(req.GetArguments(), "name", ""))
		if name == "" {
			return toolErr("name is required"), nil
		}
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		if rec, ok, err := mgr.GetManagedProfile(name); err != nil {
			return toolErr("reading managed recipe registry: %v", err), nil
		} else if ok {
			return toolJSON(map[string]any{"name": name, "source": "managed", "record": rec, "profile": rec.Profile}), nil
		}
		profile, err := cfg.Transcode.ResolveProfile(name)
		if err != nil {
			return toolErr("%v", err), nil
		}
		source := "active_bundle"
		if _, ok := cfg.Transcode.Profiles[name]; ok {
			source = "static_config"
		}
		return toolJSON(map[string]any{"name": name, "source": source, "profile": profile}), nil
	})

	s.AddTool(mcp.NewTool("recipe_save",
		mcp.WithDescription("Create or replace a centrally managed typed transcode profile. profile is a structured object and is strictly decoded: unknown/unsupported fields fail instead of being ignored. Creating a new name omits expected_generation and expected_digest; replacing an existing profile requires both current values. generation is the primary CAS token and prevents metadata-only/ABA races; digest additionally checks profile content. source_action_id is unverified reference metadata, not validated provenance. New jobs see the saved profile immediately; running jobs keep their immutable plans."),
		mcp.WithInputSchema[recipeSaveInput](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strings.TrimSpace(argString(args, "name", ""))
		if name == "" {
			return toolErr("name is required"), nil
		}
		raw, err := argJSON(args, "profile")
		if err != nil {
			return toolErr("%v", err), nil
		}
		profile, digest, err := recipe.DecodeProfileStrict(name, []byte(raw))
		if err != nil {
			return toolErr("invalid profile: %v", err), nil
		}
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		rec, err := mgr.SaveManagedProfile(
			name,
			profile,
			argString(args, "description", ""),
			argString(args, "source_action_id", ""),
			argInt64(args, "expected_generation", 0),
			argString(args, "expected_digest", ""),
		)
		if err != nil {
			return toolErr("saving managed recipe: %v", err), nil
		}
		if rec.Digest != digest {
			return toolErr("managed recipe digest changed unexpectedly during normalization"), nil
		}
		return toolJSON(map[string]any{"saved": true, "active_for_new_jobs": true, "record": rec}), nil
	})

	s.AddTool(mcp.NewTool("recipe_delete",
		mcp.WithDescription("Delete a centrally managed profile override while preserving its audit history. expected_generation and expected_digest are both required; stale metadata-only or ABA-era clients fail closed. Running jobs are not changed."),
		mcp.WithInputSchema[recipeDeleteInput](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		name := strings.TrimSpace(argString(args, "name", ""))
		if name == "" {
			return toolErr("name is required"), nil
		}
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		entry, err := mgr.DeleteManagedProfile(name, argInt64(args, "expected_generation", 0), argString(args, "expected_digest", ""))
		if err != nil {
			return toolErr("deleting managed recipe: %v", err), nil
		}
		return toolJSON(map[string]any{"deleted": true, "history_entry": entry}), nil
	})

	s.AddTool(mcp.NewTool("recipe_history",
		mcp.WithDescription("Show the bounded save/delete audit history for a centrally managed profile."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Managed profile name")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := strings.TrimSpace(argString(req.GetArguments(), "name", ""))
		if name == "" {
			return toolErr("name is required"), nil
		}
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		history, err := mgr.ManagedProfileHistory(name)
		if err != nil {
			return toolErr("reading managed recipe history: %v", err), nil
		}
		return toolJSON(map[string]any{"name": name, "history": history}), nil
	})

	s.AddTool(mcp.NewTool("recipe_reload", mcp.WithDescription("Re-read the configured bundle recipe source, validate it strictly, cache it, and atomically activate it only if valid. Centrally managed profile overrides are independent. Running jobs keep their existing plan.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		st, err := mgr.Reload(ctx)
		if err != nil {
			return toolJSON(map[string]any{"status": st, "activated": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "activated": true}), nil
	})
	s.AddTool(mcp.NewTool("recipe_update", mcp.WithDescription("Fetch the configured versioned bundle recipe source now. Invalid schema, semantics, compatibility, or integrity leave the current last-known-good bundle active. Managed overrides remain local to Navigatorr.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		st, err := mgr.Update(ctx)
		if err != nil {
			return toolJSON(map[string]any{"status": st, "activated": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "activated": true}), nil
	})
	s.AddTool(mcp.NewTool("recipe_rollback", mcp.WithDescription("Atomically restore the previous validated cached bundle recipe. Managed profile overrides and running immutable plans are not altered.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		st, err := mgr.Rollback()
		if err != nil {
			return toolJSON(map[string]any{"status": st, "rolled_back": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "rolled_back": true}), nil
	})
}
