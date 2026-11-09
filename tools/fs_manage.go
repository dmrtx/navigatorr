package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

// registerFsManageTools adds fs_mkdir (create folders), fs_copy (a verified
// copy that never overwrites unconfirmed) and fs_remove (remove one chosen
// file or folder, always previewed first).
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
		mcp.NewTool("fs_copy",
			mcp.WithDescription("Copy ONE file (a backup before importing a replacement) from allowed_read_roots to allowed_write_roots and prove it with SHA-256. The source is hashed while it is copied to a hidden temporary name next to the destination, the copy is flushed and read back, and it is put in place only when size and hash match and the source did not change; missing destination folders are created. The source is never modified. Returns status (copied, already_present or replaced), bytes, source_sha256, destination_sha256 (equal on success), created_dirs and, for a replace, replaced_sha256. Idempotent: a destination that already holds the same bytes is left untouched and reported as already_present, so a retry is safe. A destination with other content is never overwritten on its own: the call fails with status destination_differs and both hashes, writing nothing. To replace it, call again with replace_sha256 set to that destination_sha256, confirm=true and allow_destructive; the replace is atomic, refused if the destination changed meanwhile, and held to what fs_remove allows (nothing Radarr/Sonarr track or qBittorrent owns, no hidden/system names, nothing changed in the last 5 minutes). Paths are resolved (symlinks included) and must stay inside the roots. Refuses folders, a destination that is the source itself or a hard link of it, an allowed root, a share that is not mounted and a destination without room for the file. Large files take a while; the call honours client cancellation."),
			mcp.WithString("source", mcp.Required(), mcp.Description("File to copy (inside allowed_read_roots)")),
			mcp.WithString("destination", mcp.Required(), mcp.Description("Destination file path (inside allowed_write_roots), including the file name")),
			mcp.WithString("replace_sha256", mcp.Description("Only to overwrite: the destination_sha256 a destination_differs answer reported. Needs confirm=true")),
			mcp.WithString("confirm", mcp.Description("With replace_sha256: must be true")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := req.GetArguments()
			if d.Fs == nil {
				return toolErr("filesystem access is not configured"), nil
			}
			src, dst := argString(args, "source", ""), argString(args, "destination", "")
			replace := strings.ToLower(strings.TrimSpace(argString(args, "replace_sha256", "")))
			if replace != "" {
				if !argBool(args, "confirm", false) {
					return toolErr("confirm=true is required to replace the destination; nothing was written"), nil
				}
				if !allowDestructive {
					return toolErr("replacing a file is disabled. Set allow_destructive: true in config.yaml to enable; nothing was written"), nil
				}
				// A replace destroys the old file, so it is held to what
				// fs_remove allows: nothing Radarr/Sonarr track, qBittorrent
				// owns, hidden/system names or anything changed just now.
				if blocked, err := replaceBlocked(ctx, d.Fs, registry, qb, dst); err != nil {
					return toolErr("cannot tell what still depends on the destination, so nothing was written: %v", err), nil
				} else if len(blocked) > 0 {
					return toolErr("the destination cannot be replaced: %s; nothing was written", strings.Join(blocked, "; ")), nil
				}
			}
			res, err := d.Fs.Copy(ctx, src, dst, replace)
			var differs *fsop.DestinationDiffersError
			if errors.As(err, &differs) {
				_ = d.Store.LogAction("fs_copy", "", src, fmt.Sprintf(`{"destination":%q}`, dst), "refused: destination differs")
				body, _ := json.MarshalIndent(map[string]any{
					"status": "destination_differs", "source": res.Source, "destination": differs.Destination,
					"source_sha256": differs.SourceSHA256, "destination_sha256": differs.DestinationSHA256, "destination_bytes": differs.Bytes,
					"note": "the destination exists with different content; nothing was written. To overwrite exactly this content, call again with replace_sha256=<destination_sha256> and confirm=true.",
				}, "", "  ")
				return mcp.NewToolResultError(string(body)), nil
			}
			if err != nil {
				return toolErr("%v", err), nil
			}
			detail, _ := json.Marshal(res)
			_ = d.Store.LogAction("fs_copy", "", res.Source, string(detail), res.Status)
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

// replaceBlocked returns why an existing destination may not be replaced: the
// reasons fs_remove would give for removing it (with the minimum age). A
// destination that does not resolve or does not exist is left to Copy.
func replaceBlocked(ctx context.Context, fs *fsop.Resolver, registry *arrservice.Registry, qb *qbit.Client, dst string) ([]string, error) {
	real, err := fs.ResolveWrite(dst)
	if err != nil {
		return nil, nil
	}
	if _, err := os.Lstat(real); err != nil {
		return nil, nil
	}
	policy, err := removePolicy(ctx, registry, qb, real, fsop.MinRemoveAge)
	if err != nil {
		return nil, err
	}
	plan, err := fs.PlanRemove(real, false, policy)
	if err != nil {
		return nil, nil
	}
	return plan.Blocked, nil
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
