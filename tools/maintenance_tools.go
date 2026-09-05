package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jakenesler/navigatorr/store"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerMaintenanceTools(s *server.MCPServer, d *Deps) {
	// maintenance_add — idempotent: an active duplicate returns the survivor.
	s.AddTool(
		mcp.NewTool("maintenance_add",
			mcp.WithDescription("Open a library-maintenance job (oversized, missing_accessible_language, dangerous_media, possible_media_mismatch). Idempotent: re-adding the same active media+issue returns the existing job instead of duplicating it."),
			mcp.WithString("media_type", mcp.Required(), mcp.Description("series, anime or movie")),
			mcp.WithString("service", mcp.Required(), mcp.Description("sonarr or radarr")),
			mcp.WithString("media_id", mcp.Description("Service-side id (e.g. sonarr series id)")),
			mcp.WithString("title", mcp.Required(), mcp.Description("Human title, e.g. Fate/strange Fake")),
			mcp.WithString("issue_type", mcp.Required(), mcp.Description("oversized, missing_accessible_language, dangerous_media, possible_media_mismatch")),
			mcp.WithString("priority", mcp.Description("Higher runs first (default 0)")),
			mcp.WithString("current_size", mcp.Description("Current size on disk in bytes")),
			mcp.WithString("requires_subtitles", mcp.Description("true when the replacement must carry accessible subtitles")),
			mcp.WithString("notes", mcp.Description("Free-form notes")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			it := store.MaintenanceItem{
				MediaType: argString(args, "media_type", ""),
				Service:   argString(args, "service", ""),
				MediaID:   argString(args, "media_id", ""),
				Title:     argString(args, "title", ""),
				IssueType: argString(args, "issue_type", ""),
				Priority:  int(argInt64(args, "priority", 0)),
				Notes:     argString(args, "notes", ""),
			}
			if argOk(args, "current_size") {
				n := argInt64(args, "current_size", 0)
				it.CurrentSize = &n
			}
			it.RequiresSubtitles = argBool(args, "requires_subtitles", false)
			created, err := d.Store.AddItem(it)
			if err != nil {
				return toolErr("%v", err), nil
			}
			_ = d.Store.LogAction("maintenance_add", created.Service, created.Title,
				fmt.Sprintf(`{"issue_type":%q}`, created.IssueType),
				fmt.Sprintf("item %d status=%s", created.ID, created.Status))
			return toolJSON(created), nil
		},
	)

	// maintenance_list
	s.AddTool(
		mcp.NewTool("maintenance_list",
			mcp.WithDescription("List maintenance jobs with optional status, service, issue_type and minimum-priority filters."),
			mcp.WithString("status", mcp.Description("pending, researching, candidate_found, downloading, downloaded, verifying, importing, replacing, blocked, done, failed")),
			mcp.WithString("service", mcp.Description("Filter by service")),
			mcp.WithString("issue_type", mcp.Description("Filter by issue type")),
			mcp.WithString("priority", mcp.Description("Minimum priority")),
			mcp.WithString("limit", mcp.Description("Max items (default 50, max 100)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			f := store.ItemFilter{
				Status:    argString(args, "status", ""),
				Service:   argString(args, "service", ""),
				IssueType: argString(args, "issue_type", ""),
				Limit:     int(argInt64(args, "limit", 50)),
			}
			if argOk(args, "priority") {
				n := int(argInt64(args, "priority", 0))
				f.Priority = &n
			}
			items, err := d.Store.ListItems(f)
			if err != nil {
				return toolErr("%v", err), nil
			}
			if len(items) == 0 {
				return mcp.NewToolResultText("No matching maintenance jobs."), nil
			}
			return toolJSON(items), nil
		},
	)

	// maintenance_next
	s.AddTool(
		mcp.NewTool("maintenance_next",
			mcp.WithDescription("Return the next actionable job: highest priority first, oldest update on ties, skipping leased (claimed) and terminal jobs."),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			it, err := d.Store.NextItem()
			if err != nil {
				return mcp.NewToolResultText("No actionable maintenance jobs."), nil
			}
			return toolJSON(it), nil
		},
	)

	// maintenance_get
	s.AddTool(
		mcp.NewTool("maintenance_get",
			mcp.WithDescription("Get one maintenance job plus its recent release decisions."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Numeric job id")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, errRes := parseID(req.GetArguments())
			if errRes != nil {
				return errRes, nil
			}
			it, err := d.Store.GetItem(id)
			if err != nil {
				return toolErr("%v", err), nil
			}
			decisions, _ := d.Store.ListDecisions(id, 10)
			return toolJSON(map[string]any{"item": it, "decisions": decisions}), nil
		},
	)

	// maintenance_update — patch replacement/notes fields of an open job.
	s.AddTool(
		mcp.NewTool("maintenance_update",
			mcp.WithDescription("Patch an open job: replacement guid/hash/size, current file, priority, notes. Never changes status (use safe_replace steps or maintenance_resolve for that)."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Numeric job id")),
			mcp.WithString("replacement_release_guid", mcp.Description("Selected release guid")),
			mcp.WithString("replacement_torrent_hash", mcp.Description("qBittorrent hash of the replacement")),
			mcp.WithString("replacement_size", mcp.Description("Replacement size in bytes")),
			mcp.WithString("current_file_id", mcp.Description("Current file snapshot id")),
			mcp.WithString("current_size", mcp.Description("Current size in bytes")),
			mcp.WithString("priority", mcp.Description("New priority")),
			mcp.WithString("requires_subtitles", mcp.Description("true/false")),
			mcp.WithString("notes", mcp.Description("Notes (replaces existing)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id, errRes := parseID(args)
			if errRes != nil {
				return errRes, nil
			}
			var patch store.MaintenanceItem
			if v := argString(args, "replacement_release_guid", ""); argOk(args, "replacement_release_guid") {
				patch.ReplacementReleaseGUID = &v
			}
			if v := argString(args, "replacement_torrent_hash", ""); argOk(args, "replacement_torrent_hash") {
				patch.ReplacementTorrentHash = &v
			}
			if argOk(args, "replacement_size") {
				n := argInt64(args, "replacement_size", 0)
				patch.ReplacementSize = &n
			}
			if v := argString(args, "current_file_id", ""); argOk(args, "current_file_id") {
				patch.CurrentFileID = &v
			}
			if argOk(args, "current_size") {
				n := argInt64(args, "current_size", 0)
				patch.CurrentSize = &n
			}
			if argOk(args, "priority") {
				patch.Priority = int(argInt64(args, "priority", 0))
			}
			if argOk(args, "requires_subtitles") {
				patch.RequiresSubtitles = argBool(args, "requires_subtitles", false)
			}
			patch.Notes = argString(args, "notes", "")
			it, err := d.Store.UpdateItem(id, patch)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(it), nil
		},
	)

	// maintenance_claim — temporary lease for parallel agents.
	s.AddTool(
		mcp.NewTool("maintenance_claim",
			mcp.WithDescription("Take a temporary lease on a job so parallel agents do not double-work it. Fails when another agent holds a live lease."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Numeric job id")),
			mcp.WithString("owner", mcp.Required(), mcp.Description("Agent name holding the lease")),
			mcp.WithString("lease_seconds", mcp.Description("Lease duration (default 900)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id, errRes := parseID(args)
			if errRes != nil {
				return errRes, nil
			}
			owner := argString(args, "owner", "")
			if owner == "" {
				return toolErr("owner is required"), nil
			}
			lease := time.Duration(argInt64(args, "lease_seconds", 900)) * time.Second
			it, err := d.Store.ClaimItem(id, owner, lease)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(it), nil
		},
	)

	// maintenance_release
	s.AddTool(
		mcp.NewTool("maintenance_release",
			mcp.WithDescription("Give up a lease without changing the job status, so another pass can pick it up."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Numeric job id")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id, errRes := parseID(req.GetArguments())
			if errRes != nil {
				return errRes, nil
			}
			it, err := d.Store.ReleaseItem(id)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(it), nil
		},
	)

	// maintenance_resolve — done only from replacing (safe workflow gate).
	s.AddTool(
		mcp.NewTool("maintenance_resolve",
			mcp.WithDescription("Close a job as done or failed with a note. done is only accepted from the replacing state, i.e. after the safe-replacement workflow verified and imported the replacement."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Numeric job id")),
			mcp.WithString("status", mcp.Required(), mcp.Description("done or failed")),
			mcp.WithString("note", mcp.Description("What happened, e.g. space saved and verification outcome")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			id, errRes := parseID(args)
			if errRes != nil {
				return errRes, nil
			}
			status := argString(args, "status", "")
			note := argString(args, "note", "")
			it, err := d.Store.ResolveItem(id, status, note)
			if err != nil {
				return toolErr("%v", err), nil
			}
			_ = d.Store.LogAction("maintenance_resolve", it.Service, it.Title,
				fmt.Sprintf(`{"id":%d,"status":%q}`, id, status), note)
			return toolJSON(it), nil
		},
	)

	// decision_record
	s.AddTool(
		mcp.NewTool("decision_record",
			mcp.WithDescription("Record why a release was selected, rejected or kept as alternate for a job. Append-only history for later why-did-we-pick-this questions."),
			mcp.WithString("maintenance_item_id", mcp.Required(), mcp.Description("Numeric job id")),
			mcp.WithString("title", mcp.Required(), mcp.Description("Release title")),
			mcp.WithString("decision", mcp.Required(), mcp.Description("selected, rejected or alternate")),
			mcp.WithString("release_guid", mcp.Description("Release guid")),
			mcp.WithString("release_group", mcp.Description("Release group, e.g. Judas")),
			mcp.WithString("size", mcp.Description("Release size in bytes")),
			mcp.WithString("seeders", mcp.Description("Seeders observed at decision time (a fact, not a preference)")),
			mcp.WithString("score", mcp.Description("Deterministic score from rank_releases")),
			mcp.WithString("reasons", mcp.Description("Reasons: JSON array or comma-separated, e.g. multi_subs,hevc_10bit,healthy_seed_count")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			itemID := argInt64(args, "maintenance_item_id", 0)
			if itemID <= 0 {
				return toolErr("maintenance_item_id is required"), nil
			}
			title := argString(args, "title", "")
			decision := argString(args, "decision", "")
			if title == "" || decision == "" {
				return toolErr("title and decision are required"), nil
			}
			rec := store.ReleaseDecision{
				MaintenanceItemID: itemID,
				ReleaseGUID:       argString(args, "release_guid", ""),
				Title:             title,
				ReleaseGroup:      argString(args, "release_group", ""),
				Decision:          decision,
			}
			if argOk(args, "size") {
				n := argInt64(args, "size", 0)
				rec.Size = &n
			}
			if argOk(args, "seeders") {
				n := int(argInt64(args, "seeders", 0))
				rec.SeedersAtDecision = &n
			}
			if argOk(args, "score") {
				f := argFloat(args, "score", 0)
				rec.Score = &f
			}
			reasons := argStrings(args, "reasons")
			if len(reasons) > 0 {
				b, _ := json.Marshal(reasons)
				rec.ReasonsJSON = string(b)
			}
			saved, err := d.Store.RecordDecision(rec)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(saved), nil
		},
	)

	// decision_list
	s.AddTool(
		mcp.NewTool("decision_list",
			mcp.WithDescription("List the recorded release decisions for a job, newest first. Answers why-did-we-pick-this."),
			mcp.WithString("maintenance_item_id", mcp.Required(), mcp.Description("Numeric job id")),
			mcp.WithString("limit", mcp.Description("Max decisions (default 20)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			itemID := argInt64(args, "maintenance_item_id", 0)
			if itemID <= 0 {
				return toolErr("maintenance_item_id is required"), nil
			}
			list, err := d.Store.ListDecisions(itemID, int(argInt64(args, "limit", 20)))
			if err != nil {
				return toolErr("%v", err), nil
			}
			if len(list) == 0 {
				return mcp.NewToolResultText("No decisions recorded for this job."), nil
			}
			return toolJSON(list), nil
		},
	)
}
