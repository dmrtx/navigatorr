package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/maint"
	"github.com/jakenesler/navigatorr/mediainspect"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var magnetHashRe = regexp.MustCompile(`(?i)btih:([a-z0-9]+)`)

// safeDeps bundles what the safe-replacement workflow needs.
type safeDeps struct {
	*Deps
	registry         *arrservice.Registry
	qb               *qbit.Client
	allowDestructive bool
}

func registerSafeReplaceTools(s *server.MCPServer, d *Deps, registry *arrservice.Registry, qbClient *qbit.Client, allowDestructive bool) {
	sd := &safeDeps{Deps: d, registry: registry, qb: qbClient, allowDestructive: allowDestructive}

	s.AddTool(
		mcp.NewTool("safe_replace",
			mcp.WithDescription("Advance a maintenance job one safe step at a time: plan, select, add_torrent, torrent_check, verify, import_confirm, delete_original, finish. Every step is idempotent and persisted, so retries and restarts resume instead of duplicating work. The original media is NEVER deleted before verification plus a confirmed import."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Numeric maintenance job id")),
			mcp.WithString("step", mcp.Required(), mcp.Description("plan, select, add_torrent, torrent_check, verify, import_confirm, delete_original, finish")),
			mcp.WithString("release_guid", mcp.Description("select: chosen release guid")),
			mcp.WithString("title", mcp.Description("select: chosen release title")),
			mcp.WithString("release_group", mcp.Description("select: release group")),
			mcp.WithString("size", mcp.Description("select/verify: release size in bytes")),
			mcp.WithString("seeders", mcp.Description("select: seeders at decision time")),
			mcp.WithString("score", mcp.Description("select: rank_releases score")),
			mcp.WithString("reasons", mcp.Description("select: reasons JSON array or csv")),
			mcp.WithString("rejected", mcp.Description("select: JSON array of {title,reason} for rejected alternates")),
			mcp.WithString("url", mcp.Description("add_torrent: magnet link or torrent URL")),
			mcp.WithString("save_path", mcp.Description("add_torrent: optional save path")),
			mcp.WithString("torrent_hash", mcp.Description("add_torrent/torrent_check: infohash (taken from the magnet btih when present)")),
			mcp.WithString("files", mcp.Description("add_torrent: optional known file list (csv) for a pre-add safety check")),
			mcp.WithString("path", mcp.Description("verify: replacement files location; delete_original: original file path")),
			mcp.WithString("audio_langs", mcp.Description("verify: replacement audio languages (csv) when no filesystem access")),
			mcp.WithString("sub_langs", mcp.Description("verify: replacement subtitle languages (csv)")),
			mcp.WithString("complete", mcp.Description("verify: true when all episodes/files are present")),
			mcp.WithString("new_file_id", mcp.Description("import_confirm: Sonarr/Radarr file id now associated with the media")),
			mcp.WithString("via", mcp.Description("delete_original: filesystem or arr (default arr)")),
			mcp.WithString("confirm", mcp.Description("delete_original: must be true; without it nothing is deleted")),
			mcp.WithString("notes", mcp.Description("finish: outcome notes, e.g. space saved")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id, errRes := parseID(args)
			if errRes != nil {
				return errRes, nil
			}
			step := strings.ToLower(argString(args, "step", ""))
			var res *mcp.CallToolResult
			var err error
			switch step {
			case "plan":
				res, err = sd.stepPlan(id)
			case "select":
				res, err = sd.stepSelect(args, id)
			case "add_torrent":
				res, err = sd.stepAddTorrent(ctx, args, id)
			case "torrent_check":
				res, err = sd.stepTorrentCheck(ctx, args, id)
			case "verify":
				res, err = sd.stepVerify(ctx, args, id)
			case "import_confirm":
				res, err = sd.stepImportConfirm(ctx, args, id)
			case "delete_original":
				res, err = sd.stepDeleteOriginal(ctx, args, id)
			case "finish":
				res, err = sd.stepFinish(args, id)
			default:
				return toolErr("unknown step %q (use: plan, select, add_torrent, torrent_check, verify, import_confirm, delete_original, finish)", step), nil
			}
			if err != nil {
				return toolErr("%v", err), nil
			}
			return res, nil
		},
	)

	// cleanup_imported_downloads — explicit, evidence-gated torrent cleanup.
	s.AddTool(
		mcp.NewTool("cleanup_imported_downloads",
			mcp.WithDescription("List or remove completed qBittorrent downloads. Removal NEVER acts on progress alone: it needs explicit hashes, a confirmed-import flag, and allow_destructive. Modes: list (default), remove_torrent, remove_torrent_data (also needs confirm=true)."),
			mcp.WithString("mode", mcp.Description("list, remove_torrent or remove_torrent_data")),
			mcp.WithString("hashes", mcp.Description("Comma-separated infohashes (required for removal; omit in list mode for all completed)")),
			mcp.WithString("confirmed_imported", mcp.Description("Must be true for removal: Sonarr/Radarr already imported and the library file exists")),
			mcp.WithString("confirm", mcp.Description("Must be true for remove_torrent_data")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			mode := strings.ToLower(argString(args, "mode", "list"))
			if sd.qb == nil {
				return toolErr("qbittorrent is not configured"), nil
			}
			torrents, err := sd.qb.ListTorrents(ctx)
			if err != nil {
				return toolErr("listing torrents: %v", err), nil
			}
			var filter map[string]bool
			if h := argString(args, "hashes", ""); h != "" {
				filter = map[string]bool{}
				for _, x := range strings.Split(h, ",") {
					if x = strings.TrimSpace(x); x != "" {
						filter[strings.ToLower(x)] = true
					}
				}
			}
			type row struct {
				Hash     string  `json:"hash"`
				Name     string  `json:"name"`
				Progress float64 `json:"progress"`
				Size     int64   `json:"size"`
			}
			var completed []row
			for _, t := range torrents {
				if t.Progress < 1 {
					continue
				}
				if filter != nil && !filter[strings.ToLower(t.Hash)] {
					continue
				}
				completed = append(completed, row{Hash: t.Hash, Name: t.Name, Progress: t.Progress, Size: t.Size})
			}
			if mode == "list" {
				_ = d.Store.LogAction("cleanup_list", "qbittorrent", "",
					`{"mode":"list"}`, fmt.Sprintf("%d completed", len(completed)))
				return toolJSON(map[string]any{"completed": completed}), nil
			}
			if mode != "remove_torrent" && mode != "remove_torrent_data" {
				return toolErr("unknown mode %q", mode), nil
			}
			if filter == nil || len(filter) == 0 {
				return toolErr("hashes must name explicit torrents for removal"), nil
			}
			if !argBool(args, "confirmed_imported", false) {
				return toolErr("refusing removal without confirmed_imported=true: verify the Sonarr/Radarr import and the library file first"), nil
			}
			withData := mode == "remove_torrent_data"
			if withData && !argBool(args, "confirm", false) {
				return toolErr("remove_torrent_data also needs confirm=true"), nil
			}
			if !sd.allowDestructive {
				return toolErr("removal is disabled. Set allow_destructive: true in config.yaml to enable."), nil
			}
			var hashes []string
			for _, c := range completed {
				hashes = append(hashes, c.Hash)
			}
			if len(hashes) == 0 {
				return toolErr("none of the named torrents is complete; nothing removed"), nil
			}
			if err := sd.qb.DeleteTorrents(ctx, hashes, withData); err != nil {
				return toolErr("removal failed: %v", err), nil
			}
			_ = d.Store.LogAction("cleanup_remove", "qbittorrent", strings.Join(hashes, ","),
				fmt.Sprintf(`{"mode":%q,"confirmed_imported":true}`, mode),
				fmt.Sprintf("removed %d torrent(s)", len(hashes)))
			return toolJSON(map[string]any{"removed": hashes, "delete_files": withData}), nil
		},
	)
}

func (sd *safeDeps) stepPlan(id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	if it.Status != store.MaintPending {
		return toolJSON(map[string]any{
			"item": it, "note": "already past plan (" + it.Status + ")",
		}), nil
	}
	warn := ""
	if it.CurrentFileID == nil && it.CurrentSize == nil {
		warn = "no current-file snapshot recorded; set current_file_id/current_size via maintenance_update before verify"
	}
	it, err = sd.Store.Transition(id, store.MaintResearching, "safe_replace plan: research candidates")
	if err != nil {
		return nil, err
	}
	_ = sd.Store.LogAction("safe_replace_plan", it.Service, it.Title,
		fmt.Sprintf(`{"id":%d}`, id), "researching")
	return toolJSON(map[string]any{"item": it, "warning": warn,
		"next": "search Prowlarr/Sonarr/Radarr for candidates, then safe_replace step=select"}), nil
}

func (sd *safeDeps) stepSelect(args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	guid := argString(args, "release_guid", "")
	title := argString(args, "title", "")
	if title == "" {
		return nil, fmt.Errorf("title is required")
	}
	if it.Status == store.MaintCandidate && it.ReplacementReleaseGUID != nil && *it.ReplacementReleaseGUID == guid && guid != "" {
		return toolJSON(map[string]any{"item": it, "note": "candidate already selected (idempotent)"}), nil
	}
	if it.Status != store.MaintResearching && it.Status != store.MaintCandidate {
		return nil, fmt.Errorf("select needs status researching or candidate_found, item is %s", it.Status)
	}
	var score *float64
	if argOk(args, "score") {
		f := argFloat(args, "score", 0)
		score = &f
	}
	var seeders *int
	if argOk(args, "seeders") {
		n := int(argInt64(args, "seeders", 0))
		seeders = &n
	}
	var size *int64
	if argOk(args, "size") {
		n := argInt64(args, "size", 0)
		size = &n
	}
	reasons := argStrings(args, "reasons")
	reasonsJSON := "[]"
	if len(reasons) > 0 {
		b, _ := json.Marshal(reasons)
		reasonsJSON = string(b)
	}
	if _, err := sd.Store.RecordDecision(store.ReleaseDecision{
		MaintenanceItemID: id, ReleaseGUID: guid, Title: title,
		ReleaseGroup: argString(args, "release_group", ""),
		Size:         size, SeedersAtDecision: seeders, Decision: store.DecisionSelected,
		Score: score, ReasonsJSON: reasonsJSON,
	}); err != nil {
		return nil, err
	}
	// Record rejected alternates alongside, so "why not SubsPlease" survives.
	if raw, ok := args["rejected"]; ok && raw != nil {
		if arr, ok := raw.([]any); ok {
			for _, e := range arr {
				m, ok := e.(map[string]any)
				if !ok {
					continue
				}
				t := ""
				if v, ok := m["title"].(string); ok {
					t = v
				}
				if t == "" {
					continue
				}
				r := ""
				if v, ok := m["reason"].(string); ok {
					r = v
				}
				_, _ = sd.Store.RecordDecision(store.ReleaseDecision{
					MaintenanceItemID: id, Title: t, Decision: store.DecisionRejected,
					ReasonsJSON: fmt.Sprintf("[%q]", r),
				})
			}
		}
	}
	it, err = sd.Store.UpdateItem(id, store.MaintenanceItem{
		ReplacementReleaseGUID: &guid, ReplacementSize: size,
	})
	if err != nil {
		return nil, err
	}
	if it.Status == store.MaintResearching {
		it, err = sd.Store.Transition(id, store.MaintCandidate, "selected: "+title)
		if err != nil {
			return nil, err
		}
	}
	_ = sd.Store.LogAction("safe_replace_select", it.Service, it.Title,
		fmt.Sprintf(`{"guid":%q}`, guid), "selected: "+title)
	return toolJSON(map[string]any{"item": it,
		"next": "safe_replace step=add_torrent with the magnet/URL"}), nil
}

func (sd *safeDeps) stepAddTorrent(ctx context.Context, args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	if it.ReplacementTorrentHash != nil && *it.ReplacementTorrentHash != "" {
		return toolJSON(map[string]any{"item": it,
			"note": "torrent already added (idempotent, no duplicate)"}), nil
	}
	if it.Status != store.MaintCandidate {
		return nil, fmt.Errorf("add_torrent needs status candidate_found, item is %s", it.Status)
	}
	url := argString(args, "url", "")
	hash := strings.ToLower(argString(args, "torrent_hash", ""))
	if url == "" && hash == "" {
		return nil, fmt.Errorf("url or torrent_hash is required")
	}
	if guid := strOr(it.ReplacementReleaseGUID, ""); guid != "" && sd.Store.IsBlocked(guid) {
		return nil, fmt.Errorf("release is blocklisted; refusing to add")
	}
	if url != "" && sd.Store.IsBlocked(url) {
		return nil, fmt.Errorf("url is blocklisted; refusing to add")
	}
	// Pre-add safety gate on the known file list.
	if files := argStrings(args, "files"); len(files) > 0 {
		if bad := maint.ScanFilenames(files); len(bad) > 0 {
			_ = sd.Store.BlockRelease(strOr(it.ReplacementReleaseGUID, url),
				"dangerous files: "+strings.Join(bad, ", "), "auto")
			_, _ = sd.Store.Transition(id, store.MaintBlocked,
				"torrent content unsafe: "+strings.Join(bad, ", ")+"; original untouched")
			return nil, fmt.Errorf("torrent rejected (dangerous files: %s); release blocklisted, original untouched",
				strings.Join(bad, ", "))
		}
	}
	if hash == "" {
		if m := magnetHashRe.FindStringSubmatch(url); m != nil {
			hash = strings.ToLower(m[1])
		}
	}
	if url != "" {
		if sd.qb == nil {
			return nil, fmt.Errorf("qbittorrent is not configured")
		}
		// Re-adding an existing magnet is a server-side no-op, so retries
		// after a restart do not create duplicate torrents.
		if err := sd.qb.AddTorrent(ctx, url, argString(args, "save_path", "")); err != nil {
			return nil, fmt.Errorf("adding torrent: %w", err)
		}
	}
	if hash != "" {
		it, err = sd.Store.UpdateItem(id, store.MaintenanceItem{ReplacementTorrentHash: &hash})
		if err != nil {
			return nil, err
		}
	}
	it, err = sd.Store.Transition(id, store.MaintDownloading, "torrent added")
	if err != nil {
		return nil, err
	}
	_ = sd.Store.LogAction("safe_replace_add", it.Service, it.Title,
		fmt.Sprintf(`{"hash":%q}`, strOr(it.ReplacementTorrentHash, "")),
		"downloading; original intact")
	out := map[string]any{"item": it,
		"next": "wait for completion, then safe_replace step=torrent_check"}
	if hash == "" {
		out["warning"] = "hash unknown: report it via maintenance_update replacement_torrent_hash (see qbit_list_torrents) before torrent_check"
	}
	return toolJSON(out), nil
}

func (sd *safeDeps) stepTorrentCheck(ctx context.Context, args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	hash := strOr(it.ReplacementTorrentHash, "")
	if h := strings.ToLower(argString(args, "torrent_hash", "")); h != "" {
		hash = h
	}
	if hash == "" {
		return nil, fmt.Errorf("no replacement torrent hash recorded")
	}
	if sd.qb == nil {
		return nil, fmt.Errorf("qbittorrent is not configured")
	}
	files, err := sd.qb.ListFiles(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("listing torrent files: %w", err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if bad := maint.ScanFilenames(names); len(bad) > 0 {
		_ = sd.Store.BlockRelease(hash, "dangerous files: "+strings.Join(bad, ", "), "auto")
		if guid := strOr(it.ReplacementReleaseGUID, ""); guid != "" {
			_ = sd.Store.BlockRelease(guid, "dangerous files: "+strings.Join(bad, ", "), "auto")
		}
		_, _ = sd.Store.Transition(id, store.MaintBlocked,
			"malicious torrent ("+strings.Join(bad, ", ")+"); original intact, remove the torrent")
		_ = sd.Store.LogAction("safe_replace_blocked", it.Service, it.Title,
			fmt.Sprintf(`{"hash":%q}`, hash), "dangerous files; original intact")
		return nil, fmt.Errorf("torrent REJECTED: dangerous files (%s). Release blocklisted, job blocked, original untouched",
			strings.Join(bad, ", "))
	}
	// Completion gate: 100% alone authorizes nothing beyond downloaded.
	progress := -1.0
	for _, t := range mustListTorrents(ctx, sd) {
		if strings.EqualFold(t.Hash, hash) {
			progress = t.Progress
		}
	}
	// A hash the client does not know is neither complete nor downloading:
	// it was removed, never added, or mistyped. Advancing would bless a
	// phantom replacement.
	if progress < 0 {
		return nil, fmt.Errorf("torrent %s not found in qbittorrent (removed, never added, or wrong hash); job stays downloading", hash)
	}
	if progress < 1 {
		return toolJSON(map[string]any{"item": it,
			"progress": progress, "note": "still downloading; content names are safe so far"}), nil
	}
	if it.Status == store.MaintDownloading {
		it, err = sd.Store.Transition(id, store.MaintDownloaded, "download complete, files safe")
		if err != nil {
			return nil, err
		}
	}
	_ = sd.Store.LogAction("safe_replace_downloaded", it.Service, it.Title,
		fmt.Sprintf(`{"hash":%q}`, hash), "files safe; awaiting verification (original intact)")
	return toolJSON(map[string]any{"item": it, "files": len(files),
		"next": "inspect the real files, then safe_replace step=verify"}), nil
}

func (sd *safeDeps) stepVerify(ctx context.Context, args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	if it.Status == store.MaintVerifying || it.Status == store.MaintImporting || it.Status == store.MaintReplacing {
		return toolJSON(map[string]any{"item": it, "note": "already verified (idempotent)"}), nil
	}
	if it.Status != store.MaintDownloaded {
		return nil, fmt.Errorf("verify needs status downloaded, item is %s", it.Status)
	}
	path := argString(args, "path", "")
	audio := argStrings(args, "audio_langs")
	subs := argStrings(args, "sub_langs")
	complete := argBool(args, "complete", false)
	var size *int64
	if argOk(args, "size") {
		n := argInt64(args, "size", 0)
		size = &n
	}
	// Prefer a real ffprobe inspection when the files are reachable.
	probed := false
	if path != "" {
		if real, rerr := sd.Fs.ResolveRead(path); rerr == nil {
			if rep, ierr := mediainspect.InspectFile(ctx, sd.Ffprobe, real); ierr == nil {
				probed = true
				audio = rep.AudioLanguages
				subs = append(subs, rep.SubtitleLanguages...)
				subs = append(subs, langNames(rep.ExternalSubtitles)...)
				if rep.SizeBytes > 0 && size == nil {
					size = &rep.SizeBytes
				}
				if len(rep.DangerousFiles) > 0 {
					_, _ = sd.Store.Transition(id, store.MaintBlocked,
						"replacement contains dangerous files; original intact")
					return nil, fmt.Errorf("verification FAILED: dangerous files in replacement; original intact, job blocked")
				}
			}
		}
	}
	if !complete {
		_, _ = sd.Store.Transition(id, store.MaintBlocked, "verification failed: incomplete replacement; original intact")
		_ = sd.Store.LogAction("safe_replace_verify_fail", it.Service, it.Title,
			fmt.Sprintf(`{"id":%d}`, id), "incomplete; original intact")
		return nil, fmt.Errorf("verification FAILED: replacement is incomplete; original intact, job blocked")
	}
	if it.RequiresSubtitles && maint.NeedsAccessibleSubtitles(audio, subs) {
		_, _ = sd.Store.Transition(id, store.MaintBlocked,
			"verification failed: replacement lacks accessible audio/subs; original intact")
		_ = sd.Store.LogAction("safe_replace_verify_fail", it.Service, it.Title,
			fmt.Sprintf(`{"audio":%q,"subs":%q}`, strings.Join(audio, ","), strings.Join(subs, ",")),
			"missing accessible language; original intact")
		return nil, fmt.Errorf("verification FAILED: replacement has no English/Spanish audio or subtitles; original intact, job blocked")
	}
	if _, err := sd.Store.RecordCheck(store.MediaCheck{
		MediaType: it.MediaType, MediaID: it.MediaID, Path: path,
		AudioLanguages: audio, SubtitleLanguages: subs, SizeBytes: size,
	}); err != nil {
		return nil, err
	}
	if size != nil {
		_, _ = sd.Store.UpdateItem(id, store.MaintenanceItem{ReplacementSize: size})
	}
	it, err = sd.Store.Transition(id, store.MaintVerifying, "replacement verified")
	if err != nil {
		return nil, err
	}
	_ = sd.Store.LogAction("safe_replace_verified", it.Service, it.Title,
		fmt.Sprintf(`{"probed":%v}`, probed), "verified; original still intact")
	return toolJSON(map[string]any{"item": it,
		"next": "import via Sonarr/Radarr, then safe_replace step=import_confirm"}), nil
}

func (sd *safeDeps) stepImportConfirm(ctx context.Context, args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	if it.Status == store.MaintReplacing {
		return toolJSON(map[string]any{"item": it, "note": "import already confirmed (idempotent)"}), nil
	}
	if it.Status == store.MaintImporting {
		// Crash recovery: the import was confirmed and the first transition
		// ran, but the process died before authorizing deletion. Reporting
		// success here would wedge the job in importing forever (no other
		// tool advances it), so complete the second transition now.
		it, err = sd.Store.Transition(id, store.MaintReplacing,
			"replacement live; original removal authorized (recovered after restart)")
		if err != nil {
			return nil, err
		}
		_ = sd.Store.LogAction("safe_replace_imported", it.Service, it.Title,
			fmt.Sprintf(`{"recovered":true}`), "import confirmed; delete authorized")
		return toolJSON(map[string]any{"item": it,
			"next": "only now may the original be removed: safe_replace step=delete_original confirm=true"}), nil
	}
	if it.Status != store.MaintVerifying {
		return nil, fmt.Errorf("import_confirm needs status verifying, item is %s", it.Status)
	}
	newFileID := argString(args, "new_file_id", "")
	if newFileID == "" {
		return nil, fmt.Errorf("new_file_id is required: prove Sonarr/Radarr associated the new files")
	}
	// Live confirmation when the service is reachable.
	confirmed := false
	if sd.registry != nil {
		if endpoint, ok := importCheckEndpoint(it.Service); ok {
			if svc, gerr := sd.registry.Get(it.Service); gerr == nil {
				if body, code, derr := svc.DoRequest(ctx, "GET", endpoint+"/"+newFileID, nil, nil); derr == nil && code >= 200 && code <= 299 && len(body) > 2 {
					confirmed = true
				}
			}
		}
	}
	if !confirmed {
		return nil, fmt.Errorf("could not confirm file %s in %s; refusing to advance (original intact)", newFileID, it.Service)
	}
	it, err = sd.Store.Transition(id, store.MaintImporting, "import confirmed, file "+newFileID)
	if err != nil {
		return nil, err
	}
	it, err = sd.Store.Transition(id, store.MaintReplacing, "replacement live; original removal authorized")
	if err != nil {
		return nil, err
	}
	_ = sd.Store.LogAction("safe_replace_imported", it.Service, it.Title,
		fmt.Sprintf(`{"new_file_id":%q}`, newFileID), "import confirmed; delete authorized")
	return toolJSON(map[string]any{"item": it,
		"next": "only now may the original be removed: safe_replace step=delete_original confirm=true"}), nil
}

func (sd *safeDeps) stepDeleteOriginal(ctx context.Context, args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	if it.Status != store.MaintReplacing {
		return nil, fmt.Errorf("delete_original needs status replacing (verified + imported), item is %s; the original stays", it.Status)
	}
	if !argBool(args, "confirm", false) {
		return nil, fmt.Errorf("delete_original needs confirm=true; the original stays")
	}
	if !sd.allowDestructive {
		return nil, fmt.Errorf("deletion is disabled. Set allow_destructive: true in config.yaml to enable; the original stays")
	}
	via := strings.ToLower(argString(args, "via", "arr"))
	switch via {
	case "filesystem":
		path := argString(args, "path", "")
		if path == "" {
			return nil, fmt.Errorf("path is required for via=filesystem")
		}
		if err := sd.Fs.Delete(path); err != nil {
			return nil, fmt.Errorf("safe delete refused: %w", err)
		}
		_ = sd.Store.LogAction("safe_replace_delete", it.Service, it.Title,
			fmt.Sprintf(`{"via":"filesystem","path":%q}`, path), "original removed after verified import")
		return toolJSON(map[string]any{"item": it,
			"next": "safe_replace step=finish"}), nil
	case "arr":
		fileID := strOr(it.CurrentFileID, "")
		if fileID == "" {
			return nil, fmt.Errorf("no current_file_id snapshot; record it via maintenance_update before deleting through %s", it.Service)
		}
		endpoint, ok := importCheckEndpoint(it.Service)
		if !ok {
			return nil, fmt.Errorf("service %q does not support arr-side file deletion", it.Service)
		}
		svc, err := sd.registry.Get(it.Service)
		if err != nil {
			return nil, err
		}
		_, code, err := svc.DoRequest(ctx, "DELETE", endpoint+"/"+fileID, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("deleting original via %s: %w", it.Service, err)
		}
		if code < 200 || code > 299 {
			return nil, fmt.Errorf("deleting original via %s: HTTP %d", it.Service, code)
		}
		_ = sd.Store.LogAction("safe_replace_delete", it.Service, it.Title,
			fmt.Sprintf(`{"via":"arr","file_id":%q}`, fileID), "original removed after verified import")
		return toolJSON(map[string]any{"item": it,
			"next": "safe_replace step=finish"}), nil
	default:
		return nil, fmt.Errorf("unknown via %q (use filesystem or arr)", via)
	}
}

func (sd *safeDeps) stepFinish(args map[string]any, id int64) (*mcp.CallToolResult, error) {
	it, err := sd.Store.GetItem(id)
	if err != nil {
		return nil, err
	}
	if it.Status == store.MaintDone {
		return toolJSON(map[string]any{"item": it, "note": "already done (idempotent)"}), nil
	}
	notes := argString(args, "notes", "")
	if it.CurrentSize != nil && it.ReplacementSize != nil && *it.CurrentSize > *it.ReplacementSize {
		saved := *it.CurrentSize - *it.ReplacementSize
		notes += fmt.Sprintf(" saved=%d bytes (%.1fGB -> %.1fGB)",
			saved, float64(*it.CurrentSize)/1e9, float64(*it.ReplacementSize)/1e9)
	}
	it, err = sd.Store.ResolveItem(id, store.MaintDone, notes)
	if err != nil {
		return nil, err
	}
	_ = sd.Store.LogAction("safe_replace_done", it.Service, it.Title,
		fmt.Sprintf(`{"id":%d}`, id), notes)
	return toolJSON(map[string]any{"item": it}), nil
}

func importCheckEndpoint(service string) (string, bool) {
	switch service {
	case "sonarr":
		return "/episodefile", true
	case "radarr":
		return "/moviefile", true
	default:
		return "", false
	}
}

func strOr(p *string, fallback string) string {
	if p != nil {
		return *p
	}
	return fallback
}

func mustListTorrents(ctx context.Context, sd *safeDeps) []qbit.TorrentInfo {
	if sd.qb == nil {
		return nil
	}
	list, err := sd.qb.ListTorrents(ctx)
	if err != nil {
		return nil
	}
	return list
}

// langNames collects subtitle language hints from sidecar filenames
// ("Show.eng.srt" -> eng) so external subs count toward verification.
func langNames(paths []string) []string {
	var out []string
	for _, p := range paths {
		parts := strings.Split(strings.ToLower(p), ".")
		if len(parts) >= 3 {
			out = append(out, maint.NormalizeLang(parts[len(parts)-2]))
		}
	}
	return out
}
