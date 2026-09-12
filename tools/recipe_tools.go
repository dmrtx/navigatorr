package tools

import (
	"context"

	"github.com/jakenesler/navigatorr/config"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func registerRecipeTools(s *server.MCPServer, cfg *config.Config) {
	s.AddTool(mcp.NewTool("recipe_status", mcp.WithDescription("Show the active transcode recipe source, version, digest, last-known-good version, last check time, and sanitized update error.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		return toolJSON(mgr.Status()), nil
	})
	s.AddTool(mcp.NewTool("recipe_reload", mcp.WithDescription("Re-read the configured transcode recipe source, validate it strictly, cache it, and atomically activate it only if valid. Running jobs keep their existing plan.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		st, err := mgr.Reload(ctx)
		if err != nil {
			return toolJSON(map[string]any{"status": st, "activated": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "activated": true}), nil
	})
	s.AddTool(mcp.NewTool("recipe_update", mcp.WithDescription("Fetch the configured versioned recipe source now. Invalid schema, semantics, compatibility, or integrity leave the current last-known-good bundle active.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		st, err := mgr.Update(ctx)
		if err != nil {
			return toolJSON(map[string]any{"status": st, "activated": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "activated": true}), nil
	})
	s.AddTool(mcp.NewTool("recipe_rollback", mcp.WithDescription("Atomically restore the previous validated cached transcode recipe bundle. Does not alter running jobs or original media.")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mgr := cfg.Transcode.RecipeManager()
		if mgr == nil {
			return toolErr("transcode recipe manager is not initialized"), nil
		}
		st, err := mgr.Rollback()
		if err != nil {
			return toolJSON(map[string]any{"status": st, "rolled_back": false, "error": err.Error()}), nil
		}
		return toolJSON(map[string]any{"status": st, "rolled_back": true}), nil
	})
}
