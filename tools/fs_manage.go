package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jakenesler/navigatorr/arrservice"
	"github.com/jakenesler/navigatorr/fsop"
	"github.com/jakenesler/navigatorr/qbit"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// arrListCap bounds what one library manager may list for a removal check.
const arrListCap = 20000

// registerFsManageTools adds fs_mkdir (create folders) and fs_remove (remove
// one chosen file or folder, always previewed first).
func registerFsManageTools(s *server.MCPServer, d *Deps, registry *arrservice.Registry, qb *qbit.Client, allowDestructive bool) {
	s.AddTool(
		mcp.NewTool("fs_mkdir",
			mcp.WithDescription("Create a folder (and any missing parents) inside allowed_write_roots. A folder that already exists is not an error. Never creates the root itself, a hidden or system name (.x, @eaDir, #recycle), or anything through a symlink, and refuses when the root's share is not mounted."),
			mcp.WithString("path", mcp.Required(), mcp.Description("Folder path to create")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if d.Fs == nil {
				return toolErr("filesystem access is not configured"), nil
			}
			path := argString(req.GetArguments(), "path", "")
			res, err := d.Fs.Mkdir(path)
			if err != nil {
				return toolErr("%v", err), nil
			}
			detail, _ := json.Marshal(res)
			outcome := "created"
			if res.Existed {
				outcome = "already existed"
			}
			_ = d.Store.LogAction("fs_mkdir", "", path, string(detail), outcome)
			return toolJSON(res), nil
		},
	)

	s.AddTool(
		mcp.NewTool("fs_remove",
			mcp.WithDescription("Remove ONE file or folder inside allowed_write_roots, in two steps. mode=preview (default, read-only) returns exactly what would go (files, folders, bytes, the biggest entries) and a plan_digest, or the reasons it is blocked. mode=delete needs that plan_digest, confirm=true and allow_destructive: it plans again, removes only if the digest still matches, and takes the inside of a folder before the folder. A folder with files needs recursive=true. Blocked: an allowed root or a folder holding one, symlinks, special files, hidden/system names (.navigatorr-*, .voxsort-trash, @eaDir, #recycle, recovery), anything changed in the last min_age_minutes (default 10, minimum 5), anything qBittorrent owns or saves into, any file Radarr/Sonarr track and any movie or series folder (remove those through Radarr/Sonarr); if qBittorrent, Radarr or Sonarr cannot be read it refuses rather than guess. At most 2000 entries per removal. Files are deleted, not moved to a trash."),
			mcp.WithString("path", mcp.Required(), mcp.Description("File or folder to remove")),
			mcp.WithString("mode", mcp.Description("preview (default) or delete")),
			mcp.WithString("recursive", mcp.Description("Allow removing a folder together with its contents (default false)")),
			mcp.WithString("plan_digest", mcp.Description("delete: the plan_digest the preview returned")),
			mcp.WithString("min_age_minutes", mcp.Description("Keep anything changed more recently than this (default 10, minimum 5)")),
			mcp.WithString("confirm", mcp.Description("delete: must be true")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			if d.Fs == nil {
				return toolErr("filesystem access is not configured"), nil
			}
			path := argString(args, "path", "")
			mode := strings.ToLower(argString(args, "mode", "preview"))
			if mode != "preview" && mode != "delete" {
				return toolErr("unknown mode %q (use preview or delete)", mode), nil
			}
			recursive := argBool(args, "recursive", false)
			age := time.Duration(argInt64(args, "min_age_minutes", int64(fsop.DefaultRemoveAge/time.Minute))) * time.Minute
			if age < fsop.MinRemoveAge {
				age = fsop.MinRemoveAge
			}
			if mode == "delete" {
				if argString(args, "plan_digest", "") == "" {
					return toolErr("plan_digest is required: preview first, review what would go, then quote its plan_digest; nothing was removed"), nil
				}
				if !argBool(args, "confirm", false) {
					return toolErr("confirm=true is required; nothing was removed"), nil
				}
				if !allowDestructive {
					return toolErr("removal is disabled. Set allow_destructive: true in config.yaml to enable; nothing was removed"), nil
				}
			}
			real, err := d.Fs.ResolveWrite(path)
			if err != nil {
				return toolErr("%v", err), nil
			}
			policy, err := removePolicy(ctx, registry, qb, real, age)
			if err != nil {
				return toolErr("cannot tell what still depends on it, so nothing was checked or removed: %v", err), nil
			}

			if mode == "preview" {
				plan, err := d.Fs.PlanRemove(path, recursive, policy)
				if err != nil {
					return toolErr("%v", err), nil
				}
				_ = d.Store.LogAction("fs_remove_preview", "", plan.Path,
					fmt.Sprintf(`{"recursive":%v,"min_age_minutes":%d}`, recursive, int(age/time.Minute)),
					fmt.Sprintf("%s files=%d folders=%d bytes=%d blocked=%d", plan.Kind, plan.Files, plan.Dirs, plan.Bytes, len(plan.Blocked)))
				note := "preview removes nothing. To remove exactly this, call again with mode=delete, confirm=true and this plan_digest."
				if len(plan.Blocked) > 0 {
					note = "blocked: nothing can be removed until the reasons in blocked are gone."
				}
				return toolJSON(map[string]any{"plan": plan, "note": note}), nil
			}

			res := d.Fs.Remove(path, recursive, argString(args, "plan_digest", ""), policy)
			detail, _ := json.Marshal(res)
			_ = d.Store.LogAction("fs_remove", "", res.Path,
				fmt.Sprintf(`{"recursive":%v}`, recursive),
				fmt.Sprintf("%s files=%d folders=%d bytes=%d %s", res.Status, res.Files, res.Dirs, res.Bytes, detail))
			if res.Status == "failed" || res.Status == "skipped" {
				return toolErr("%s: %s", res.Status, res.Reason), nil
			}
			return toolJSON(res), nil
		},
	)
}

// removePolicy gathers what depends on the files at real: the download
// client's folders and the library managers' tracked files and folders.
func removePolicy(ctx context.Context, registry *arrservice.Registry, qb *qbit.Client, real string, age time.Duration) (fsop.RemovePolicy, error) {
	lp, err := leftoverPolicy(ctx, qb, age, []string{real})
	if err != nil {
		return fsop.RemovePolicy{}, err
	}
	rp := fsop.RemovePolicy{Now: lp.Now, MinAge: age, Keep: lp.Keep, Owned: lp.Owned}
	if registry != nil {
		rp.Tracked, err = arrClaims(ctx, registry, real)
		if err != nil {
			return rp, err
		}
	}
	return rp, nil
}

// arrClaims returns the movie and series folders and the tracked files of
// Radarr and Sonarr that touch real (inside it, or holding it).
func arrClaims(ctx context.Context, registry *arrservice.Registry, real string) ([]fsop.Claim, error) {
	var claims []fsop.Claim
	for _, name := range registry.List() {
		lower := strings.ToLower(name)
		radarr, sonarr := strings.HasPrefix(lower, "radarr"), strings.HasPrefix(lower, "sonarr")
		svc, err := registry.Get(name)
		if err != nil {
			return nil, err
		}
		if !radarr && !sonarr {
			// Sonarr and Radarr speak /api/v3. A service on that API under
			// another name could track library files this check would miss.
			if strings.Contains(strings.ToLower(svc.BaseURL+"/"), "/api/v3/") {
				return nil, fmt.Errorf("service %q uses the Sonarr/Radarr API but is not named sonarr or radarr, so its tracked files cannot be checked", name)
			}
			continue
		}
		if radarr {
			movies, truncated, err := arrCollection(ctx, svc, "/movie", arrListCap)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if truncated {
				return nil, fmt.Errorf("%s lists more than %d movies", name, arrListCap)
			}
			for _, m := range movies {
				folder, _ := m["path"].(string)
				if folder != "" && fsop.PathsOverlap(real, folder) {
					claims = append(claims, fsop.Claim{Path: folder, Owner: name, Folder: true})
				}
				if mf, ok := m["movieFile"].(map[string]any); ok {
					if p, _ := mf["path"].(string); p != "" && fsop.PathsOverlap(real, p) {
						claims = append(claims, fsop.Claim{Path: p, Owner: name})
					}
				}
			}
			continue
		}
		series, truncated, err := arrCollection(ctx, svc, "/series", arrListCap)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if truncated {
			return nil, fmt.Errorf("%s lists more than %d series", name, arrListCap)
		}
		for _, sr := range series {
			folder, _ := sr["path"].(string)
			if folder == "" || !fsop.PathsOverlap(real, folder) {
				continue
			}
			claims = append(claims, fsop.Claim{Path: folder, Owner: name, Folder: true})
			if sr["id"] == nil {
				return nil, fmt.Errorf("%s: a series without an id", name)
			}
			files, truncated, err := arrCollection(ctx, svc, "/episodefile?seriesId="+numStr(sr["id"]), episodeFileCap)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if truncated {
				return nil, fmt.Errorf("%s: a series has more than %d episode files", name, episodeFileCap)
			}
			for _, f := range files {
				if p, _ := f["path"].(string); p != "" {
					claims = append(claims, fsop.Claim{Path: p, Owner: name})
				}
			}
		}
	}
	return claims, nil
}
