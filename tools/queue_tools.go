package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sort"
	"strings"

	"github.com/jakenesler/navigatorr/queue"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxQueueListItems caps how many requests queue_list renders at once.
const maxQueueListItems = 50

// summarize renders the per-status counts in a stable order.
func summarize(counts map[string]int) string {
	if len(counts) == 0 {
		return "The queue is empty."
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return "Queue holds " + strings.Join(parts, ", ") + "."
}

func registerQueueTools(s *server.MCPServer, store *queue.Store) {
	// queue_list
	s.AddTool(
		mcp.NewTool("queue_list",
			mcp.WithDescription("List media requests submitted to navigatorr's queue. Defaults to pending requests waiting to be worked."),
			mcp.WithString("status", mcp.Description("Filter by status: pending, claimed, done, failed. Omit for pending; use \"all\" for everything.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			status := mcp.ParseString(req, "status", queue.StatusPending)
			if status == "all" {
				status = ""
			}
			// Reject an unknown status instead of filtering to nothing, which
			// reads to the model as "the queue is empty".
			if status != "" && !queue.ValidStatus(status) {
				return mcp.NewToolResultError(fmt.Sprintf(
					"status must be one of %s, or \"all\"", strings.Join(queue.Statuses, ", "))), nil
			}

			items := store.List(status)
			counts := store.Counts()
			if len(items) == 0 {
				// Say what else is in the queue, so a backlog parked in
				// claimed does not look like an empty queue.
				return mcp.NewToolResultText("No matching requests. " + summarize(counts)), nil
			}

			// Cap the payload. queue_list output goes straight into the
			// model's context, and a long-lived queue would otherwise fill it.
			note := ""
			if len(items) > maxQueueListItems {
				note = fmt.Sprintf("\n\nShowing the %d oldest of %d matching requests. %s",
					maxQueueListItems, len(items), summarize(counts))
				items = items[:maxQueueListItems]
			}
			data, _ := json.MarshalIndent(items, "", "  ")
			return mcp.NewToolResultText(string(data) + note), nil
		},
	)

	// queue_claim
	s.AddTool(
		mcp.NewTool("queue_claim",
			mcp.WithDescription("Claim a pending request before working it, so concurrent agents do not duplicate the work."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Request ID, e.g. r3")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := mcp.ParseString(req, "id", "")
			if id == "" {
				return mcp.NewToolResultError("id is required"), nil
			}
			it, err := store.Claim(id)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("Claimed %s: %s", it.ID, it.Text)), nil
		},
	)

	// queue_resolve
	s.AddTool(
		mcp.NewTool("queue_resolve",
			mcp.WithDescription("Close a request as done or failed. Always include a note describing what was actually added, or why it could not be."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Request ID, e.g. r3")),
			mcp.WithString("status", mcp.Required(), mcp.Description("done or failed")),
			mcp.WithString("note", mcp.Description("What happened, e.g. \"added Boston Legal (tvdb 74058), 5 seasons monitored\"")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := mcp.ParseString(req, "id", "")
			status := mcp.ParseString(req, "status", "")
			note := mcp.ParseString(req, "note", "")
			if id == "" {
				return mcp.NewToolResultError("id is required"), nil
			}
			it, err := store.Resolve(id, status, note)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("%s -> %s", it.ID, it.Status)), nil
		},
	)

	// queue_release
	s.AddTool(
		mcp.NewTool("queue_release",
			mcp.WithDescription("Return a claimed request to pending without resolving it, so another pass can pick it up."),
			mcp.WithString("id", mcp.Required(), mcp.Description("Request ID, e.g. r3")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			id := mcp.ParseString(req, "id", "")
			if id == "" {
				return mcp.NewToolResultError("id is required"), nil
			}
			it, err := store.Release(id)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(fmt.Sprintf("%s -> pending", it.ID)), nil
		},
	)
}
