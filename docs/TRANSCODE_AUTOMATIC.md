# Automatic processing v2

The existing Process button and MCP `action_run` use `mode=size|quality|x265_preserve`.
There are no new configuration steps. New operations freeze `transcode-modes-v2`;
existing v1 snapshots retain their original thresholds and 15% quality saving gate.

* size: existing sampled quality gates and at least 15% actual savings.
* quality: existing strict sampled quality gates, with no output growth.
* x265_preserve: strict sampled quality, libx265 only, no growth; existing HEVC omitted.

For sources at least 50 seconds long, one immutable benchmark contains three
discovery windows and two nonoverlapping verification windows. The worker first
tests the existing x265 medium 20/23/26 and supported VideoToolbox 65/70/75
settings. If measurements fail, it tests one appropriate x265 slow family:
16/18/20 for quality rejection, or 23/26/28 when measured quality can pass but
size cannot. The alternative also applies after an initial verification failure.
At most two candidates receive verification. Only a candidate passing all five
windows can start a full conversion. Actual final bytes, sampled final quality,
streams and source/candidate integrity still must pass before publication.

All choices and limits are frozen before admission. No profile is created by
this search, no quality threshold is relaxed, and no new action restarts its
600-second deadline. The operation's existing 3600-second search allowance and
same-source unsuccessful-search review remain enforced. A negative measured
outcome preserves the original; it does not imply every possible encode is bad.

`action_status.transcode` and the web detail use the same explanation, including
tested candidates, stages and verified sample count. A benchmark winner and an
accepted full conversion are distinct outcomes. `transcode_goal_change` provides
a read-only selection preview and a versioned command for the same goal recovery
available in the web UI. Parent inputs and previous attempts remain immutable;
active attempts finish before another attempt is admitted. Replacement approval
is separate and is never authorized by processing or changing a goal.

The metrics describe the actual sampled source class and model, not a universal
quality percentage. No claim of full-motion human review or lossless video is made.
