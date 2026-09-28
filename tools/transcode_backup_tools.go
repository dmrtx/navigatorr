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
		mcp.WithDescription("List retained Navigatorr promotion backups and their disk usage (default mode=list). Follow next_offset even on an empty page; page_bytes is only this page. Inventory uses action ownership, not a filesystem sweep. mode=clean with action_id retries ONLY final verification and cleanup of a failed promotion: verifies the adopted library file and hashes, then removes its owned recovery/temporary files and completes the action. Earlier failures and active jobs remain protected. cleanup_available is not proof of safe deletion; verification can still fail and retain recovery. Does not encode, import, rename or rescan. After timeout, check action_status before retrying."),
		mcp.WithString("mode", mcp.Enum("list", "clean"), mcp.Description("list (default) or clean one listed action")),
		mcp.WithString("action_id", mcp.Description("Required for clean; use an action_id returned by list")),
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
		case "clean":
			id := strings.TrimSpace(argString(args, "action_id", ""))
			if id == "" {
				return toolErr("action_id from transcode_backups list is required for clean"), nil
			}
			result, err := engine.CleanTranscodeBackup(ctx, id)
			if err != nil {
				return toolErr("transcode_backups: %v", err), nil
			}
			return toolJSON(toCompactSummary(result)), nil
		default:
			return toolErr("mode must be list or clean"), nil
		}
	})
}
