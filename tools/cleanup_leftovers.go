package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	maxLeftoverRemovals = 50
	defaultLeftoverAge  = 60
	minLeftoverAge      = 5
)

// registerCleanupLeftoversTool adds the tool that clears what finished
// downloads leave in a downloads folder once nothing there is media or still
// belongs to qBittorrent.
func registerCleanupLeftoversTool(s *server.MCPServer, d *Deps, qb *qbit.Client, allowDestructive bool) {
	s.AddTool(
		mcp.NewTool("cleanup_download_leftovers",
			mcp.WithDescription("Find and remove folders a finished download left behind in a downloads folder: empty folders and folders holding only junk (.nfo, .png/.jpg, .txt, .sfv, .m3u, .url, .md5). mode=list (default) is read-only: it returns the exact folders that can go (class empty, junk_only, or junk_with_subtitles when loose .srt/.ass/.sub files are inside) and, for the top level, the folders kept and why. mode=remove_empty_dirs removes only folders holding no file; mode=remove_junk_dirs also removes the junk files in them; both need the exact folders from list in paths (at most 50), confirm=true and allow_destructive, re-check every folder immediately before removing it, and report removed, already_absent, skipped (with the reason) or failed per folder. Loose subtitles are removed only with include_subtitles=true. Never touches video or audio, an unknown file, a partial download (.!qB, .parts), a symlink, hidden or system folders (.navigatorr-*, .voxsort-trash, .oz-media-migration, @eaDir, #recycle, recovery), an allowed root, a folder qBittorrent owns or saves into (torrent, category, default and incomplete paths) or anything changed in the last min_age_minutes. If qBittorrent cannot be read the tool refuses rather than guess. Download-client paths are compared as the server sees them (the same mount paths), and only qBittorrent is consulted."),
			mcp.WithString("mode", mcp.Description("list (default), remove_empty_dirs or remove_junk_dirs")),
			mcp.WithString("path", mcp.Description("list: the downloads folder to scan (inside allowed_read_roots)")),
			mcp.WithString("depth", mcp.Description("list: how many levels below path to look, 1-4 (default 3)")),
			mcp.WithString("limit", mcp.Description("list: most folders to return, up to 200 (default 100); remove and list again for the rest")),
			mcp.WithString("min_age_minutes", mcp.Description("Keep anything changed more recently than this (default 60, minimum 5)")),
			mcp.WithString("paths", mcp.Description("remove: JSON array of the exact folders from list (at most 50)")),
			mcp.WithString("include_subtitles", mcp.Description("remove_junk_dirs: also remove loose subtitles (default false)")),
			mcp.WithString("confirm", mcp.Description("Must be true for removal")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			mode := strings.ToLower(argString(args, "mode", "list"))
			if mode != "list" && mode != "remove_empty_dirs" && mode != "remove_junk_dirs" {
				return toolErr("unknown mode %q (use list, remove_empty_dirs or remove_junk_dirs)", mode), nil
			}
			if d.Fs == nil {
				return toolErr("filesystem access is not configured"), nil
			}
			age := argInt64(args, "min_age_minutes", defaultLeftoverAge)
			if age < minLeftoverAge {
				age = minLeftoverAge
			}

			if mode != "list" {
				paths := argStrings(args, "paths")
				if len(paths) == 0 {
					return toolErr("paths must name the exact folders from list"), nil
				}
				if len(paths) > maxLeftoverRemovals {
					return toolErr("at most %d folders per call; got %d", maxLeftoverRemovals, len(paths)), nil
				}
				if !argBool(args, "confirm", false) {
					return toolErr("confirm=true is required; nothing was removed"), nil
				}
				if !allowDestructive {
					return toolErr("removal is disabled. Set allow_destructive: true in config.yaml to enable; nothing was removed"), nil
				}
			} else if argString(args, "path", "") == "" {
				return toolErr("path is required: the downloads folder to scan"), nil
			}

			policy, err := leftoverPolicy(ctx, qb, time.Duration(age)*time.Minute)
			if err != nil {
				return toolErr("cannot tell which folders qBittorrent still owns, so nothing was scanned or removed: %v", err), nil
			}

			if mode == "list" {
				scan, err := d.Fs.ScanLeftovers(argString(args, "path", ""),
					policy, int(argInt64(args, "depth", 3)), int(argInt64(args, "limit", 100)))
				if err != nil {
					return toolErr("%v", err), nil
				}
				_ = d.Store.LogAction("cleanup_leftovers_list", "", scan.Root,
					fmt.Sprintf(`{"mode":"list","min_age_minutes":%d}`, age),
					fmt.Sprintf("%d leftover folder(s), truncated=%v", len(scan.Found), scan.Truncated))
				return toolJSON(map[string]any{
					"scan":   scan,
					"note":   "list removes nothing. Pass the folders you want gone as paths with mode=remove_empty_dirs or remove_junk_dirs and confirm=true.",
					"client": policyNote(qb),
				}), nil
			}

			onlyEmpty := mode == "remove_empty_dirs"
			withSubs := argBool(args, "include_subtitles", false)
			results := make([]fsop.LeftoverRemoval, 0, maxLeftoverRemovals)
			seen := map[string]bool{}
			var removed, skipped, failed int
			var bytes int64
			for _, p := range argStrings(args, "paths") {
				if seen[p] {
					continue
				}
				seen[p] = true
				r := d.Fs.RemoveLeftover(p, onlyEmpty, withSubs, policy)
				switch r.Status {
				case "removed":
					removed++
					bytes += r.Bytes
				case "failed":
					failed++
				case "skipped":
					skipped++
				}
				results = append(results, r)
			}
			detail, _ := json.Marshal(results)
			_ = d.Store.LogAction("cleanup_leftovers_remove", "", "",
				fmt.Sprintf(`{"mode":%q,"include_subtitles":%v,"paths":%d}`, mode, withSubs, len(seen)),
				fmt.Sprintf("removed=%d skipped=%d failed=%d bytes=%d %s", removed, skipped, failed, bytes, detail))
			return toolJSON(map[string]any{
				"mode": mode, "removed": removed, "skipped": skipped, "failed": failed,
				"bytes_freed": bytes, "results": results,
			}), nil
		},
	)
}

func policyNote(qb *qbit.Client) string {
	if qb == nil {
		return "qBittorrent is not configured: no folder was treated as owned by a download client"
	}
	return "folders qBittorrent owns or saves into were kept"
}

// leftoverPolicy asks qBittorrent what it owns. With no client configured
// nothing is owned; with one that cannot be read, the caller must stop.
func leftoverPolicy(ctx context.Context, qb *qbit.Client, minAge time.Duration) (fsop.LeftoverPolicy, error) {
	p := fsop.LeftoverPolicy{Now: time.Now(), MinAge: minAge}
	if qb == nil {
		return p, nil
	}
	torrents, err := qb.ListTorrents(ctx)
	if err != nil {
		return p, fmt.Errorf("listing torrents: %w", err)
	}
	cats, err := qb.CategorySavePaths(ctx)
	if err != nil {
		return p, fmt.Errorf("reading categories: %w", err)
	}
	defaults, err := qb.DefaultPaths(ctx)
	if err != nil {
		return p, fmt.Errorf("reading preferences: %w", err)
	}
	p.Keep = append(p.Keep, cats...)
	p.Keep = append(p.Keep, defaults...)
	for _, t := range torrents {
		if t.SavePath != "" {
			p.Keep = append(p.Keep, t.SavePath)
		}
		content := strings.TrimRight(t.ContentPath, "/")
		save := strings.TrimRight(t.SavePath, "/")
		switch {
		case content == "":
		case content != save:
			p.Owned = append(p.Owned, content)
		default:
			// A torrent without its own folder keeps its files straight in the
			// save path: own each file by name so subfolders it fills stay.
			files, err := qb.ListFiles(ctx, t.Hash)
			if err != nil {
				return p, fmt.Errorf("listing files of a torrent: %w", err)
			}
			for _, f := range files {
				p.Owned = append(p.Owned, path.Join(save, f.Name))
			}
		}
	}
	return p, nil
}
