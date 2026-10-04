# Adversarial review — 2026-10-04

The local review is complete. All reported layout and navigation concerns were checked against the running interface, together with delayed responses, failed reads, repeated taps, expired authentication and consumed replacement candidates. Findings below are fixed in the review branch. No deployment or merge was performed; the requested Cloudflare deployment is cancelled.

## Findings fixed

| Area                  | Correction                                                                                                                                                                                                                                                                                                                                           |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| File rows             | Folder icons and file checkboxes occupy the same column, aligning every filename. Sizes use lighter metadata text and appropriate B/KB/MB/GB units.                                                                                                                                                                                                  |
| Search and navigation | Search can close and clear its filter. Changing folders removes stale actionable rows immediately; failed reads offer Retry and recover automatically. Submission controls stay disabled during reads or without a source.                                                                                                                           |
| Dialogs               | Notifications appear inside the active modal. The path dialog's Open button stays on one line. Missing files produce a concise error. Dialog actions retain content-sized widths.                                                                                                                                                                    |
| Authentication        | Access redirects and HTML denials invalidate stale controls before parsing the body. Expired sessions close all approval dialogs. Cloudflare mode exposes no local token, sign-out or empty local-session row.                                                                                                                                       |
| Profiles              | Optional VideoToolbox settings can be configured on new profiles, and optimization creates valid sampling/quality/search policies. Invalid numbers cannot poison a saved draft. Required preservation remains enforced. Merely opening settings does not add optional values. Failed profile reads keep the old draft inactive until retry succeeds. |
| Replacement routing   | UI/MCP jobs use durable library context or their parent batch. Unknown historical context requires an explicit method. Existing replacement actions are reused, and consumed candidates lose replacement controls and pending-savings labels. Exact paths still require approval before mutation.                                                    |
| Job detail            | Failed reads clear stale controls and expose Retry. Repeated batch paging cannot skip pages; failures restore the prior offset.                                                                                                                                                                                                                      |

## Visual and interaction coverage

Files, Queue, Profiles, Stats and Settings were checked at 320, 360, 390, 430 and 768 pixels, landscape 844×390 and desktop 1280×900. No page overflow or clipped form fields remained in these checks. Modal settings, replacement selection, exact batch approval and manual-path errors were inspected on the running temporary preview.

The interface is English, with compact typography, icon navigation and refresh, full-row progress, separate statistics, continuous Files/Queue loading and direct folder breadcrumbs. Installation help is opt-in and dismissible. Marketing headers, duplicate listings, generic MCP/JSON consoles and persistent installation instructions are absent. Settings uses dedicated profile-bundle and recovery controls.

Screenshots in [screenshots](screenshots/) show synthetic fixtures, including deliberately tiny files whose sizes come from disk. They are visual evidence, not production media measurements.

## Validation

- Full Go suite: `CGO_ENABLED=0 go test -count=1 ./...` passed.
- `go vet ./...`, Go formatting, JavaScript syntax and diff checks passed.
- 59 UI/PWA regressions passed, including authentication edge responses, delayed selections, failed reads, single-flight paging/submission, optional profile settings, durable replacement routing and shell-cache isolation.
- Serialized x265 and VideoToolbox optimization/bitrate profiles passed strict Go schema validation.

## Remaining acceptance checks

Real Google/Cloudflare Access login and private-host policy, live NAS/worker encoding and replacement, and installation/keyboard behavior on physical iOS/Android devices were not exercised. The preview uses a simulated authenticated gateway, temporary SQLite data and synthetic media. No production files were replaced.

Queue pagination uses bounded offset pages rather than a server snapshot token. A changing large queue can shift records between continuation reads; deduplication and the next full visible-window refresh correct the view. Batch file details keep bounded paging. Savings reports verified file-content reduction after recovery cleanup, rather than disk-volume free space.
