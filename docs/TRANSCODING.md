# Transcoding architecture

Navigatorr treats transcoding policy as data, not as worker code.

The stable image/worker exposes a small hard-coded **capability boundary**: supported containers, encoders, stream operations, preservation features, and safe codec conversions. A versioned **recipe bundle** decides which of those capabilities to use for a profile and how to handle container compatibility. Recipes cannot add worker capabilities, raw FFmpeg arguments, shell commands, or arbitrary binaries.

This separation is intentional: a compatibility or tuning problem should normally be fixed by publishing/editing a recipe and reloading it, not by rebuilding the Navigatorr or worker image.

## Normal failure workflow

When a transcode fails because the current policy does not describe a safe way to handle the media:

1. Inspect the source streams and the typed failure classification.
2. Change or add a recipe/profile using only supported declarative fields.
3. Increment `bundle_version` when publishing a changed version.
4. Run `recipe_reload` for a file source or `recipe_update` for a remote source.
5. Confirm `recipe_status` reports the expected version and SHA-256 digest.
6. Retry `transcode_media`.

The existing image is unchanged. If the new bundle is malformed, unsafe, incompatible with the worker capability boundary, has the wrong digest, or cannot be fetched, Navigatorr keeps the last-known-good bundle active.

A new image is required only when the engine itself needs a **new capability** (for example, a new encoder or conversion implementation), not when an existing capability merely needs different policy.

## Recipe sources

`transcode.recipes.source` accepts:

- `builtin` — embedded safe default bundle; useful for zero-config startup.
- `file` — local YAML or JSON bundle, suitable for a bind mount or configuration management.
- `https` — strict HTTPS manifest that points to a same-host versioned bundle and includes its SHA-256 digest.
- `github` — versioned manifest/bundle hosted on `raw.githubusercontent.com`, selected by `stable`, `beta`, or a pinned revision.

Example configuration:

```yaml
transcode:
  enabled: true
  executor: ssh
  default_profile: hevc-vt-balanced

  recipes:
    source: file
    path: /config/transcode-recipes.yaml
    cache_dir: /config/cache/transcode-recipes

  ssh:
    host: 192.0.2.10
    user: transcoder
    remote_binary: /opt/homebrew/bin/navigatorr-transcode
```

GitHub channel example:

```yaml
transcode:
  recipes:
    source: github
    repository: example/transcode-recipes
    channel: stable
    refresh_interval: 15m
    cache_dir: /config/cache/transcode-recipes
```

A pinned `revision` may be used instead of `channel`. `main` and `master` are deliberately rejected as pinned revisions so a supposedly immutable deployment cannot silently change underneath a running installation.

## Bundle schema

A bundle is strictly decoded. Unknown fields fail validation.

```yaml
schema_version: 1
bundle_version: "2026.09.2"

containers:
  mkv:
    subtitle_copy:
      - subrip
      - ass
      - ssa
      - hdmv_pgs_subtitle
    subtitle_conversions:
      mov_text:
        target_codec: subrip
        reason: matroska_compatibility

profiles:
  hevc-vt-balanced:
    container: mkv
    video:
      codec: hevc_videotoolbox
      quality: 65
    audio:
      mode: copy
    subtitles:
      mode: preserve
      convert_incompatible: true
    preserve:
      metadata: true
      chapters: true
      attachments: true
    resilience:
      max_attempts: 3
      transient_retries: 2
      retry_backoff_seconds: [5, 20]
      max_fallbacks: 1
      fallbacks:
        - when: container_subtitle_incompatible
          action: apply_container_conversion
        - when: worker_busy
          action: retry
        - when: ssh_transient
          action: retry
```

JSON is also accepted because the strict YAML decoder supports JSON syntax. The schema intentionally contains no raw argument field.

Local `transcode.profiles` entries are retained as deterministic whole-profile overrides for backwards compatibility. They do not extend worker capabilities and are validated through the same recipe rules.

## Plan resolution and worker boundary

Before SSH submission, Navigatorr probes the source and resolves every subtitle stream into an exact action. For the built-in Matroska policy, for example:

- `mov_text` -> `subrip` when the profile authorizes the container conversion fallback.
- `ass`, `ssa`, `subrip`, supported bitmap subtitles, and other allowlisted Matroska-compatible codecs -> `copy`.
- an unknown codec with no recipe-authorized safe conversion -> fail closed before submission.

The resulting immutable plan contains the recipe version, recipe SHA-256 digest, plan SHA-256 digest, exact per-stream actions, bounded retry policy, and applied fallbacks. The worker probes the source again and verifies stream index/codec plus the plan digest before constructing FFmpeg argv. A recipe therefore cannot smuggle in unsupported operations.

The worker still has one narrowly scoped legacy `hevc-vt` profile-only compatibility shim for rolling upgrades. New Navigatorr submissions always send a fully resolved plan; the shim is not the normal policy path and should be removed after old clients are retired.

## Hot reload, LKG, and rollback

Recipe activation is atomic. A candidate bundle is fetched, integrity-checked, parsed, semantically validated, cached, and persisted before the active pointer is swapped. Jobs that already resolved a plan continue with that exact plan even if a newer bundle becomes active.

Versioned bundles are cached as `<cache>/<bundle_version>/bundle.yaml`. Reusing the same `bundle_version` with different bytes/digest is rejected as an immutable-version collision.

The MCP tools are:

- `recipe_status` — active source/version/digest, LKG and last sanitized error.
- `recipe_reload` — re-read and atomically activate the configured source when valid.
- `recipe_update` — fetch the configured remote/versioned source now.
- `recipe_rollback` — atomically activate the previous validated cached bundle.

Bad updates never replace the active LKG.

## VideoToolbox quality semantics

For `hevc_videotoolbox`, FFmpeg maps `-q:v` to VideoToolbox's quality property as a value from 0 to 1. Higher numeric `-q:v` values therefore mean **higher quality**, not more compression.

The built-in profiles use:

- `hevc-vt-balanced`: 65
- `hevc-vt-quality`: 75
- `hevc-vt-space`: 55

## Safety and validation

`transcode_media` is candidate-only. `replace_original: true` is rejected. The original is SHA-256 hashed before work begins and re-hashed at acceptance.

Post-transcode validation checks duration, video codec, audio stream count/codecs/languages/channels, subtitle count and expected copy/conversion codecs, subtitle languages and `forced`/`default` dispositions, attachments, and chapters. A discrepancy requires rejection or an explicit user decision; the original is not deleted or overwritten.

## Rollout

A safe rollout is:

1. Deploy the new Navigatorr and worker once so the capability engine understands versioned plans.
2. Start with `source: builtin` and verify `recipe_status`.
3. Move policy to `source: file` or a versioned GitHub/HTTPS source.
4. For future policy/compatibility changes, publish a new recipe bundle version and call `recipe_update`/`recipe_reload` instead of rebuilding the image.
5. If a recipe causes trouble, use `recipe_rollback`; fetch/validation failures automatically keep LKG active.
