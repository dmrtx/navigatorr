# Transcode recipe control plane

Navigatorr is the authority for transcode recipes. Workers are executors: they
receive an already validated, immutable `transcode.Plan` plus the existing
plan/execution-spec digests. A worker does not need a copy of the recipe
registry and does not install recipes.

## Three recipe layers

New actions resolve profiles with this precedence:

1. centrally managed profiles;
2. static deployment configuration overrides;
3. the active versioned recipe bundle.

The active bundle remains useful for bootstrap/default profiles and Git-backed
releases. Static configuration remains backward compatible. Day-to-day recipe
changes belong in the managed registry under the existing recipe cache
directory (`managed-profiles.json`) and are changed through the recipe MCP
tools.

The work queue is deliberately not recipe storage.

Managed saves are strict, atomic and versioned per profile. Each normalized
profile has a stable SHA-256 content digest, a generation, timestamps and an
optional `source_action_id`. Save/delete transitions are retained in bounded
history. A managed delete only affects future resolutions; running actions keep
the plan they already resolved.

## Ephemeral experiments

`benchmark_transcode` and `transcode_media` accept `profile_config`, a
complete typed `recipe.Profile` object. It is mutually exclusive with
`profile`.

The server strictly decodes `profile_config` with unknown-field rejection,
normalizes it, validates encoder-specific constraints, assigns an
`ephemeral_recipe_digest`, resolves it through the normal compatibility
resolver, and sends only the resulting immutable plan to the worker.

This unlocks the existing optimization policy without editing a deployed YAML.
For example, a profile can select `libx265`, `preset: slow`,
`tune: animation`, a CRF search ladder, and the existing sampling policy.
Arbitrary FFmpeg/x265 argument strings remain forbidden.

Legacy top-level experimental knobs are rejected instead of silently ignored.
For example, passing `tune` or `crf_candidates` directly to
`benchmark_transcode` fails and points the caller to `profile_config`.

A successful experiment can be persisted with `recipe_save`, copying the
normalized profile returned by the action and recording the action ID in
`source_action_id`. A later dedicated promote-from-action helper can verify
the action before saving, but it is not required for recipe distribution.

## MCP recipe tools

- `recipe_list`: inspect bundle/static/managed layers.
- `recipe_get`: inspect the effective typed profile and source layer.
- `recipe_save`: create/update a strict managed profile, optionally using
  `expected_digest` for optimistic concurrency.
- `recipe_delete`: remove the managed override without affecting running jobs.
- `recipe_history`: inspect save/delete generations.
- `recipe_status`, `recipe_reload`, `recipe_update`, `recipe_rollback`:
  retain their bundle/LKG responsibilities.

## Deployment and multiple workers

Ansible should bootstrap Navigatorr, permissions, cache/config volumes and
worker endpoints. It should not be the day-to-day distribution mechanism for
recipes.

This model naturally supports multiple transcode workers later: the coordinator
chooses a compatible worker and sends the same resolved plan. Worker discovery,
capability-aware routing and load balancing are separate scheduling concerns
and do not require recipe synchronization.
