package maintenanceui

// testProfileConfig is a complete, valid ephemeral profile, since admission
// now decodes profile_config strictly instead of leaving that to the first
// step of the batch.
func testProfileConfig() map[string]any {
	return map[string]any{
		"container": "mkv",
		"video":     map[string]any{"codec": "hevc_videotoolbox", "quality": 80},
		"audio":     map[string]any{"mode": "copy"},
		"subtitles": map[string]any{"mode": "preserve", "convert_incompatible": true},
		"preserve":  map[string]any{"metadata": true, "chapters": true, "attachments": true},
		"resilience": map[string]any{
			"max_attempts":          1,
			"transient_retries":     0,
			"retry_backoff_seconds": []any{},
			"max_fallbacks":         1,
			"fallbacks":             []any{map[string]any{"when": "container_subtitle_incompatible", "action": "apply_container_conversion"}},
		},
	}
}
