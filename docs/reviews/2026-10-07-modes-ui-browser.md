# Processing modes and scheduler UI — local browser evidence

Checked 2026-10-07 against the working-tree implementation based on
`5b7d378fe55a61147b66d92925b9a30682af97fb`.

## Environment and boundary

The root reviewer used the real in-app browser with the repository's HTML,
CSS and JavaScript served from a loopback-only synthetic HTTP adapter at
`http://127.0.0.1:18791`. The adapter lived at
`/private/tmp/navigatorr-ui-fixture.py`; it exposed two invented video paths,
a synthetic worker observation, empty activity, a built-in profile and a
failing profile-save response. No production requests, encodes, replacements,
recovery deletions or preset mutations were performed.

The adapter captures UI submissions; it does not run the real action engine.
These checks demonstrate browser interaction and submitted scope/policy fields,
not backend execution, quality acceptance or UI/MCP cross-control.

## Observed results

- Initial load rendered inventory and the contextual configuration form.
- A single-file Smaller files submission produced:
  `{"mode":"size","path":"/fixture/one.mkv"}`.
- Selecting both files and Prioritize quality produced:
  `{"mode":"quality","paths":["/fixture/one.mkv","/fixture/two.mkv"]}`.
- A single-file Preserve quality / x265 submission produced:
  `{"mode":"x265_preserve","path":"/fixture/one.mkv"}`.
- The three captured payloads contained no hidden profile, quality, size-limit,
  priority or preservation overrides. The reviewer read the capture at
  `/private/tmp/navigatorr-ui-submissions.json`.
- Header, Activity and Settings showed the same synthetic degraded scheduler
  observation. The detail correctly distinguished a normal post-encode sweep
  from a degraded queue sweep.
- At a 390 × 844 mobile viewport, Settings navigation and the three-mode
  configuration form were visually legible. Existing selection/configuration
  navigation remained two steps.
- The profile editor retained the permanent empty Description label and a
  visible focus indicator. A synthetic failed save preserved the edited text,
  dirty indication and announced failure.
- Browser Back/Forward away from and back to the same profile preserved its
  dirty draft without an inaccurate discard confirmation.

The root reviewer inspected screenshots during the browser check; no durable
screenshot artifact was exported. The viewport was reset afterward.

## Automated checks

`node --test maintenanceui/*test.mjs`: 167 tests passed. Regressions cover
mode payload isolation, explicit folder/Sonarr scope, hidden discovery limits,
legacy submissions, scheduler recovery/freshness, initial worker/render races,
failed saves, cancellation of discard, history navigation and editing while a
save is in flight.

The maintenanceui Go suite passed with local HTTP fixtures enabled.
Targeted recipe manager and MCP tests passed for reviewed restoration,
incomplete/stale digest pairs, activation during confirmation, no mutation on
conflict and unchanged running snapshots. Integration-suite results and final
commit identity are reported by the parent reviewer.

## Subsequent coordinator instrumentation regressions

After the browser check, coordinator inventory, preflight/hash/probe and
promotion invocation wall measurements were added. They retain existing
statuses, errors and skip outputs, persist with the action and exclude time
between invocations. `active_compute_ms` and unmeasured byte/cache counts
remain unknown; overlapping phases are not summed into total duration.
Worker and benchmark costs are namespaced cumulative snapshots, so repeated
polls replace observations rather than add duplicate work.

Targeted Go regressions passed for a restart across a synthetic 24-hour human
wait, preserving the measured 2-second inventory and 7-second combined
promotion invocations without replaying completed work. The injected clock
makes this a lifecycle/accounting regression, not a performance benchmark.
A regression also confirms UI operations and MCP action_status expose the
same persisted costs. Node tests exercise the read-only detail with unknown
CPU/bytes, cache hits/misses and search caps distinct from measured consumption.
The newly added phase-cost detail was checked with these automated tests;
it was not part of the earlier real-browser screenshot inspection.
