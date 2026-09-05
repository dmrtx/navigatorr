package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// scanFinding is one library issue found by a scan.
type scanFinding struct {
	Service   string `json:"service"`
	MediaType string `json:"media_type"`
	MediaID   string `json:"media_id"`
	Title     string `json:"title"`
	Issue     string `json:"issue_type"`
	Detail    string `json:"detail"`
	JobID     *int64 `json:"job_id,omitempty"`
}

func registerScanTools(s *server.MCPServer, d *Deps, registry *arrservice.Registry) {
	// scan_library_issues — dry run by default; creates jobs only when asked.
	s.AddTool(
		mcp.NewTool("scan_library_issues",
			mcp.WithDescription("Scan Sonarr/Radarr libraries for maintenance issues: oversized anime (per-episode size), movies lacking accessible audio/subs, dangerous filenames, possible title/file mismatches. dry_run=true (default) only reports; dry_run=false creates maintenance jobs idempotently."),
			mcp.WithString("service", mcp.Description("sonarr, radarr or all (default all)")),
			mcp.WithString("dry_run", mcp.Description("true (default) to report only, false to create jobs")),
			mcp.WithString("limit", mcp.Description("Max entries to scan per service (default 30)")),
			mcp.WithString("per_episode_mb", mcp.Description("Oversized threshold per episode in MB (default from config)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			which := strings.ToLower(argString(args, "service", "all"))
			dry := argBool(args, "dry_run", true)
			limit := int(argInt64(args, "limit", 30))
			if limit <= 0 || limit > 100 {
				limit = 30
			}
			thresholdMB := argInt64(args, "per_episode_mb", d.Config.Maintenance.OversizedPerEpisodeMB)
			threshold := thresholdMB << 20
			services := []string{}
			switch which {
			case "sonarr", "radarr":
				services = []string{which}
			case "all", "":
				for _, name := range registry.List() {
					if name == "sonarr" || name == "radarr" {
						services = append(services, name)
					}
				}
			default:
				return toolErr("service must be sonarr, radarr or all"), nil
			}
			var issues []scanFinding
			truncated := false
			for _, svcName := range services {
				more, trunc, err := scanService(ctx, registry, d, svcName, limit, threshold, !dry)
				if err != nil {
					issues = append(issues, scanFinding{Service: svcName, Issue: "scan_error", Detail: err.Error()})
					continue
				}
				issues = append(issues, more...)
				truncated = truncated || trunc
			}
			_ = d.Store.LogAction("scan_library", strings.Join(services, ","),
				"", fmt.Sprintf(`{"dry_run":%v}`, dry), fmt.Sprintf("%d issues", len(issues)))
			return toolJSON(map[string]any{
				"dry_run": dry, "issues": issues, "truncated": truncated,
				"note": firstOr(nil, "set dry_run=false to open maintenance jobs for these issues"),
			}), nil
		},
	)

	// get_context — compact LLM briefing.
	s.AddTool(
		mcp.NewTool("get_context",
			mcp.WithDescription("Get a compact briefing: relevant preferences, active jobs, recent decisions and latest actions. Bounded output, never a database dump."),
			mcp.WithString("scope", mcp.Description("anime, movies or global (default global)")),
			mcp.WithString("media_type", mcp.Description("series, anime or movie (narrows jobs)")),
			mcp.WithString("media_id", mcp.Description("Service-side id (narrows jobs with media_type)")),
			mcp.WithString("limit", mcp.Description("Max jobs (default 5, max 20)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			c, err := d.Store.GetContext(
				argString(args, "scope", "global"),
				argString(args, "media_type", ""),
				argString(args, "media_id", ""),
				int(argInt64(args, "limit", 5)))
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(c), nil
		},
	)

	// block_release / block_list
	s.AddTool(
		mcp.NewTool("block_release",
			mcp.WithDescription("Blocklist a release guid/hash/URL so rank_releases and safe_replace refuse it. Source defaults to manual; the workflow uses auto."),
			mcp.WithString("identifier", mcp.Required(), mcp.Description("Release guid, infohash or URL")),
			mcp.WithString("reason", mcp.Description("Why it is blocked")),
			mcp.WithString("source", mcp.Description("manual or auto")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id := argString(args, "identifier", "")
			if id == "" {
				return toolErr("identifier is required"), nil
			}
			if err := d.Store.BlockRelease(id, argString(args, "reason", ""), argString(args, "source", "manual")); err != nil {
				return toolErr("%v", err), nil
			}
			return mcp.NewToolResultText("Blocked " + id), nil
		},
	)
	s.AddTool(
		mcp.NewTool("block_list",
			mcp.WithDescription("List blocklisted releases, newest first."),
			mcp.WithString("limit", mcp.Description("Max entries (default 50)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			list, err := d.Store.ListBlocked(int(argInt64(req.GetArguments(), "limit", 50)))
			if err != nil {
				return toolErr("%v", err), nil
			}
			if len(list) == 0 {
				return mcp.NewToolResultText("Blocklist is empty."), nil
			}
			return toolJSON(list), nil
		},
	)
}

func registerFsTools(s *server.MCPServer, d *Deps) {
	// fs_stat
	s.AddTool(
		mcp.NewTool("fs_stat",
			mcp.WithDescription("Stat a file inside allowed_read_roots: size, type, timestamps."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Filesystem path")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			st, err := d.Fs.FileStat(argString(req.GetArguments(), "path", ""))
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(st), nil
		},
	)
	// fs_list
	s.AddTool(
		mcp.NewTool("fs_list",
			mcp.WithDescription("List a directory inside allowed_read_roots. Bounded output (default 100 entries, depth 1, max depth 4)."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Directory path")),
			mcp.WithString("depth", mcp.Description("Recursion depth 1-4 (default 1)")),
			mcp.WithString("limit", mcp.Description("Max entries (default 100, max 500)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			list, err := d.Fs.List(argString(args, "path", ""),
				int(argInt64(args, "depth", 1)), int(argInt64(args, "limit", 100)))
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(list), nil
		},
	)
	// fs_hash
	s.AddTool(
		mcp.NewTool("fs_hash",
			mcp.WithDescription("Compute the SHA-256 of a file inside allowed_read_roots, to verify identity or change."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			h, n, err := d.Fs.Hash(argString(req.GetArguments(), "path", ""))
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(map[string]any{"sha256": h, "size": n}), nil
		},
	)
	// fs_safe_move — both ends inside write roots.
	s.AddTool(
		mcp.NewTool("fs_safe_move",
			mcp.WithDescription("Move/rename a file where both source and destination stay inside allowed_write_roots. Refuses to overwrite."),
			mcp.WithString("source", mcp.Required(), mcp.Description("Source path")),
			mcp.WithString("destination", mcp.Required(), mcp.Description("Destination path")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			src, dst := argString(args, "source", ""), argString(args, "destination", "")
			if err := d.Fs.Move(src, dst); err != nil {
				return toolErr("%v", err), nil
			}
			_ = d.Store.LogAction("fs_move", "", src, fmt.Sprintf(`{"destination":%q}`, dst), "moved")
			return mcp.NewToolResultText("Moved " + src + " -> " + dst), nil
		},
	)
	// fs_safe_delete — only for a validated job in replacing state.
	s.AddTool(
		mcp.NewTool("fs_safe_delete",
			mcp.WithDescription("Delete ONE file inside allowed_write_roots, only when it belongs to a maintenance job in replacing state (verified + imported), confirm=true AND allow_destructive is enabled. Never deletes directories."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path")),
			mcp.WithString("maintenance_item_id", mcp.Required(), mcp.Description("Authorizing job id")),
			mcp.WithString("confirm", mcp.Description("Must be true")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			path := argString(args, "path", "")
			itemID := argInt64(args, "maintenance_item_id", 0)
			if path == "" || itemID <= 0 {
				return toolErr("path and maintenance_item_id are required"), nil
			}
			it, err := d.Store.GetItem(itemID)
			if err != nil {
				return toolErr("%v", err), nil
			}
			if it.Status != store.MaintReplacing {
				return toolErr("job %d is %s, not replacing: the file stays (verify + import first)", itemID, it.Status), nil
			}
			// The global kill-switch covers this path like every other
			// destructive one; the job state alone must not authorize deletes.
			if !d.Config.AllowDestructive {
				return toolErr("deletion is disabled. Set allow_destructive: true in config.yaml to enable; the file stays"), nil
			}
			if !argBool(args, "confirm", false) {
				return toolErr("confirm=true is required; the file stays"), nil
			}
			if err := d.Fs.Delete(path); err != nil {
				return toolErr("%v", err), nil
			}
			_ = d.Store.LogAction("fs_delete", it.Service, it.Title,
				fmt.Sprintf(`{"path":%q,"item":%d}`, path, itemID), "deleted after verified import")
			return mcp.NewToolResultText("Deleted " + path), nil
		},
	)
	// scan_dangerous_files
	s.AddTool(
		mcp.NewTool("scan_dangerous_files",
			mcp.WithDescription("Walk a directory inside allowed_read_roots (bounded) looking for executables and disguised names like Episode.mkv.exe."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Root to scan")),
			mcp.WithString("max_files", mcp.Description("Max entries to visit (default 2000)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			real, err := d.Fs.ResolveRead(argString(args, "path", ""))
			if err != nil {
				return toolErr("path not allowed: %v", err), nil
			}
			found, err := mediainspect.ScanDangerous(real, int(argInt64(args, "max_files", 2000)))
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(map[string]any{"dangerous": found, "count": len(found)}), nil
		},
	)
}

// scanService inspects one *arr library with bounded, field-selected calls.
func scanService(ctx context.Context, registry *arrservice.Registry, d *Deps, svcName string, limit int, threshold int64, create bool) ([]scanFinding, bool, error) {
	svc, err := registry.Get(svcName)
	if err != nil {
		return nil, false, err
	}
	var issues []scanFinding
	if svcName == "sonarr" {
		items, trunc, err := arrCollection(ctx, svc, "/series", limit)
		if err != nil {
			return nil, false, err
		}
		for _, m := range items {
			title, _ := m["title"].(string)
			idStr := numStr(m["id"])
			stats, _ := m["statistics"].(map[string]any)
			size := int64(numVal(stats["sizeOnDisk"]))
			count := int(numVal(stats["episodeFileCount"]))
			if maint.IsOversizedEpisode(size, count, threshold) {
				issues = append(issues, scanFinding{Service: "sonarr", MediaType: "series",
					MediaID: idStr, Title: title, Issue: maint.IssueOversized,
					Detail: fmt.Sprintf("%.1fGB across %d files (~%.1fGB/episode)",
						float64(size)/1e9, count, float64(size)/float64(count)/1e9)})
			}
			// Sample episode files for language/dangerous signals.
			files, _, ferr := arrCollection(ctx, svc, "/episodefile?seriesId="+idStr, 5)
			if ferr != nil {
				continue
			}
			issues = append(issues, fileLevelFindings("sonarr", "series", idStr, title, files)...)
		}
		if create {
			attachJobs(d, issues)
		}
		return issues, trunc, nil
	}
	items, trunc, err := arrCollection(ctx, svc, "/movie", limit)
	if err != nil {
		return nil, false, err
	}
	for _, m := range items {
		title, _ := m["title"].(string)
		idStr := numStr(m["id"])
		if mf, ok := m["movieFile"].(map[string]any); ok && mf != nil {
			issues = append(issues, fileLevelFindings("radarr", "movie", idStr, title, []map[string]any{mf})...)
		}
	}
	if create {
		attachJobs(d, issues)
	}
	return issues, trunc, nil
}

func attachJobs(d *Deps, issues []scanFinding) {
	for i, f := range issues {
		if f.Issue == "scan_error" {
			continue
		}
		it, err := d.Store.AddItem(store.MaintenanceItem{
			MediaType: f.MediaType, Service: f.Service, MediaID: f.MediaID,
			Title: f.Title, IssueType: f.Issue, Notes: f.Detail,
			RequiresSubtitles: f.Issue == maint.IssueMissingLanguage,
		})
		if err == nil {
			issues[i].JobID = &it.ID
		}
	}
}

// fileLevelFindings derives language/dangerous/mismatch issues from file records.
func fileLevelFindings(service, mediaType, mediaID, title string, files []map[string]any) []scanFinding {
	var out []scanFinding
	anyAccessible := false
	sampled := 0
	for _, f := range files {
		relPath, _ := f["relativePath"].(string)
		if relPath == "" {
			relPath, _ = f["path"].(string)
		}
		if relPath != "" && maint.IsDangerousFilename(relPath) {
			out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
				Title: title, Issue: maint.IssueDangerousMedia, Detail: "dangerous name: " + relPath})
		}
		if relPath != "" && maint.PossibleMismatch(title, relPath) {
			out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
				Title: title, Issue: maint.IssuePossibleMismatch, Detail: "file does not mention the title: " + relPath})
		}
		mi, _ := f["mediaInfo"].(map[string]any)
		if mi == nil {
			continue
		}
		audio := langList(mi["audioLanguages"])
		subs := langList(mi["subtitles"])
		if len(subs) == 0 {
			subs = langList(mi["subtitleLanguages"])
		}
		sampled++
		if !maint.NeedsAccessibleSubtitles(audio, subs) {
			anyAccessible = true
		}
	}
	if sampled > 0 && !anyAccessible {
		out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
			Title: title, Issue: maint.IssueMissingLanguage,
			Detail: fmt.Sprintf("no English/Spanish audio or subtitles in %d sampled file(s); inspect the real files", sampled)})
	}
	return out
}

// arrCollection GETs an *arr collection endpoint with hard size caps.
func arrCollection(ctx context.Context, svc *arrservice.Service, path string, limit int) ([]map[string]any, bool, error) {
	body, code, err := svc.DoRequest(ctx, "GET", path, nil, nil)
	if err != nil {
		return nil, false, err
	}
	if code < 200 || code > 299 {
		return nil, false, fmt.Errorf("%s: HTTP %d", path, code)
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, false, fmt.Errorf("%s: unexpected response shape", path)
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

func numVal(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}

func numStr(v any) string {
	switch t := v.(type) {
	case float64:
		return fmt.Sprintf("%.0f", t)
	case string:
		return t
	}
	return fmt.Sprintf("%v", v)
}

// langList tolerates both ["eng"] and [{name:"English"}] shapes.
func langList(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			switch x := e.(type) {
			case string:
				out = append(out, x)
			case map[string]any:
				if n, ok := x["name"].(string); ok {
					out = append(out, n)
				} else if n, ok := x["languageName"].(string); ok {
					out = append(out, n)
				}
			}
		}
	case []string:
		out = t
	case string:
		if t != "" {
			out = []string{t}
		}
	}
	return out
}
