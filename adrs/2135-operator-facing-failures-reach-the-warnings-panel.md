# ADR 2135: Operator-facing failures always reach the warnings panel

**Status:** Accepted
**Date:** 2026-10-11
**Issue:** [#2135](https://github.com/handarbeit/fabrik/issues/2135)

## Context

The TUI Warnings panel (ADR-052) is backed by a dismissable, de-duplicated store
(`warnings/warnings.go`). Only six conditions used it — source staleness, repository access,
`allow_auto_merge`, version skew, stage drift and undeclared reviewers — and each recorder logged and
then called `warnings.Record` by hand, with nothing forcing a new warning to do both. Many other
operator-level problems of the same kind (an unrecognised `.fabrik/config.yaml` key, an invalid setting
silently replaced by its default, a gitignored config file, a missing git credential helper, a failed log
rotation, repos receiving no webhooks…) reached only the scrolling log, where a busy operator misses them.
The skill-naming chain (#2137, #2139, #2140) adds more conditions that must be *loud*.

## Decision

### 1. What belongs in the panel

An **operator-level condition**: a configuration, environment or engine fault that **blocks or degrades
work** and **needs a person to act**. Per-item states — paused, awaiting input, escalated — already have
their own surfaces (the board, the attention classifier, item comments) and are not moved here, nor are
transient runtime states (a poll error, a rate limit) that the header's backoff status already shows, nor
read-only/one-shot subcommand output. Borderline lines are dispositioned individually with a reason
(`docs/state-machine.md` §7.11 audit table), never defaulted to the panel.

### 2. One shared record/clear path — `warnings.Report` / `Resolve` / `Reconcile`

`warnings/report.go` adds a `Condition` and three functions. `Report(w, c)` writes the log line to `w`
(nil = none) **and** records the entry in one call, so a condition cannot reach only the log by accident.
`Resolve(key)` clears one entry when its re-checked condition no longer holds. `Reconcile(w, type, cs)`
reports a complete set and then `ClearMissing`s the type (ADR-1348's set-sweep idiom) — it is for checks
that **ran to completion**: an empty set clears the type, so a check that errored or returned early must
not call it. The writer is a parameter, so `Run()`'s old duplicated "log file open / not open" branches
collapse to one `startupW` and cannot diverge. All six existing recorders migrated, with their keys,
types and texts unchanged (`Condition.KeyOverride` keeps the legacy `source_staleness` key).

A source-scan test (`warnings/scan_test.go`) fails on any `[startup] warn…`, `[warn]`,
`[upgrade] warning` or `WARNING:` literal in the daemon's source that is neither routed through the shared
path nor on an audited exception list with a stated reason (FR-005).

### 3. Dismissal follows a stable fingerprint, not raw `Detail`

The spec says a dismissed entry reappears when its detail changes. Comparing raw `Detail` would break
`source_staleness` (commit counts in the detail), `version_skew` (the PID) and the TUI's `[Fix failed]`
append. `Entry.Fingerprint` (optional, `omitempty`, file version stays 1) is the stable identity:
`Record` resets `Dismissed` only when both the stored and the new fingerprint are non-empty and differ; an
empty new fingerprint keeps the stored one. `Report` defaults the fingerprint to `Detail`; the volatile
recorders pin an explicit one (staleness: constant; version_skew: the version pair; stage_drift: the sorted
missing-key set). A dismissed entry therefore stays dismissed while its condition is unchanged.

### 4. A recurring condition costs nothing

`Record` skips the file write when title, detail, fix and fingerprint are unchanged and `LastSeen` is under
10 minutes old; `Clear` of an absent key does not write. A condition re-reported every poll yields one
entry and (almost) no I/O. The temp file is now unique per write (`os.CreateTemp`), so the daemon, the TUI
fix action and `fabrik upgrade` cannot clobber each other's partial write.

### 5. Many invalid settings → one entry

The ~60 `[warn] … is invalid; using default` lines in `cmd/root.go` still print one line each, but are
collected and recorded as a single `config_value:settings` entry listing every fallback (cleared on the
next start when all are valid). One badly broken configuration can no longer push the 3-row panel window
past a blocking entry.

### 6. Entries with no automatic fix

`FixAction == ""` is valid: the panel shows title and detail only, and the TUI's fix key is a no-op for
such an entry instead of the "unknown fix_action" error.

### 7. Amendments

- **ADR-1544** (unrecognised config keys: stderr only, no `warnings.Record`) is superseded in that scope:
  an unrecognised key is silently ignored, which is exactly the failure a loud surface exists for. It is
  now a `config_key:<key>` entry reconciled each start. The *check failing* remains log-only.
- Precedent for reversing a log-only decision: ADR-1347 chose a log line for "no write access"; ADR-1750
  reversed that for `repo_access`.

## Consequences

- New conditions in the skill-naming chain wire in with one `warnings.Report` call and a test each; the
  as-built doc states the contract.
- The panel stays flat (no severity field, no layout change): aggregation and keeping transient/per-item
  lines log-only limit overload. A severity ordering remains a possible follow-up.
- Test packages that can reach a recorder redirect the warnings file process-wide
  (`testenv.IsolateWarningsProcess`) so no `.fabrik/warnings.json` is written into a package directory.
- Out of scope: pushing entries over the MCP channel.
