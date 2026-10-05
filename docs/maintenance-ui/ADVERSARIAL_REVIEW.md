# Adversarial review — 2026-10-04

The initial local review and the subsequent mobile-flow corrections are recorded here. Reported layout and navigation concerns were checked against the running interface, together with delayed responses, failed reads, repeated taps, expired authentication and consumed replacement candidates. Findings below are fixed in the review branch. No deployment or merge was performed; the requested Cloudflare deployment is cancelled.

## Findings fixed

| Area                  | Correction                                                                                                                                                                                                                                                                                                                                           |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| File rows             | Folder icons and file checkboxes occupy the same column, aligning every filename. Sizes use lighter metadata text and appropriate B/KB/MB/GB units.                                                                                                                                                                                                  |
| Search and navigation | Search can close and clear its filter. Changing folders removes stale actionable rows immediately; failed reads offer Retry and recover automatically. Submission controls stay disabled during reads or without a source.                                                                                                                           |
| Dialogs               | Notifications appear inside the active modal. The path dialog's Use file button stays on one line. Missing files produce a concise error. Dialog actions retain content-sized widths.                                                                                                                                                                    |
| Authentication        | Access redirects and HTML denials invalidate stale controls before parsing the body. Expired sessions close all approval dialogs. Cloudflare mode exposes no local token, sign-out or empty local-session row.                                                                                                                                       |
| Profiles              | Optional VideoToolbox settings can be configured on new profiles, and optimization creates valid sampling/quality/search policies. Invalid numbers cannot poison a saved draft. Required preservation remains enforced. Merely opening settings does not add optional values. Failed profile reads keep the old draft inactive until retry succeeds. |
| Replacement routing   | UI/MCP jobs use durable library context or their parent batch. Unknown historical context requires an explicit method. Existing replacement actions are reused, and consumed candidates lose replacement controls and pending-savings labels. Exact paths still require approval before mutation.                                                    |
| Job detail            | Failed reads clear stale controls and expose Retry. Repeated batch paging cannot skip pages; failures restore the prior offset.                                                                                                                                                                                                                      |

## Visual and interaction coverage

Files, Queue, Profiles, Stats and Settings were checked at 320, 360, 390, 430 and 768 pixels, landscape 844×390 and desktop 1280×900. No page overflow or clipped form fields remained in these checks. Modal settings, replacement selection, exact batch approval and manual-path errors were inspected on the running temporary preview.

The interface is English, with compact typography, icon navigation and refresh, full-row progress, separate statistics, continuous Files/Queue loading and direct folder breadcrumbs. Installation help is opt-in and dismissible. Marketing headers, duplicate listings, generic MCP/JSON consoles and persistent installation instructions are absent. Settings uses dedicated shared-preset and recovery controls.

## Follow-up corrections

- Single-column Files layouts now have two steps: selection and configuration. This applies through 900 pixels, including phone landscape. Wider desktop views retain both panes. Back preserves settings and selection and restores keyboard focus. Single-file, multi-file, whole-folder and manual-path transitions were exercised on the running preview; no jobs were submitted.
- File checkboxes now have a 14-pixel visual, with the original generous hit area and aligned folder-icon column. Open a path is an icon in the source toolbar.
- Queue rows align the filename, status, concise result/size summary and available actions. Failed jobs expose their error reason when available, candidate savings remain distinct from freed bytes, and decision rows no longer display stale worker telemetry or misleading zero-file counts.
- Shared presets replaces Profile bundle. Refresh presets replaces the duplicate Reload/Update operations; Restore previous remains. The UI identifies their scope as new jobs only.
- These changes were visually checked at 320 and 390 pixels, landscape 844×390 and desktop 1280×900. No horizontal overflow or clipped form fields remained. Fresh preview console checks found no runtime errors.

Screenshots in [screenshots](screenshots/) show synthetic fixtures, including deliberately tiny files whose sizes come from disk. They are visual evidence, not production media measurements.

## Independent agy review and acceptance

The user-requested `coding-agent-mcp` review ran with `agent: agy` in read-only review mode. Its first report covered Files, Queue, Profiles, dialogs and responsive behavior; a second report proposed the Queue/Profile action hierarchy. Neither run edited the repository. Findings were checked against the current source and live preview before acceptance.

- Desktop checkbox selection now immediately prepares a single-file or selected-files batch. Opening a filename makes that file's selection explicit, clearing an earlier batch; Back shows the corresponding checkbox and selected count.
- Successful encoding clears submitted sources and returns Files to its browse step while retaining encoding preferences. Benchmarks retain their source for subsequent encoding. Failed submissions retain the source and idempotency receipt. These transitions are covered by regressions; no encoding job was submitted during visual review.
- Batch summaries explicitly identify the number of selected files. A hidden season field no longer leaves the file limit confined to half a row.
- Root selectors show the folder name, with the full path as a tooltip. Selects have space for their arrow, and narrow job/audio fields use the available row. The final 320-pixel preview had no page overflow or clipped controls.
- Navigation uses `aria-current` on ordinary buttons. Dialogs have accessible names. Profile settings has a bottom Done action; the separate Save action persists the draft. Path inspection is labelled Use file and advances only after successful validation.
- Queue and Profile actions share a single size token: 36×36 pixels on desktop and 44×44 for touch/single-column views, with 18-pixel icons and a 32-pixel visible surface. Dialog buttons use that same token. The final mobile geometry check measured every rendered Queue/Profile action at 44×44.
- The action hierarchy follows agy's semantic critique: replacement/approval/save uses forest fill, review/settings/history uses a quiet tinted surface, retry uses a warm recovery tint, and cancel is frameless. Icons replace repeated boxes and long labels in the list; accessible names and tooltips preserve their meaning. Desktop Queue actions align in one rail; phone actions sit beneath the summary. Profile restore/delete is separated from Save and distinguishes restoring an override from deleting a custom profile.
- Mobile Profiles uses one native profile picker instead of a scrolling row of large profile buttons. The vague More settings link is replaced by the named sliders control. History is unavailable until a saved profile is selected.

Two findings were already resolved while the review ran: the 900-pixel focus breakpoint and manual-path advancement. The claim that file checkboxes had a 16-pixel hit area was rejected after measurement: the label is 44×44 on mobile; only the visual input is 14×14. Keyboard focus outlines remain visible.

Final visual acceptance covered 320×844 and 390×844, landscape 844×390, and desktop 1280×900. It included native audio/profile selection, single and batch file selection, Back/Continue focus, advanced-profile Done, action alignment and fresh-console inspection.

## Validation

- Full Go suite: `CGO_ENABLED=0 go test -count=1 ./...` passed.
- `go vet ./...`, Go formatting, JavaScript syntax and diff checks passed.
- 70 UI/PWA regressions passed, including authentication edge responses, delayed selections, failed reads, single-flight paging/submission, optional profile settings, durable replacement routing, two-step selection/focus, explicit file selection, submission reset and shell-cache isolation.
- The focused Go UI server suite passed again after the follow-up interface changes.
- Serialized x265 and VideoToolbox optimization/bitrate profiles passed strict Go schema validation.

## Remaining acceptance checks

Real Google/Cloudflare Access login and private-host policy, live NAS/worker encoding and replacement, and installation/keyboard behavior on physical iOS/Android devices were not exercised. The preview uses a simulated authenticated gateway, temporary SQLite data and synthetic media. No production files were replaced.

Queue pagination uses bounded offset pages rather than a server snapshot token. A changing large queue can shift records between continuation reads; deduplication and the next full visible-window refresh correct the view. Batch file details keep bounded paging. Savings reports verified file-content reduction after recovery cleanup, rather than disk-volume free space.


## Queue follow-up: real batches and replacement lifecycle

A LAN production review found anonymous filesystem batches, omitted skip reasons, a separate replacement row for an existing conversion, and no visible progress during local promotion steps. Queue projection now groups only durable parent/candidate links before filtering and pagination, retains a stable workflow number, and puts active work first. Independently submitted previews and executions remain distinct. Global savings accounting still uses the ungrouped lifetime ledger.

Batch rows show their folder/season, two actual filenames, total file count and bounded skip reasons; the file dialog keeps the full paginated contents and reasons. Preview eligibility is labelled eligible rather than queued. Active batch telemetry identifies the measured child file. Replacement rows show the current named step, and details show all eight numbered stages without inventing a byte percentage. Conversion and its linked replacement use the same queue identity, with the original conversion available in details.

Replacement approval uses an application dialog with original/candidate paths and sizes, potential savings and recovery behavior. Opening it is read-only. Approval rechecks action status, available decision and both content hashes; expiration or a changed plan prevents submission. Review and Replace retain short visible labels and consistent control sizing.

Validation: full Go tests and vet, 73 JavaScript tests, grouping/filter/pagination and live-child telemetry regressions, plus browser inspection of synthetic running/review/skipped states at desktop, 390 and 320 pixels. No production media was changed by this review.
