package action

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Explicit paths freeze the filesystem selection at admission. Directory
// discovery belongs to the caller; a later restart must not silently pick up
// new files. The shared inspection/calibration/scheduler owns all encoding.
// batchPathsInput checks the shape of a filesystem batch's paths input, which
// needs no file access: it is not mixed with Sonarr selectors, not combined
// with promotion, and a bounded array of non-empty strings. present reports
// whether the batch is a filesystem batch at all.
func batchPathsInput(inputs map[string]any) (paths []string, present bool, err error) {
	raw, present := inputs["paths"]
	if !present {
		return nil, false, nil
	}
	for _, key := range []string{"service", "series_id", "season", "episode_file_ids"} {
		if _, mixed := inputs[key]; mixed {
			return nil, true, inputErr("paths", "paths cannot be combined with %s", key)
		}
	}
	if getBool(inputs, "promote_candidates") {
		return nil, true, inputErr("paths", "filesystem batches are candidate-only; promote_candidates requires a Sonarr series")
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &paths) != nil || len(paths) == 0 || len(paths) > 1000 {
		return nil, true, inputErr("paths", "paths must be a non-empty array of up to 1000 video file paths")
	}
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			return nil, true, inputErr("paths", "paths must not contain empty paths")
		}
	}
	return paths, true, nil
}

func (e *Engine) resolveBatchPaths(inputs map[string]any) ([]string, bool, error) {
	paths, present, err := batchPathsInput(inputs)
	if !present || err != nil {
		return nil, present, err
	}
	seen := map[string]bool{}
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		clean, err := e.deps.Fs.ResolveRead(path)
		if err != nil {
			return nil, true, fmt.Errorf("batch source path is outside allowed read roots: %w", err)
		}
		fi, err := os.Stat(clean)
		if err != nil {
			return nil, true, fmt.Errorf("batch source file is inaccessible: %w", err)
		}
		if !fi.Mode().IsRegular() {
			return nil, true, fmt.Errorf("paths must contain regular video files, not directories or devices")
		}
		switch strings.ToLower(filepath.Ext(clean)) {
		case ".mkv", ".mp4", ".m4v", ".mov", ".avi", ".webm", ".ts", ".m2ts", ".mpg", ".mpeg", ".vob", ".wmv", ".flv", ".ogv":
		default:
			return nil, true, fmt.Errorf("unsupported video file extension: %s; audio-only batches are not supported", filepath.Ext(clean))
		}
		if !seen[clean] {
			seen[clean] = true
			resolved = append(resolved, clean)
		}
	}
	sort.Strings(resolved)
	return resolved, true, nil
}
