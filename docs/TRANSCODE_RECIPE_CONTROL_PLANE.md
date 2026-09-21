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
optional `source_action_id`. Generations are monotonic per profile name even
across delete/recreate cycles, so a version identity is never reused.
Save/delete transitions are retained in bounded history. A managed delete only
affects future resolutions; running actions keep the plan they already resolved.

Mutations fail closed by default: creating a previously unused/deleted profile
name omits `expected_digest`; updating an active managed profile requires its
current digest; deleting an active managed profile also requires its current
digest. This prevents an LLM or concurrent client from blindly overwriting or
deleting a recipe that changed after it was read.

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
normalized profile returned by the action. The MCP schema exposes `profile`
as a real structured object rather than JSON embedded in a string.

`source_action_id` is currently unverified reference metadata only. Navigatorr
does not yet prove that the action exists, completed successfully, or produced
the same digest. A later dedicated promote-from-action helper can provide that
verified provenance before saving; it is not required for recipe distribution.

## MCP recipe tools

- `recipe_list`: inspect bundle/static/managed layers.
- `recipe_get`: inspect the effective typed profile and source layer.
- `recipe_save`: create a strict managed profile, or update one only when the
  caller supplies its current `expected_digest`.
- `recipe_delete`: remove the managed override only with its current
  `expected_digest`, without affecting running jobs.
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
