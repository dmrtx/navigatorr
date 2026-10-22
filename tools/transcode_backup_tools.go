package tools

import (
	"context"
	"strings"

	"github.com/jakenesler/navigatorr/action"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerTranscodeBackupTool(s *server.MCPServer, engine *action.Engine) {
	s.AddTool(mcp.NewTool("transcode_backups",
		mcp.WithDescription("List retained Navigatorr promotion backups and their disk usage (default mode=list). Follow next_offset even on an empty page; page_bytes is only this page. Inventory uses action ownership, not a filesystem sweep. mode=clean with action_id retries ONLY final verification and cleanup of a failed promotion: verifies the adopted library file and hashes, then removes its owned recovery/temporary files and completes the action. Earlier failures and active jobs remain protected. cleanup_available is not proof of safe deletion; verification can still fail and retain recovery. Does not encode, import, rename or rescan. After timeout, check action_status before retrying. mode=candidates lists, read-only, transcode candidate files left in .navigatorr-candidates by finished attempts that can no longer be promoted (rejected, cancelled, failed or replaced by a batch settings revision), with the reason, size and owning action; it deletes nothing. Its paging is by action, so a page can be empty and still carry next_offset. mode=discard_candidate with one listed action_id and confirm=true removes ONLY that attempt's own candidate and partial in .navigatorr-candidates, after re-checking that nothing can promote it; it needs allow_destructive, refuses an active action, a started replacement, a symlink or any file not named for the attempt's job, never touches the original or recovery files, and treats an already-missing file as removed. An item marked worker_check belongs to a worker job that might still write: discard_candidate first asks the worker and removes nothing unless the job is finished or unknown to it."),
		mcp.WithString("mode", mcp.Enum("list", "candidates", "discard_candidate", "clean", "discard_duplicate", "discard", "restore"), mcp.Description("list (default), candidates (read-only leftover candidate files), discard_candidate (remove one listed leftover candidate; needs confirm=true), clean final promotion checkpoint, or discard_duplicate: remove only owned recovery artifacts after the unchanged original and recovery copy match the recorded SHA-256. discard permanently removes only the owned recovery copy of a finished job WITHOUT verifying its replacement; requires explicit user intent. Job status/history is preserved; no conversion or library mutation is resumed. restore moves an original that a cancelled or failed same-name replacement left at its recovery path back to its library path: only while that path is empty, after checking the file against its recorded SHA-256 (a full read, unless its file identity is unchanged since this process verified it), with a no-replace rename, for same-name filesystem promotions only; nothing is deleted (restore_available in list).")),
		mcp.WithString("action_id", mcp.Description("Required for clean, discard_duplicate, discard, restore (an action_id returned by list) and discard_candidate (one returned by candidates)")),
		mcp.WithString("confirm", mcp.Description("discard_candidate: must be true; without it nothing is removed")),
		mcp.WithNumber("offset", mcp.Min(0), mcp.Description("List pagination: use returned next_offset; default 0")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.GetArguments()
		switch argString(args, "mode", "list") {
		case "list":
			page, err := engine.ListTranscodeBackups(ctx, int(argInt64(args, "offset", 0)))
			if err != nil {
				return toolErr("transcode_backups: %v", err), nil
			}
			return toolBoundedJSON(page, MaxActionResponseBytes, nil), nil
		case "candidates":
			page, err := engine.ListLeftoverCandidates(ctx, int(argInt64(args, "offset", 0)))
			if err != nil {
				return toolErr("transcode_backups: %v", err), nil
			}
			return toolBoundedJSON(page, MaxActionResponseBytes, nil), nil
		case "discard_candidate":
			id := strings.TrimSpace(argString(args, "action_id", ""))
			if id == "" {
				return toolErr("action_id from transcode_backups mode=candidates is required for discard_candidate"), nil
			}
			if !argBool(args, "confirm", false) {
				return toolErr("discard_candidate needs confirm=true; the candidate stays"), nil
			}
			result, err := engine.DiscardLeftoverCandidate(ctx, id)
			if err != nil {
				return toolErr("transcode_backups: %v", err), nil
			}
			// The compact summary has no cleanup record, and a superseded
			// completed attempt's message does not mention it.
			cleanup, _ := result.State["candidate_cleanup"].(map[string]any)
			return toolJSON(struct {
				ActionCompactSummary
				CandidateCleanup map[string]any `json:"candidate_cleanup,omitempty"`
			}{toCompactSummary(result), cleanup}), nil
		case "clean", "discard_duplicate", "discard", "restore":
			id := strings.TrimSpace(argString(args, "action_id", ""))
			if id == "" {
				return toolErr("action_id from transcode_backups list is required for clean"), nil
			}
			var result *action.ActionResult
			var err error
			if argString(args, "mode", "list") == "restore" {
				result, err = engine.RestoreTranscodeBackup(ctx, id)
			} else if argString(args, "mode", "list") == "discard" {
				result, err = engine.DiscardTranscodeBackup(ctx, id)
			} else if argString(args, "mode", "list") == "discard_duplicate" {
				result, err = engine.DiscardDuplicateTranscodeBackup(ctx, id)
			} else {
				result, err = engine.CleanTranscodeBackup(ctx, id)
			}
			if err != nil {
				return toolErr("transcode_backups: %v", err), nil
			}
			return toolJSON(toCompactSummary(result)), nil
		default:
			return toolErr("mode must be list, candidates, discard_candidate, clean, discard_duplicate, discard or restore"), nil
		}
	})
}
