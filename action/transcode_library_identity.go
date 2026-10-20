package action

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// libraryIdentity says which library item a transcode source belongs to, so
// searches of sibling episodes can find each other. Service is sonarr, radarr
// or empty; Season and Episode are -1 when unknown. FolderKey is a digest of
// the source's directory and never the path itself, so it can be stored and
// shown without revealing where media lives.
type libraryIdentity struct {
	Service   string `json:"service,omitempty"`
	ID        int64  `json:"id,omitempty"`
	Season    int    `json:"season"`
	Episode   int    `json:"episode"`
	FolderKey string `json:"folder_key,omitempty"`
}

func (l libraryIdentity) hasLibrary() bool { return l.Service != "" && l.ID > 0 }

var episodeInfoPattern = regexp.MustCompile(`^S(\d{1,3})E(\d{1,4})`)

// transcodeLibraryIdentity resolves a transcode_media source's identity from
// its library_context input, else its parent transcode_batch (service and
// series id, plus the batch item's season and episode). The folder key is
// always set when the source path is known.
func (e *Engine) transcodeLibraryIdentity(ec *ExecutionContext) libraryIdentity {
	id := libraryIdentity{Season: -1, Episode: -1, FolderKey: folderKey(getString(ec.Inputs, "path"))}
	if ctx, ok := ec.Inputs["library_context"].(map[string]any); ok {
		service := strings.ToLower(strings.TrimSpace(getString(ctx, "service")))
		libID := getInt64(ctx, "id")
		if libID == 0 {
			libID = getInt64(ctx, "series_id")
		}
		if libID == 0 {
			libID = getInt64(ctx, "movie_id")
		}
		if (service == "sonarr" || service == "radarr") && libID > 0 {
			id.Service, id.ID = service, libID
			if _, ok := ctx["season"]; ok {
				id.Season = getInt(ctx, "season")
			}
			if _, ok := ctx["episode"]; ok {
				id.Episode = getInt(ctx, "episode")
			}
		}
	}
	parentID := strings.TrimSpace(getString(ec.Inputs, "parent_action_id"))
	if parentID == "" || e.deps.Store == nil {
		return id
	}
	parent, err := e.deps.Store.GetActionInstance(parentID)
	if err != nil || parent == nil || parent.ActionName != "transcode_batch" {
		return id
	}
	if !id.hasLibrary() {
		var inputs map[string]any
		if json.Unmarshal([]byte(parent.InputsJSON), &inputs) == nil {
			if service := strings.ToLower(getString(inputs, "service")); service == "sonarr" && getInt64(inputs, "series_id") > 0 {
				id.Service, id.ID = service, getInt64(inputs, "series_id")
			}
		}
	}
	if id.Season < 0 {
		if itemKey := getString(ec.Inputs, "batch_item_key"); itemKey != "" {
			if item, err := e.deps.Store.GetTranscodeBatchItem(parent.ID, itemKey); err == nil && item != nil {
				id.Season, id.Episode = parseEpisodeInfo(item.EpisodeInfo)
			}
		}
	}
	return id
}

// parseEpisodeInfo reads a batch item's "S01E02" label (multi-episode files
// keep their first episode). It returns -1, -1 when there is none.
func parseEpisodeInfo(info string) (int, int) {
	m := episodeInfoPattern.FindStringSubmatch(strings.TrimSpace(info))
	if m == nil {
		return -1, -1
	}
	season, _ := strconv.Atoi(m[1])
	episode, _ := strconv.Atoi(m[2])
	return season, episode
}

func folderKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Dir(filepath.Clean(path))))
	return hex.EncodeToString(sum[:16])
}
