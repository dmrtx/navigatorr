package tools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jakenesler/navigatorr/config"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
)

// Deps carries the maintenance-agent dependencies into the tool handlers.
type Deps struct {
	Store   *store.Store
	Config  *config.Config
	Fs      *fsop.Resolver
	Ffprobe string
}

// argOk reports whether a raw argument was supplied.
func argOk(args map[string]any, key string) bool {
	v, ok := args[key]
	return ok && v != nil
}

// argString coerces an argument to string.
func argString(args map[string]any, key, def string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// argInt64 coerces an argument to int64.
func argInt64(args map[string]any, key string, def int64) int64 {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return int64(f)
		}
	}
	return def
}

// argFloat coerces an argument to float64.
func argFloat(args map[string]any, key string, def float64) float64 {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return def
}

// argBool coerces an argument to bool.
func argBool(args map[string]any, key string, def bool) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(t)); err == nil {
			return b
		}
	case float64:
		return t != 0
	}
	return def
}

// argStrings coerces an argument (JSON array, comma string, or single value)
// to a string slice.
func argStrings(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			} else if e != nil {
				out = append(out, fmt.Sprintf("%v", e))
			}
		}
		return out
	case []string:
		return t
	case string:
		trimmed := strings.TrimSpace(t)
		if strings.HasPrefix(trimmed, "[") {
			var arr []string
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return arr
			}
		}
		if trimmed == "" {
			return nil
		}
		parts := strings.Split(trimmed, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// argJSON re-marshals any argument to its JSON encoding.
func argJSON(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", fmt.Errorf("%s is required", key)
	}
	if s, ok := v.(string); ok {
		if !json.Valid([]byte(s)) {
			// A bare string is valid input too: encode it as JSON.
			b, _ := json.Marshal(s)
			return string(b), nil
		}
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding %s: %w", key, err)
	}
	return string(b), nil
}

// toolErr formats a handler failure as a tool error (not a transport error),
// matching the existing tools' convention.
func toolErr(format string, args ...any) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(format, args...))
}

// toolJSON renders a compact JSON payload.
func toolJSON(v any) *mcp.CallToolResult {
	data, _ := json.MarshalIndent(v, "", "  ")
	return mcp.NewToolResultText(string(data))
}

// parseID coerces a maintenance item id argument.
func parseID(args map[string]any) (int64, *mcp.CallToolResult) {
	id := argInt64(args, "id", 0)
	if id <= 0 {
		// Also accept "m12" style? No: keep numeric ids, strict schemas.
		return 0, toolErr("id is required (numeric maintenance item id)")
	}
	return id, nil
}

// loadRankPrefs merges stored preferences over the config defaults for a
// media type ("anime"/"series" or "movies"/"movie").
func loadRankPrefs(st *store.Store, cfg *config.Config, mediaType string) maint.RankPrefs {
	scope := "movies"
	base := maint.DefaultMoviePrefs()
	if mediaType == "anime" || mediaType == "series" {
		scope = "anime"
		base = maint.DefaultAnimePrefs()
	}
	base.PreferredGroups = cfg.Maintenance.PreferredGroups
	if cfg.Maintenance.PreferredResolution != "" {
		base.PreferredRes = cfg.Maintenance.PreferredResolution
	}
	get := func(key string, dst any) {
		for _, sc := range []string{scope, "global"} {
			if p, err := st.GetPreference(sc, key); err == nil {
				if err := p.Value(dst); err == nil {
					return
				}
			}
		}
	}
	get("preferred_release_groups", &base.PreferredGroups)
	get("preferred_resolution", &base.PreferredRes)
	get("prefer_hevc", &base.PreferHEVC)
	get("prefer_10bit", &base.Prefer10Bit)
	get("prefer_dual_audio", &base.PreferDualAudio)
	get("prefer_multi_subs", &base.PreferMultiSubs)
	get("require_subtitles_when_non_english_spanish", &base.RequireSubs)
	get("min_seeders", &base.MinSeeders)
	get("compact_bias", &base.CompactBias)
	var maxMB float64
	get("max_release_size_mb", &maxMB)
	if maxMB > 0 {
		base.MaxSizeBytes = int64(maxMB) << 20
	}
	return base
}

// SeedDefaults writes the initial preference defaults once. Stored values
// always win: seeding never overwrites an existing key.
func SeedDefaults(st *store.Store, cfg *config.Config) {
	set := func(scope, key, value string) {
		if _, err := st.GetPreference(scope, key); err == nil {
			return
		}
		_, _ = st.SetPreference(scope, key, value, "default", 0)
	}
	groups, _ := json.Marshal(cfg.Maintenance.PreferredGroups)
	res, _ := json.Marshal(cfg.Maintenance.PreferredResolution)
	set("anime", "preferred_release_groups", string(groups))
	set("anime", "preferred_resolution", string(res))
	set("anime", "prefer_hevc", "true")
	set("anime", "prefer_10bit", "true")
	set("anime", "prefer_dual_audio", "true")
	set("anime", "prefer_multi_subs", "true")
	set("anime", "require_subtitles_when_non_english_spanish", "true")
	set("anime", "subtitle_languages", `["eng","spa"]`)
	set("anime", "keep_original_until_verified", "true")
	set("anime", "avoid_unseeded", "true")
	set("movies", "acceptable_audio", `["eng","spa"]`)
	set("movies", "acceptable_subtitles", `["eng","spa"]`)
	set("movies", "require_subtitles_when_non_english_spanish", "true")
	set("global", "blocked_extensions", `["exe","scr","bat","cmd","com","msi","ps1","jar"]`)
	set("global", "allow_destructive", "false")
}
