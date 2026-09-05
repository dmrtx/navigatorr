package tools

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxMemoryList caps memory_list output; values ride along in full, so the
// cap protects the model's context the same way queue_list's cap does.
const maxMemoryList = 50

func registerMemoryTools(s *server.MCPServer, d *Deps) {
	// memory_set
	s.AddTool(
		mcp.NewTool("memory_set",
			mcp.WithDescription("Store a persistent preference or fact. Scopes: global, anime, movies, project:<name>, media:<service>:<id>. Values may be any JSON. ttl_seconds makes it a temporary fact that expires instead of becoming permanent."),
			mcp.WithString("scope", mcp.Description("Preference scope, e.g. anime, movies, global")),
			mcp.WithString("key", mcp.Required(), mcp.Description("Preference key, e.g. preferred_release_groups")),
			mcp.WithString("value", mcp.Description("JSON value. May also be passed as a native array/object/boolean by capable clients.")),
			mcp.WithString("source", mcp.Description("user, default or fact (default: user)")),
			mcp.WithString("ttl_seconds", mcp.Description("Optional TTL for temporary facts such as seeder counts. Omit for durable preferences.")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			scope := argString(args, "scope", "global")
			key := argString(args, "key", "")
			source := argString(args, "source", "user")
			if key == "" {
				return toolErr("key is required"), nil
			}
			value, err := argJSON(args, "value")
			if err != nil {
				return toolErr("%v", err), nil
			}
			var ttl time.Duration
			if secs := argInt64(args, "ttl_seconds", 0); secs > 0 {
				ttl = time.Duration(secs) * time.Second
			}
			p, err := d.Store.SetPreference(scope, key, value, source, ttl)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(p), nil
		},
	)

	// memory_get
	s.AddTool(
		mcp.NewTool("memory_get",
			mcp.WithDescription("Get persistent preferences. With key, returns one preference; without key, returns every active preference in the scope. Expired facts are never returned."),
			mcp.WithString("scope", mcp.Description("Preference scope (default: global)")),
			mcp.WithString("key", mcp.Description("Preference key (omit to list the whole scope)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			scope := argString(args, "scope", "global")
			key := argString(args, "key", "")
			if key == "" {
				prefs, err := d.Store.ListPreferences(scope)
				if err != nil {
					return toolErr("%v", err), nil
				}
				if len(prefs) == 0 {
					return mcp.NewToolResultText("No preferences stored in scope " + scope + "."), nil
				}
				if len(prefs) > maxMemoryList {
					prefs = prefs[:maxMemoryList]
				}
				return toolJSON(prefs), nil
			}
			p, err := d.Store.GetPreference(scope, key)
			if err != nil {
				return toolErr("%v", err), nil
			}
			return toolJSON(p), nil
		},
	)

	// memory_list
	s.AddTool(
		mcp.NewTool("memory_list",
			mcp.WithDescription("List persistent preferences, optionally filtered by scope. Expired facts are excluded."),
			mcp.WithString("scope", mcp.Description("Filter by scope (omit for all)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			prefs, err := d.Store.ListPreferences(argString(args, "scope", ""))
			if err != nil {
				return toolErr("%v", err), nil
			}
			if len(prefs) == 0 {
				return mcp.NewToolResultText("No preferences stored."), nil
			}
			if len(prefs) > maxMemoryList {
				prefs = prefs[:maxMemoryList]
			}
			return toolJSON(prefs), nil
		},
	)

	// memory_search
	s.AddTool(
		mcp.NewTool("memory_search",
			mcp.WithDescription("Search preferences by scope, key or value text. Expired facts are excluded."),
			mcp.WithString("query", mcp.Required(), mcp.Description("Search text")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			q := argString(args, "query", "")
			if q == "" {
				return toolErr("query is required"), nil
			}
			prefs, err := d.Store.SearchPreferences(q)
			if err != nil {
				return toolErr("%v", err), nil
			}
			if len(prefs) == 0 {
				return mcp.NewToolResultText("No matching preferences."), nil
			}
			return toolJSON(prefs), nil
		},
	)

	// memory_delete
	s.AddTool(
		mcp.NewTool("memory_delete",
			mcp.WithDescription("Delete a stored preference or fact."),
			mcp.WithString("scope", mcp.Description("Preference scope (default: global)")),
			mcp.WithString("key", mcp.Required(), mcp.Description("Preference key")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			scope := argString(args, "scope", "global")
			key := argString(args, "key", "")
			if key == "" {
				return toolErr("key is required"), nil
			}
			if err := d.Store.DeletePreference(scope, key); err != nil {
				return toolErr("%v", err), nil
			}
			return mcp.NewToolResultText("Deleted " + scope + "/" + key), nil
		},
	)
}
