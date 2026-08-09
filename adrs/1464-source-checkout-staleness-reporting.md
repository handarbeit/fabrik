# ADR 1464: Source-checkout staleness reporting, independent of the idle-upgrade gate

**Date**: 2026-08-09
**Status**: Accepted
**Issue**: #1464 — a busy source-checkout daemon runs arbitrarily stale code with no signal

## Context

On 2026-08-08 the primary dev instance of this repo's own Fabrik daemon started at 08:31 and
was still running `8a5ef0f` at 23:50 — 75 commits and ~15 hours behind `origin/main`, across ten
merges, with `--auto-upgrade` enabled the entire time. Every fix that landed that day was live
on `main` and absent from the running engine, and #1428's merge-train ejection diagnostics were
opaque at 18:59 specifically because #1420's fix to them had merged at 18:40 and simply wasn't
running. Nothing in the TUI, the startup output, or `fabrik.log` said so; the staleness was found
only by reading the binary's SHA by hand.

The root cause: `checkAndUpgrade` (`engine/upgrade.go`) — the function that actually rebuilds and
`syscall.Exec`s a dev build onto current code — is reachable from exactly two places: once at
startup, and once after `idleUpgradeThreshold` (2) *consecutive fully-idle* polls
(`engine/poll.go`). `idleCount` resets to 0 on any in-flight worker, including a repo-scoped
merge-train worker (`Store.HasInFlightWorker()`), and that reset is correct — `syscall.Exec`
would kill whatever Claude invocation is currently running. But it means a continuously busy
board (dispatching work, or with a merge-train worker in flight, on literally every poll) never
reaches the idle branch at all, and the only other trigger is a restart that never happens on its
own. Detecting staleness and acting on it were gated behind the same condition, even though only
the latter has a real safety reason to require idleness.

`internal/selfupgrade.CheckAndRebuildDev` (ADR-1196) already computes the comparison this issue
needs — running binary's embedded SHA vs. local `HEAD`, then local vs. `origin/<base>` — but only
as an inline step interleaved with the `git pull`/`go build`/`syscall.Exec` side effects that
follow it. There was no way to ask "how stale am I?" without also risking "and now rebuild."

## Decision

Split *detecting* staleness from *acting* on it, and give detection its own, always-reachable
call site. The existing idle-gated exec path (`checkAndUpgrade`, `idleUpgradeThreshold`,
`checkVersionSkew`) is untouched — this is purely additive.

### Extract the comparison: `selfupgrade.CompareDevBuild`

`internal/selfupgrade/devbuild.go` gains `CompareDevBuild(cfg DevBuildConfig) (DevBuildStatus,
error)`, containing exactly the read-only steps `CheckAndRebuildDev` used to run inline before
deciding to act: the `IsSourceCheckout` gate, resolving local `HEAD`, the SHA-mismatch shortcut
(skips the network fetch entirely when the running binary's embedded SHA already doesn't match
local `HEAD`), and — only when that shortcut doesn't fire — `git fetch` + resolving
`origin/<base>` + the `merge-base --is-ancestor` check. It additionally computes two things the
original inline code never needed: `CommitsBehind` (`git rev-list --count HEAD..origin/<base>`)
and `LocalCommitTime` (`git log -1 --format=%cI HEAD`, no extra network call) — R2's "commit count
and age of the running build."

`CompareDevBuild` structurally never calls `git pull`, `go build`, or `execFn` — there is no
branch, flag, or code path inside it that reaches any of the three. `CheckAndRebuildDev` is
rewritten to call `CompareDevBuild` for its decision (`ShaMismatch`/`RemoteAhead`/`NeedsRebuild`)
and only then proceeds to pull/build/exec exactly as before. This is the single source of truth
R4 asks for: there is no second, independently-written comparison for the staleness warning to
disagree with, because there is no other comparison. `devbuild_test.go`'s existing suite passes
unchanged against the refactor (the same regression-guard technique ADR-1196 used for its own
extraction), plus new tests exercise `CompareDevBuild` directly — including an explicit assertion
that a rebuild-eligible fixture still leaves the on-disk executable's bytes untouched and never
calls the `PostBuildHook`/`execFn` seams (the AC6 "structurally unreachable, not just
unreached-this-run" bar the issue's acceptance criteria set).

### New engine call site, deliberately outside the idle gate

`engine/staleness.go` adds `checkSourceStaleness()` and its poll-count gate,
`maybeCheckSourceStaleness()`. The gate is a flat poll counter
(`stalenessCheckPollInterval = 30`, ≈15 minutes at the default 30s `--poll` interval, firing on
the very first poll too — free startup coverage) with no idleness precondition at all. It's
called from `poll()` before the `if dispatched == 0 { ... idleCount ... }` block — never nested
inside it — which is what makes it reachable on a board that dispatches work every single cycle.

Applicability mirrors `checkAndUpgrade`'s existing fork: `e.cfg.Version` must start with `"dev"`,
then `CompareDevBuild`'s own `IsSourceCheckout` gate applies. A release build never calls into
`selfupgrade` for this check at all — zero git subprocesses, satisfying R6/AC5 by construction
rather than by an extra guard.

### Poll-count gate, not a ticker goroutine

Two scheduling idioms already coexist in this codebase: a dedicated background ticker goroutine
(`reconcileLoop`, `runWorktreeJanitor`), and an in-`poll()` `time.Time`/counter gate (the Layer 2
`lastProjectUpdatedAt` check). This issue's acceptance criteria need a fixture that drives many
poll cycles deterministically without real or mocked wall-clock waiting — a synchronous in-poll
counter is directly driveable by calling `poll()` N times in a test, which a ticker goroutine is
not. No new `Config`/CLI flag: `idleUpgradeThreshold` is precedent for a hardcoded const covering
the same class of decision, and R4 only asks that an interval be chosen and stated, not that it
be operator-tunable.

### Single fixed warning key, no stale-sweep needed

The warning is recorded under one fixed key, `"source_staleness"` — unlike
`allow_auto_merge`/`stage_drift` (one entry per board repo/configured stage, a set that shrinks),
there is exactly one source checkout per Fabrik process. Every throttled evaluation re-derives
the comparison from scratch and either records or clears the same key, so the `Clear` branch is
reachable on every single evaluation — the same reasoning `checkVersionSkew`'s own doc comment
already makes for its per-executable-path key (#1074). No `ClearMissing`-style sweep is needed;
see `docs/state-machine.md` §7.11 for the parallel writeup.

### Preserve the SHA-mismatch shortcut for the reporting use case too

`CompareDevBuild` keeps `CheckAndRebuildDev`'s existing behavior of skipping the network fetch
when the running binary's embedded SHA doesn't prefix local `HEAD` — an already-tested case
(`TestCheckAndRebuildDev_SHAMismatchTriggersRebuildWithoutFetch`). This means that rare edge case
(a human committed locally without the daemon rebuilding) reports "rebuild pending" without an
origin-relative commit count, rather than forcing an extra fetch purely to enrich a report. This
is acceptable because it isn't the incident's failure mode: on a running dev daemon, local `HEAD`
only ever advances via `CheckAndRebuildDev`'s own `git pull`, so in the ordinary "origin moved on,
nobody restarted" scenario the SHA still matches and the full fetch-and-count path runs.

### `kill -HUP` is a real fix only when `--auto-upgrade` is enabled

A SIGHUP re-execs the running binary in place (`performSighupRestart` → `syscall.Exec`), which
re-enters `main()` and re-runs the startup-time `checkAndUpgrade()` call. For a dev build, that
call does pull/rebuild/re-exec if stale — so `kill -HUP` transitively fixes staleness, but only
because the startup call itself is gated on `e.cfg.AutoUpgrade`. When `--auto-upgrade` is
disabled, that startup call never runs either, so a SIGHUP just re-execs the same stale binary.
`checkSourceStaleness` therefore branches its `FixAction`/`FixParams` on `AutoUpgrade`: a
`shell_command` fix (`kill -HUP <pid>`) only when it's enabled; when it's disabled, `FixAction` is
left empty and the `Detail` text spells out the manual `git pull && go build` + restart steps
instead. Offering `kill -HUP` unconditionally would be actively misleading for a real, common
configuration, not just unhelpful.

## Deliberately not in scope: automatic upgrade on a busy board

The obvious alternative — detect that `main` moved, drain in-flight workers, upgrade — was
considered and rejected for now, for reasons stated in the issue: it depends on #1393 (clean-stop
shutdown; today's drain path cancels the root context, killing Claude subprocesses mid-run), it
self-triggers on Fabrik's own merges (a ten-merge day becomes ten restarts, each paying a cold
board-bootstrap cache), and in-memory merge-train state does not survive a re-exec. A warning gets
the operator the same decision at none of that cost; if it proves to always be answered "yes,"
automating it is a follow-up once #1393 exists.

## Consequences

**Positive:**
- Closes the exact incident: a source-checkout daemon that dispatches work every poll now
  surfaces "N commits / H hours behind origin/main" via the TUI Warnings panel, startup output,
  and `fabrik.log` — the sentence that would have caught the 2026-08-08 incident.
- The staleness warning and the real upgrade decision share one comparison function
  (`CompareDevBuild`), so they cannot silently diverge — the exact risk ADR-1196 already flagged
  once for the extraction-vs-duplication choice, one layer deeper this time (extracting the
  comparison *within* `devbuild.go`, not just across a package boundary).
- Zero behavior change to the existing idle-gated exec path: `idleUpgradeThreshold`,
  `checkAndUpgrade`, and `checkVersionSkew` are untouched, verified by the existing test suite
  passing unchanged.
- `CompareDevBuild` is independently useful: any future caller that wants a read-only "is this
  dev checkout stale" answer (e.g. a `fabrik status` subcommand) can call it directly without
  risking a rebuild.

**Negative / Trade-offs:**
- A new `git fetch` every ~15 minutes on a source-checkout daemon, independent of idle state —
  bounded network/CPU cost, justified in the issue by the incident's own severity (a full working
  day of silent staleness) against a fetch's low per-call cost. The ~15-minute figure is a poll
  count (`stalenessCheckPollInterval` = 30), not a wall-clock duration — it only holds at the
  default 30s `--poll`. An operator running a much shorter `--poll` gets a proportionally shorter
  wall-clock interval and proportionally more frequent fetches; this is an accepted consequence of
  reusing the same poll-count convention `idleUpgradeThreshold` already uses, not a separate
  wall-clock ticker.
- Two structurally similar "is the on-disk/remote code newer than what's running" checks now
  exist (`checkVersionSkew` for the local disk/process split, `checkSourceStaleness` for the
  local/origin split) with slightly different triggers and messaging — an operator needs to read
  both entries to understand the full picture, though they're independent hypotheses about the
  same underlying symptom class ("what's running isn't what's meant to be running") and don't
  overlap in practice.
- The staleness check's own poll-count gate is a hardcoded constant, not configurable — acceptable
  per R4's explicit framing ("pick an interval... state it"), but a future operator wanting a
  tighter or looser cadence would need a code change, not a flag.

## Related Work

- `adrs/1196-extract-self-upgrade-package.md` — the prior extraction of `CheckAndRebuildDev`
  itself into `internal/selfupgrade`, and the "extraction over duplication" rationale this issue
  continues one layer deeper.
- `engine/upgrade.go` — `checkAndUpgrade`/`checkVersionSkew`, both untouched by this issue; the
  idle-gated exec path this issue's detection is deliberately decoupled from.
- `engine/staleness.go` — the new call site.
- `internal/selfupgrade/devbuild.go` — `CompareDevBuild`, the extracted comparison.
- `docs/state-machine.md` §7.11 (Stale Warning Sweep) and §9.2 (Worker In-Flight Guard) — the
  as-built documentation of the warning's no-sweep-needed reasoning and its deliberate exclusion
  from the idle+no-in-flight gate.
- #1074/#1348 — prior work establishing `checkVersionSkew` and the `warnings/` lifecycle this
  issue reuses.
- #1393 — clean-stop shutdown; prerequisite for any future automatic-upgrade-on-busy-board
  follow-up, explicitly out of scope here.
