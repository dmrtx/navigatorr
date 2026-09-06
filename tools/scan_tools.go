package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// scanTimeout bounds the whole library sweep. Per-series file sampling fans
// out below, but a wedged service must still fail the scan instead of
// hanging the MCP call indefinitely.
const scanTimeout = 4 * time.Minute

// scanWorkers caps concurrent per-series fetches so a 30-series scan does
// not open 30 simultaneous connections against Sonarr/Radarr.
const scanWorkers = 4

// scanFinding is one library issue found by a scan.
type scanFinding struct {
	Service   string `json:"service"`
	MediaType string `json:"media_type"`
	MediaID   string `json:"media_id"`
	Title     string `json:"title"`
	Issue     string `json:"issue_type"`
	Detail    string `json:"detail"`
	FileID    string `json:"file_id,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
	JobID     *int64 `json:"job_id,omitempty"`
}

type scanStats struct {
	ScanID          string
	Service         string
	Processed       int
	IssuesFound     int
	NeedsInspection int
	AutoResolved    int
	Complete        bool
	Issues          []scanFinding
}

func registerScanTools(s *server.MCPServer, d *Deps, registry *arrservice.Registry) {
	// scan_library_issues — dry run by default; creates jobs only when asked.
	s.AddTool(
		mcp.NewTool("scan_library_issues",
			mcp.WithDescription("Scan Sonarr/Radarr libraries for maintenance issues: oversized anime (per-episode size), movies lacking accessible audio/subs, dangerous filenames, possible title/file mismatches. Rebuilt with snapshots, bounded concurrency, language policy (EN/ES), und inspection, and auto-reconciliation. dry_run=true (default) only reports; dry_run=false creates maintenance jobs idempotently."),
			mcp.WithString("service", mcp.Description("sonarr, radarr or all (default all)")),
			mcp.WithString("dry_run", mcp.Description("true (default) to report only, false to create jobs")),
			mcp.WithString("limit", mcp.Description("Max entries to scan per service (default: 0 = scan entire library)")),
			mcp.WithString("per_episode_mb", mcp.Description("Oversized threshold per episode in MB (default from config)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			which := strings.ToLower(argString(args, "service", "all"))
			dry := argBool(args, "dry_run", true)
			limit := int(argInt64(args, "limit", 0))
			thresholdMB := argInt64(args, "per_episode_mb", d.Config.Maintenance.OversizedPerEpisodeMB)
			threshold := thresholdMB << 20

			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, scanTimeout)
			defer cancel()

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

			start := time.Now()
			var totalProcessed, totalIssuesFound, totalNeedsInspection, totalAutoResolved int
			var allIssues []scanFinding
			var scanIDs []string
			truncated := false

			for _, svcName := range services {
				stat, err := scanService(ctx, registry, d, svcName, limit, threshold, !dry)
				if err != nil {
					allIssues = append(allIssues, scanFinding{Service: svcName, Issue: "scan_error", Detail: err.Error()})
					continue
				}
				if stat.ScanID != "" {
					scanIDs = append(scanIDs, stat.ScanID)
				}
				totalProcessed += stat.Processed
				totalIssuesFound += stat.IssuesFound
				totalNeedsInspection += stat.NeedsInspection
				totalAutoResolved += stat.AutoResolved
				truncated = truncated || !stat.Complete
				allIssues = append(allIssues, stat.Issues...)
			}

			sampleIssues := allIssues
			if len(sampleIssues) > 15 {
				sampleIssues = sampleIssues[:15]
			}

			_ = d.Store.LogAction("scan_library", strings.Join(services, ","),
				"", fmt.Sprintf(`{"dry_run":%v}`, dry),
				fmt.Sprintf("processed=%d issues=%d needs_inspection=%d auto_resolved=%d",
					totalProcessed, totalIssuesFound, totalNeedsInspection, totalAutoResolved))

			timedOut := ctx.Err() == context.DeadlineExceeded
			res := map[string]any{
				"scan_id":          strings.Join(scanIDs, ","),
				"processed":        totalProcessed,
				"issues_found":     totalIssuesFound,
				"needs_inspection": totalNeedsInspection,
				"auto_resolved":    totalAutoResolved,
				"complete":         !truncated && !timedOut,
				"duration_ms":      time.Since(start).Milliseconds(),
				"dry_run":          dry,
				"issues":           sampleIssues,
			}
			if dry {
				res["note"] = "set dry_run=false to open maintenance jobs for these issues"
			}
			return toolJSON(res), nil
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
			mcp.WithDescription("Compute the SHA-256 of a file inside allowed_read_roots, to verify identity or change. Large media files take a while; the operation honours client cancellation."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File path")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			h, n, err := d.Fs.Hash(ctx, argString(req.GetArguments(), "path", ""))
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
func scanService(ctx context.Context, registry *arrservice.Registry, d *Deps, svcName string, limit int, threshold int64, create bool) (scanStats, error) {
	stat := scanStats{Service: svcName, Complete: true}
	svc, err := registry.Get(svcName)
	if err != nil {
		return stat, err
	}

	if svcName == "sonarr" {
		items, snapID, trunc, err := fetchCollectionSnapshot(ctx, registry, svc, "/series", limit)
		if err != nil {
			return stat, err
		}
		stat.ScanID = snapID
		stat.Complete = !trunc
		stat.Processed = len(items)

		perSeries := make([][]scanFinding, len(items))
		var wg sync.WaitGroup
		sem := make(chan struct{}, scanWorkers)

		for i, m := range items {
			wg.Add(1)
			go func(i int, m map[string]any) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				title, _ := m["title"].(string)
				idStr := numStr(m["id"])
				var f []scanFinding
				stats, _ := m["statistics"].(map[string]any)
				size := int64(numVal(stats["sizeOnDisk"]))
				count := int(numVal(stats["episodeFileCount"]))

				if maint.IsOversizedEpisode(size, count, threshold) {
					f = append(f, scanFinding{
						Service: "sonarr", MediaType: "series",
						MediaID: idStr, Title: title, Issue: maint.IssueOversized,
						Detail: fmt.Sprintf("%.1fGB across %d files (~%.1fGB/episode)",
							float64(size)/1e9, count, float64(size)/float64(count)/1e9),
					})
				} else if count > 0 {
					_, _, _ = d.Store.AutoResolveStale("sonarr", "series", idStr, maint.IssueOversized, "per-episode size within threshold")
				}

				files, _, ferr := arrCollection(ctx, svc, "/episodefile?seriesId="+idStr, 5)
				if ferr != nil {
					perSeries[i] = f
					return
				}
				findings := fileLevelFindings("sonarr", "series", idStr, title, files)
				f = append(f, findings...)
				perSeries[i] = f
			}(i, m)
		}
		wg.Wait()

		for _, f := range perSeries {
			for _, item := range f {
				if item.Issue == maint.IssueNeedsInspection {
					stat.NeedsInspection++
				} else {
					stat.IssuesFound++
				}
				stat.Issues = append(stat.Issues, item)
			}
		}
		if create {
			attachJobs(d, stat.Issues)
		}
		return stat, nil
	}

	if svcName == "radarr" {
		items, snapID, trunc, err := fetchCollectionSnapshot(ctx, registry, svc, "/movie", limit)
		if err != nil {
			return stat, err
		}
		stat.ScanID = snapID
		stat.Complete = !trunc
		stat.Processed = len(items)

		for _, m := range items {
			title, _ := m["title"].(string)
			idStr := numStr(m["id"])
			mf, ok := m["movieFile"].(map[string]any)
			if !ok || mf == nil {
				continue
			}
			fileID := numStr(mf["id"])
			fileSize := int64(numVal(mf["size"]))
			relPath, _ := mf["relativePath"].(string)
			if relPath == "" {
				relPath, _ = mf["path"].(string)
			}

			if relPath != "" && maint.IsDangerousFilename(relPath) {
				stat.IssuesFound++
				stat.Issues = append(stat.Issues, scanFinding{
					Service: "radarr", MediaType: "movie", MediaID: idStr,
					Title: title, Issue: maint.IssueDangerousMedia, Detail: "dangerous name: " + relPath,
					FileID: fileID, FileSize: fileSize,
				})
			}
			if relPath != "" && maint.PossibleMismatch(title, relPath) {
				stat.IssuesFound++
				stat.Issues = append(stat.Issues, scanFinding{
					Service: "radarr", MediaType: "movie", MediaID: idStr,
					Title: title, Issue: maint.IssuePossibleMismatch, Detail: "file does not mention the title: " + relPath,
					FileID: fileID, FileSize: fileSize,
				})
			}

			mi, _ := mf["mediaInfo"].(map[string]any)
			var audio, subs []string
			if mi != nil {
				audio = langList(mi["audioLanguages"])
				subs = langList(mi["subtitles"])
				if len(subs) == 0 {
					subs = langList(mi["subtitleLanguages"])
				}
			}

			verdict := maint.EvaluateLanguageAccessibility(audio, subs)
			if verdict == "accessible" {
				if _, resolved, _ := d.Store.AutoResolveStale("radarr", "movie", idStr, maint.IssueMissingLanguage, "current file has accessible audio/subtitles"); resolved {
					stat.AutoResolved++
				}
				continue
			}

			if verdict == maint.IssueNeedsInspection {
				// Check if we already inspected this exact file_id
				check, _ := d.Store.GetLatestMediaCheck("movie", idStr, fileID)
				if check != nil {
					pVerdict := maint.EvaluateLanguageAccessibility(check.AudioLanguages, check.SubtitleLanguages)
					if pVerdict == "accessible" {
						if _, resolved, _ := d.Store.AutoResolveStale("radarr", "movie", idStr, maint.IssueMissingLanguage, "inspected file has accessible audio/subtitles"); resolved {
							stat.AutoResolved++
						}
						continue
					}
					verdict = pVerdict
				} else {
					// Real inspection via ffprobe + sidecars if reachable
					fullPath := relPath
					if p, ok := mf["path"].(string); ok && p != "" {
						fullPath = p
					}
					if fullPath != "" && d.Fs != nil {
						if realPath, rerr := d.Fs.ResolveRead(fullPath); rerr == nil {
							if registry != nil && registry.Pool != nil {
								_ = registry.Pool.AcquireMedia(ctx)
							}
							rep, ierr := mediainspect.InspectFile(ctx, d.Ffprobe, realPath)
							if registry != nil && registry.Pool != nil {
								registry.Pool.ReleaseMedia()
							}
							if ierr == nil && rep.Probed {
								probedSubs := append(rep.SubtitleLanguages, langNames(rep.ExternalSubtitles)...)
								_, _ = d.Store.RecordCheck(store.MediaCheck{
									MediaType: "movie", MediaID: idStr, FileID: fileID, Path: rep.Path,
									AudioLanguages: rep.AudioLanguages, SubtitleLanguages: probedSubs,
									Container: rep.Container, VideoCodec: rep.VideoCodec,
									Resolution: rep.Resolution, SizeBytes: &rep.SizeBytes,
								})
								pVerdict := maint.EvaluateLanguageAccessibility(rep.AudioLanguages, probedSubs)
								if pVerdict == "accessible" {
									if _, resolved, _ := d.Store.AutoResolveStale("radarr", "movie", idStr, maint.IssueMissingLanguage, "inspected file has accessible audio/subtitles"); resolved {
										stat.AutoResolved++
									}
									continue
								}
								verdict = pVerdict
							}
						}
					}
				}
			}

			if verdict == maint.IssueMissingLanguage {
				stat.IssuesFound++
				stat.Issues = append(stat.Issues, scanFinding{
					Service: "radarr", MediaType: "movie", MediaID: idStr,
					Title: title, Issue: maint.IssueMissingLanguage,
					Detail: "no English or Spanish audio or subtitles",
					FileID: fileID, FileSize: fileSize,
				})
			} else if verdict == maint.IssueNeedsInspection {
				stat.NeedsInspection++
				stat.Issues = append(stat.Issues, scanFinding{
					Service: "radarr", MediaType: "movie", MediaID: idStr,
					Title: title, Issue: maint.IssueNeedsInspection,
					Detail: "audio/subtitle languages indeterminate (und/unknown/empty); needs real inspection",
					FileID: fileID, FileSize: fileSize,
				})
			}
		}

		if create {
			attachJobs(d, stat.Issues)
		}
		return stat, nil
	}

	return stat, fmt.Errorf("unsupported service %s", svcName)
}

func fetchCollectionSnapshot(ctx context.Context, registry *arrservice.Registry, svc *arrservice.Service, path string, limit int) ([]map[string]any, string, bool, error) {
	if registry != nil && registry.Snapshots != nil {
		if snap, ok := registry.Snapshots.Find(svc.Name, path, ""); ok {
			items := toMapSlice(snap.Items)
			if limit > 0 && len(items) > limit {
				return items[:limit], snap.ID, true, nil
			}
			return items, snap.ID, false, nil
		}
	}

	body, code, err := svc.DoRequest(ctx, "GET", path, nil, nil)
	if err != nil {
		return nil, "", false, err
	}
	if code < 200 || code > 299 {
		return nil, "", false, fmt.Errorf("%s: HTTP %d", path, code)
	}

	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, "", false, fmt.Errorf("%s: unexpected response shape", path)
	}

	snapID := ""
	if registry != nil && registry.Snapshots != nil {
		rawAny := make([]any, len(items))
		for i, it := range items {
			rawAny[i] = it
		}
		snap := registry.Snapshots.Create(svc.Name, path, "", rawAny)
		snapID = snap.ID
	}

	if limit > 0 && len(items) > limit {
		return items[:limit], snapID, true, nil
	}
	return items, snapID, false, nil
}

func toMapSlice(in []any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, it := range in {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func attachJobs(d *Deps, issues []scanFinding) {
	for i, f := range issues {
		if f.Issue == "scan_error" {
			continue
		}
		it := store.MaintenanceItem{
			MediaType:         f.MediaType,
			Service:           f.Service,
			MediaID:           f.MediaID,
			Title:             f.Title,
			IssueType:         f.Issue,
			Notes:             f.Detail,
			RequiresSubtitles: f.Issue == maint.IssueMissingLanguage,
		}
		if f.FileID != "" {
			it.CurrentFileID = &f.FileID
		}
		if f.FileSize > 0 {
			it.CurrentSize = &f.FileSize
		}
		created, err := d.Store.AddItem(it)
		if err == nil {
			issues[i].JobID = &created.ID
		}
	}
}

// fileLevelFindings derives language/dangerous/mismatch issues from file records.
func fileLevelFindings(service, mediaType, mediaID, title string, files []map[string]any) []scanFinding {
	var out []scanFinding
	anyAccessible := false
	needsInspection := false
	sampled := 0
	for _, f := range files {
		relPath, _ := f["relativePath"].(string)
		if relPath == "" {
			relPath, _ = f["path"].(string)
		}
		fileID := numStr(f["id"])
		fileSize := int64(numVal(f["size"]))

		if relPath != "" && maint.IsDangerousFilename(relPath) {
			out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
				Title: title, Issue: maint.IssueDangerousMedia, Detail: "dangerous name: " + relPath,
				FileID: fileID, FileSize: fileSize})
		}
		if relPath != "" && maint.PossibleMismatch(title, relPath) {
			out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
				Title: title, Issue: maint.IssuePossibleMismatch, Detail: "file does not mention the title: " + relPath,
				FileID: fileID, FileSize: fileSize})
		}
		mi, _ := f["mediaInfo"].(map[string]any)
		if mi == nil {
			needsInspection = true
			continue
		}
		audio := langList(mi["audioLanguages"])
		subs := langList(mi["subtitles"])
		if len(subs) == 0 {
			subs = langList(mi["subtitleLanguages"])
		}
		sampled++
		verdict := maint.EvaluateLanguageAccessibility(audio, subs)
		if verdict == "accessible" {
			anyAccessible = true
		} else if verdict == maint.IssueNeedsInspection {
			needsInspection = true
		}
	}
	if sampled > 0 && !anyAccessible {
		if needsInspection {
			out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
				Title: title, Issue: maint.IssueNeedsInspection,
				Detail: fmt.Sprintf("audio/subtitle languages indeterminate in %d sampled file(s); inspect the real files", sampled)})
		} else {
			out = append(out, scanFinding{Service: service, MediaType: mediaType, MediaID: mediaID,
				Title: title, Issue: maint.IssueMissingLanguage,
				Detail: fmt.Sprintf("no English/Spanish audio or subtitles in %d sampled file(s)", sampled)})
		}
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
