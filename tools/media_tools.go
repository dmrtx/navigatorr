package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerMediaTools(s *server.MCPServer, d *Deps, registry *arrservice.Registry, qbClient *qbit.Client) {
	// inspect_media — real-file inspection with *arr fallback.
	s.AddTool(
		mcp.NewTool("inspect_media",
			mcp.WithDescription("Inspect a real media file: container, video codec, resolution, bit depth, audio/subtitle languages, duration, size, embedded and sidecar subtitles, dangerous names. Give path for a filesystem inspection (ffprobe when available), or service+file_id to resolve the path via Sonarr/Radarr and inspect it, falling back to the service mediaInfo when the file is not reachable."),
			mcp.WithString("path", mcp.Description("Filesystem path inside allowed_read_roots")),
			mcp.WithString("service", mcp.Description("sonarr or radarr (used with file_id)")),
			mcp.WithString("file_id", mcp.Description("Episode file id (sonarr /episodefile/{id}) or movie file id (radarr /moviefile/{id})")),
			mcp.WithString("media_type", mcp.Description("series, anime or movie (for the inspection record)")),
			mcp.WithString("media_id", mcp.Description("Series/movie id (for the inspection record)")),
			mcp.WithString("record", mcp.Description("true to persist the inspection as a media check")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			path := argString(args, "path", "")
			if path == "" {
				svcName := argString(args, "service", "")
				fileID := argString(args, "file_id", "")
				if svcName == "" || fileID == "" {
					return toolErr("path, or service+file_id, is required"), nil
				}
				resolved, meta, err := resolveArrFilePath(ctx, registry, svcName, fileID)
				if err != nil {
					return toolErr("%v", err), nil
				}
				path = resolved
				// When the service hands us mediaInfo but the file itself is
				// unreachable from here, report the metadata honestly labeled
				// instead of failing: the LLM still learns the languages.
				if path == "" {
					return toolJSON(map[string]any{
						"source": "arr_mediainfo",
						"probed": false,
						"note":   "file is not reachable from the navigatorr host; values come from the service mediaInfo, verify with a filesystem inspection when possible",
						"media":  meta,
					}), nil
				}
			}
			real, err := d.Fs.ResolveRead(path)
			if err != nil {
				return toolErr("path not allowed: %v", err), nil
			}
			rep, err := mediainspect.InspectFile(ctx, d.Ffprobe, real)
			if err != nil {
				return toolErr("%v", err), nil
			}
			if argBool(args, "record", false) {
				mediaType := argString(args, "media_type", "")
				mediaID := argString(args, "media_id", "")
				if mediaType != "" {
					_, _ = d.Store.RecordCheck(store.MediaCheck{
						MediaType: mediaType, MediaID: mediaID, Path: rep.Path,
						Container: rep.Container, VideoCodec: rep.VideoCodec,
						Resolution: rep.Resolution, BitDepth: &rep.BitDepth,
						AudioLanguages: rep.AudioLanguages, SubtitleLanguages: rep.SubtitleLanguages,
						DangerousFiles: rep.DangerousFiles,
						DurationSec:    &rep.DurationSec, SizeBytes: &rep.SizeBytes,
					})
				}
			}
			return toolJSON(rep), nil
		},
	)

	// qbit_list_files — the torrent-content safety gate.
	if qbClient != nil {
		s.AddTool(
			mcp.NewTool("qbit_list_files",
				mcp.WithDescription("List the files inside a qBittorrent torrent by hash. Every name is flagged safe or dangerous (executables and disguised names like Episode.mkv.exe). Always call this before trusting a replacement download."),
				mcp.WithString("hash", mcp.Required(), mcp.Description("Torrent infohash")),
			),
			func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				hash := argString(req.GetArguments(), "hash", "")
				if hash == "" {
					return toolErr("hash is required"), nil
				}
				files, err := qbClient.ListFiles(ctx, hash)
				if err != nil {
					return toolErr("failed to list torrent files: %v", err), nil
				}
				names := make([]string, 0, len(files))
				for _, f := range files {
					names = append(names, f.Name)
				}
				dangerous := maint.ScanFilenames(names)
				flagged := map[string]bool{}
				for _, n := range dangerous {
					flagged[n] = true
				}
				type fileOut struct {
					Name      string  `json:"name"`
					Size      int64   `json:"size"`
					Progress  float64 `json:"progress"`
					Dangerous bool    `json:"dangerous"`
				}
				out := make([]fileOut, 0, len(files))
				for _, f := range files {
					out = append(out, fileOut{Name: f.Name, Size: f.Size, Progress: f.Progress, Dangerous: flagged[f.Name]})
				}
				// Cap: a big season pack must not flood the context.
				note := ""
				if len(out) > 100 {
					note = fmt.Sprintf("showing first 100 of %d files", len(out))
					out = out[:100]
				}
				return toolJSON(map[string]any{
					"files": out, "dangerous": dangerous, "note": note,
					"verdict": map[string]any{
						"safe": len(dangerous) == 0,
						"why":  firstOr(dangerous, "no executables or disguised names detected"),
					},
				}), nil
			},
		)
	}

	// rank_releases — deterministic scoring over Prowlarr/search results.
	s.AddTool(
		mcp.NewTool("rank_releases",
			mcp.WithDescription("Rank release candidates deterministically from preferences and the current media size. Scoring is rule-based (codec, group, subs, seeders, size reduction); the LLM applies the result instead of inventing points. Each candidate needs at least title; size, seeders, group, codec, resolution, audio/subs improve the score."),
			mcp.WithString("media_type", mcp.Description("anime, series or movie (default anime)")),
			mcp.WithString("current_size", mcp.Description("Current media size in bytes (enables size-reduction bonuses)")),
			mcp.WithString("candidates", mcp.Description("JSON array of candidates: [{guid,title,release_group,size,seeders,video_codec,resolution,bit_depth,audio_langs,sub_langs,dual_audio,multi_subs}]")),
			mcp.WithString("limit", mcp.Description("Max ranked releases to return (default 10)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			mediaType := argString(args, "media_type", "anime")
			raw, ok := args["candidates"]
			if !ok || raw == nil {
				return toolErr("candidates is required"), nil
			}
			var cands []maint.ReleaseCandidate
			if err := decodeCandidates(raw, &cands); err != nil {
				return toolErr("invalid candidates: %v", err), nil
			}
			if len(cands) == 0 {
				return toolErr("no candidates provided"), nil
			}
			if len(cands) > 50 {
				cands = cands[:50]
			}
			prefs := loadRankPrefs(d.Store, d.Config, mediaType)
			if argOk(args, "current_size") {
				prefs.CurrentSizeBytes = argInt64(args, "current_size", 0)
			}
			ranked := maint.RankRelease(cands, prefs)
			limit := int(argInt64(args, "limit", 10))
			if limit <= 0 || limit > 50 {
				limit = 10
			}
			if len(ranked) > limit {
				ranked = ranked[:limit]
			}
			return toolJSON(ranked), nil
		},
	)
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return "dangerous files: " + strings.Join(list, ", ")
	}
	return fallback
}

// decodeCandidates accepts the candidates argument as a native array or a
// JSON string, with tolerant per-field coercion.
func decodeCandidates(raw any, dst *[]maint.ReleaseCandidate) error {
	var items []map[string]any
	switch t := raw.(type) {
	case string:
		if err := json.Unmarshal([]byte(t), &items); err != nil {
			return err
		}
	case []any:
		for _, e := range t {
			m, ok := e.(map[string]any)
			if !ok {
				return fmt.Errorf("candidate entries must be objects")
			}
			items = append(items, m)
		}
	default:
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
	}
	for _, m := range items {
		c := maint.ReleaseCandidate{
			GUID:         strField(m, "guid"),
			Title:        strField(m, "title"),
			ReleaseGroup: strField(m, "release_group"),
			VideoCodec:   strField(m, "video_codec"),
			Resolution:   strField(m, "resolution"),
		}
		c.ReleaseGroup = firstNonEmpty(c.ReleaseGroup, strField(m, "group"))
		c.Size = numField(m, "size")
		c.Seeders = int(numField(m, "seeders"))
		c.BitDepth = int(numField(m, "bit_depth"))
		c.DualAudio = boolField(m, "dual_audio")
		c.MultiSubs = boolField(m, "multi_subs")
		c.AudioLangs = listField(m, "audio_langs")
		if len(c.AudioLangs) == 0 {
			c.AudioLangs = listField(m, "audio")
		}
		c.SubLangs = listField(m, "sub_langs")
		if len(c.SubLangs) == 0 {
			c.SubLangs = listField(m, "subs")
		}
		*dst = append(*dst, c)
	}
	return nil
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func numField(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		return argInt64(map[string]any{"v": t}, "v", 0)
	}
	return 0
}

func boolField(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	return argBool(map[string]any{"v": v}, "v", false)
}

func listField(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	return argStrings(map[string]any{"v": v}, "v")
}

// resolveArrFilePath asks Sonarr/Radarr where a file id lives on disk and
// returns its mediaInfo when the path is not reachable from this host.
func resolveArrFilePath(ctx context.Context, registry *arrservice.Registry, svcName, fileID string) (string, map[string]any, error) {
	svc, err := registry.Get(svcName)
	if err != nil {
		return "", nil, err
	}
	var endpoint string
	switch svcName {
	case "sonarr":
		endpoint = "/episodefile/" + fileID
	case "radarr":
		endpoint = "/moviefile/" + fileID
	default:
		return "", nil, fmt.Errorf("service %q does not expose file records (use sonarr or radarr)", svcName)
	}
	body, code, err := svc.DoRequest(ctx, "GET", endpoint, nil, nil)
	if err != nil {
		return "", nil, err
	}
	if code < 200 || code > 299 {
		return "", nil, fmt.Errorf("%s returned HTTP %d", endpoint, code)
	}
	var meta map[string]any
	if err := json.Unmarshal(body, &meta); err != nil {
		return "", nil, fmt.Errorf("decoding file record: %w", err)
	}
	path, _ := meta["path"].(string)
	return path, meta, nil
}
