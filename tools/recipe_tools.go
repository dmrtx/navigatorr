package tools

import (
	"context"
	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/transcode"
	"github.com/jakenesler/navigatorr/transcode/recipe"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type recipeSaveInput struct {
	Name               string         `json:"name" jsonschema:"description=Managed profile name"`
	Profile            recipe.Profile `json:"profile" jsonschema:"description=Complete typed transcode recipe profile"`
	Description        string         `json:"description,omitempty" jsonschema:"description=Optional human readable purpose or validation note"`
	SourceActionID     string         `json:"source_action_id,omitempty" jsonschema:"description=Optional unverified action reference metadata; recipe_save does not validate provenance"`
	ExpectedGeneration int64          `json:"expected_generation,omitempty" jsonschema:"description=Required when replacing an existing managed profile; omit only when creating a new name"`
	ExpectedDigest     string         `json:"expected_digest,omitempty" jsonschema:"description=Required when replacing an existing managed profile; omit only when creating a new name"`
}

type recipeDeleteInput struct {
	Name               string `json:"name" jsonschema:"description=Managed profile name"`
	ExpectedGeneration int64  `json:"expected_generation" jsonschema:"description=Current managed profile generation; required for optimistic concurrency"`
	ExpectedDigest     string `json:"expected_digest" jsonschema:"description=Current managed profile digest; required for optimistic concurrency"`
}

const (
	defaultRecipeListLimit    = 50
	maxRecipeListLimit        = 100
	defaultRecipeHistoryLimit = 10
	maxRecipeHistoryLimit     = 50
)

func compactAudioPolicy() map[string]any {
	targets := map[int]int{}
	for _, channels := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		_, targets[channels] = transcode.AudioTarget("compact", "dts", channels)
	}
	return map[string]any{"convert_to": "aac", "convert_codecs": []string{"dts", "truehd", "mlp", "flac", "alac", "pcm_*"}, "target_kbps_by_channels": targets, "otherwise": "copy (including AAC/AC3/EAC3/Opus, unknown channel count, and more than 8 channels)", "preserve": "track count, languages, channels and dispositions", "lossy": true, "note": "Nominal targets, not measured audio quality. Benchmark estimates audio; fallback_audio_bitrate_bps is an estimation fallback, not the encoding target."}
}

func recipeShadowing(cfg *config.Config, rec recipe.ManagedProfileRecord) []map[string]any {
	var out []map[string]any
	base := cfg.Transcode.RecipeBaseProfiles(rec.Name)
	for _, source := range []string{"static_config", "active_bundle"} {
		p, ok := base[source]
		if !ok {
			continue
		}
		_, digest, err := recipe.NormalizeAndDigestProfile(rec.Name, p)
		entry := map[string]any{"source": source, "warning": "Managed profile fully overrides this layer; base updates are not inherited."}
		if err == nil {
			entry["digest"], entry["differs"] = digest, digest != rec.Digest
		}
		if source == "active_bundle" {
			entry["bundle_version"] = cfg.Transcode.RecipeManager().Status().ActiveVersion
		}
		out = append(out, entry)
	}
	return out
}

func managedProfileSummary(rec recipe.ManagedProfileRecord) map[string]any {
	return map[string]any{
		"name":             rec.Name,
		"generation":       rec.Generation,
		"digest":           rec.Digest,
		"description":      rec.Description,
		"source_action_id": rec.SourceActionID,
		"updated_at":       rec.UpdatedAt,
	}
}

func managedHistorySummary(entry recipe.ManagedProfileHistoryEntry) map[string]any {
	return map[string]any{
		"event":            entry.Event,
		"generation":       entry.Generation,
		"digest":           entry.Digest,
		"description":      entry.Description,
		"source_action_id": entry.SourceActionID,
		"at":               entry.At,
	}
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
		mcp.WithDescription("List available transcode recipe profiles without embedding full managed profile bodies. Managed summaries are paginated; use recipe_get for one complete effective profile."),
		mcp.WithNumber("limit", mcp.Description("Optional managed-profile page size (default 50, max 100)"), mcp.Min(1), mcp.Max(maxRecipeListLimit)),
		mcp.WithNumber("offset", mcp.Description("Optional zero-based managed-profile offset"), mcp.Min(0)),
		mcp.WithBoolean("include_selection_details", mcp.Description("Include compact effective encoder, quality, audio, and sample-testing settings for profile selection")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		managed, err := mgr.ListManagedProfiles()
		if err != nil {
			return toolErr("reading managed recipe registry: %v", err), nil
		}
		args := req.GetArguments()
		limit := int(argInt64(args, "limit", defaultRecipeListLimit))
		if limit <= 0 {
			limit = defaultRecipeListLimit
		}
		if limit > maxRecipeListLimit {
			limit = maxRecipeListLimit
		}
		offset := int(argInt64(args, "offset", 0))
		if offset < 0 {
			offset = 0
		}
		if offset > len(managed) {
			offset = len(managed)
		}
		end := offset + limit
		if end > len(managed) {
			end = len(managed)
		}
		managedSummaries := make([]map[string]any, 0, end-offset)
		for _, rec := range managed[offset:end] {
			summary := managedProfileSummary(rec)
			if shadowed := recipeShadowing(cfg, rec); len(shadowed) > 0 {
				summary["shadowed_profiles"] = shadowed
			}
			managedSummaries = append(managedSummaries, summary)
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
		payload := map[string]any{
			"precedence":             []string{"managed", "static_config", "active_bundle"},
			"managed_profiles":       managedSummaries,
			"managed_total":          len(managed),
			"managed_returned":       len(managedSummaries),
			"managed_offset":         offset,
			"managed_limit":          limit,
			"static_config_profiles": staticNames,
			"active_bundle_profiles": bundleNames,
		}
		if args["include_selection_details"] == true {
			// Resolve the effective layer, including managed overrides outside
			// this page. Selection must never describe the shadowed bundle.
			managedByName := map[string]recipe.ManagedProfileRecord{}
			for _, rec := range managed {
				managedByName[rec.Name] = rec
			}
			names := map[string]bool{}
			for _, name := range append(bundleNames, staticNames...) {
				names[name] = true
			}
			for _, rec := range managed[offset:end] {
				names[rec.Name] = true
			}
			details := map[string]any{}
			for name := range names {
				// Reuse the registry read above instead of reading and decoding
				// the entire registry again for every dropdown option.
				base := cfg.Transcode.RecipeBaseProfiles(name)
				profile := base["active_bundle"]
				source, description := "active_bundle", ""
				if p, ok := base["static_config"]; ok {
					profile, source = p, "static_config"
				}
				if rec, ok := managedByName[name]; ok {
					profile, source, description = rec.Profile, "managed", rec.Description
				}
				if err := recipe.ValidateProfile(name, profile); err != nil {
					continue
				}

				v := profile.Video
				details[name] = map[string]any{
					"source": source, "description": description,
					"profile": map[string]any{
						"container": profile.Container, "audio": profile.Audio,
						"video":        map[string]any{"codec": v.Codec, "quality": v.Quality, "average_bitrate_kbps": v.AverageBitrateKbps, "preset": v.Preset, "profile": v.Profile, "pixel_format": v.PixelFormat, "spatial_aq": v.SpatialAQ},
						"optimization": map[string]any{"enabled": profile.Optimization != nil && profile.Optimization.Enabled},
					},
				}
			}
			payload["selection_details"] = details
		}
		return toolBoundedJSON(payload, MaxActionResponseBytes, nil), nil
	})

	s.AddTool(mcp.NewTool("recipe_get",
		mcp.WithDescription("Get the effective typed profile, compact audio policy, and lower recipe layers hidden by a managed override. Overrides are complete profiles and do not inherit bundle updates."),
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
			return toolJSON(map[string]any{"name": name, "source": "managed", "record": rec, "profile": rec.Profile, "shadowed_profiles": recipeShadowing(cfg, rec), "compact_audio_policy": compactAudioPolicy()}), nil
		}
		profile, err := cfg.Transcode.ResolveProfile(name)
		if err != nil {
			return toolErr("%v", err), nil
		}
		source := "active_bundle"
		if _, ok := cfg.Transcode.Profiles[name]; ok {
			source = "static_config"
		}
		return toolJSON(map[string]any{"name": name, "source": source, "profile": profile, "compact_audio_policy": compactAudioPolicy()}), nil
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
		mcp.WithDescription("Show recent save/delete audit metadata for a centrally managed profile without repeating full profile bodies. Returns the newest entries in chronological order; use recipe_get for the current full profile."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Managed profile name")),
		mcp.WithNumber("limit", mcp.Description("Optional number of recent history entries (default 10, max 50)"), mcp.Min(1), mcp.Max(maxRecipeHistoryLimit)),
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
		history, err := mgr.ManagedProfileHistory(name)
		if err != nil {
			return toolErr("reading managed recipe history: %v", err), nil
		}
		limit := int(argInt64(args, "limit", defaultRecipeHistoryLimit))
		if limit <= 0 {
			limit = defaultRecipeHistoryLimit
		}
		if limit > maxRecipeHistoryLimit {
			limit = maxRecipeHistoryLimit
		}
		start := 0
		if len(history) > limit {
			start = len(history) - limit
		}
		summaries := make([]map[string]any, 0, len(history)-start)
		for _, entry := range history[start:] {
			summaries = append(summaries, managedHistorySummary(entry))
		}
		payload := map[string]any{
			"name":             name,
			"history":          summaries,
			"total_entries":    len(history),
			"returned_entries": len(summaries),
			"limit":            limit,
		}
		return toolBoundedJSON(payload, MaxActionResponseBytes, nil), nil
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
	s.AddTool(mcp.NewTool("recipe_rollback", mcp.WithDescription("Atomically restore the previous validated cached bundle recipe. Supply both expected digests from recipe_status to reject stale reviewed restoration. Managed profile overrides and running immutable plans are not altered."),
		mcp.WithString("expected_active_digest", mcp.Description("Reviewed active digest from recipe_status; requires expected_previous_digest")),
		mcp.WithString("expected_previous_digest", mcp.Description("Reviewed restoration destination digest from recipe_status; requires expected_active_digest"))), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		args := req.GetArguments()
		st, err := mgr.RollbackReviewed(argString(args, "expected_active_digest", ""), argString(args, "expected_previous_digest", ""))
		if err != nil {
			return toolJSON(map[string]any{"status": st, "rolled_back": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "rolled_back": true}), nil
	})
}
