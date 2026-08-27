# ADR 1642: Pruefer repo-resident `.pruefer/config.yaml`, resolved at the base ref

**Status:** Accepted
**Date:** 2026-08-27
**Issue:** [#1642](https://github.com/handarbeit/fabrik/issues/1642)

## Context

Every setting governing how Pruefer reviews a repo has, until now, lived exclusively in the operator's own `.pruefer/config.yaml` — a file on the machine running the daemon. The repo being reviewed has had no say at all: it cannot exclude its own generated paths, cap its own diff size, or opt into a stricter severity threshold. That is backwards for settings whose right value is a property of the *repository*, not of the operator running the daemon — and it does not scale with #1641's installation-derived repo discovery (ADR-1641): once the reviewed-repo set is derived from which repos the GitHub App is installed on, a repo joins by being installed on, with no config file of its own to express a preference.

This issue introduces a second, repo-resident `.pruefer/config.yaml` — committed at the root of the *reviewed* repo, not the operator's daemon working directory — that a repo's own authors can use to narrow (never widen) how Pruefer reviews their PRs.

### The security constraint is the same one ADR-1113 already established

`buildReviewArgs` (`pruefer/claude.go:316-338`) already passes `--setting-sources user` specifically because a reviewed repo's own `.claude/settings.json` "come[s] from code that has not been reviewed yet" and could otherwise widen the reviewer's own tool grants. A repo-resident `.pruefer/config.yaml` is exactly the same class of untrusted input — it comes from the artifact under review — and the identical rule must apply: **a PR must never be able to change how it is itself reviewed.**

Concretely: if Pruefer resolved repo config at the PR's *head*, a PR could loosen `request_changes_threshold`, add its own touched paths to `excluded_paths`, or raise `max_diff_bytes` on the very change that most needs scrutiny — and with ADR-1251's severity-gated `REQUEST_CHANGES` live, disarming the reviewer this way would suppress a blocking verdict on exactly the PR trying to sneak something past review.

### Direction of "narrowing" for `request_changes_threshold`

`severityRank` (`pruefer/severity.go`) ranks `low(1) < medium(2) < high(3) < critical(4)`, and `decideEvent` (`pruefer/review.go`) fires `REQUEST_CHANGES` when a finding's rank is `>=` the threshold's rank. A **lower** threshold value is therefore **stricter** (more findings qualify) — the narrowing direction a repo must be allowed to move toward. A **higher** value, or unsetting an operator-configured threshold back to `""` (off — the most lenient possible state), is the widening direction that must be rejected. This is the opposite of what a naive reading of "lower number sounds more restrictive" might suggest, and is easy to get backwards — see the "narrowingRank" discussion below.

## Decision

### A repo-resident config narrows a strict, explicit allow-list of fields — never the full config schema

`pruefer/reporeconfig.go` introduces `yamlRepoConfig`, a standalone struct with exactly five fields: `excluded_paths`, `excluded_labels`, `excluded_authors`, `max_diff_bytes`, `request_changes_threshold`. This is deliberately **not** a reuse of `yamlConfig` (the operator's own on-disk shape). Reusing `yamlConfig` would make every operator-only field (`model`, `github_app_id`, `watched_repos`, #1641's `max_derived_repos`/`repo_rederivation_interval`, …) structurally *parseable* from repo YAML even if downstream code chose to ignore it — a weaker property than what this issue requires. With a dedicated struct, an operator-scoped key has nowhere to land at all; the classification is enforced by the schema itself, not by a convention some future change could silently erode.

An operator-scoped or otherwise-unrecognized key present in a repo's file is detected via a second, map-shaped YAML parse (diffed against a fixed recognized-key set) purely for diagnostic purposes — `RepoConfigProvenance.UnknownKeys` — and is always ignored with a logged warning, never an error and never a value that reaches `Config`.

### Merge is narrowing-only, enforced per field in code

`applyRepoNarrowing(operator Config, repo yamlRepoConfig) (Config, []narrowingWarning)` implements exactly five rules:

- `excluded_paths` / `excluded_labels` / `excluded_authors`: **union** — a repo's entries are added to the operator's list; a repo can never remove an operator entry.
- `max_diff_bytes`: a repo may only set a value **lower than or equal to** the operator's (or narrow from an uncapped operator to any positive cap). Requesting a higher value, or uncapping when the operator has a real cap, is rejected.
- `request_changes_threshold`: a repo may only move to a value with a **lower-or-equal severity rank** than the operator's — including turning the gate on when the operator left it off. Raising the rank, or unsetting an operator-configured threshold back to off, is rejected.

Every rejection is returned as a `narrowingWarning` rather than silently dropped — logged once per review by `logRepoConfigResolution` (R5), alongside which repo config (if any) was applied and at which base SHA, since that provenance is itself the security-relevant fact this ADR exists to establish.

#### `narrowingRank`, not `severityRank`, governs the threshold comparison

`severityRank("")` returns `0` (least severe) — correct for its own purpose, a fail-closed per-finding comparison in `decideEvent` where an untrusted or missing per-finding severity must never be trusted to escalate toward `REQUEST_CHANGES`. Reusing that ranking for the narrowing-direction comparison would have silently re-inverted the exact bug this issue's spec had to correct during Specify: `""` (off) must rank as the *most lenient* state for narrowing purposes — above `critical`, not below `low`. `pruefer/reporeconfig.go` therefore introduces a separate `narrowingRank(s Severity) int` (`"" → 5`, else `severityRank(s)`) with a doc comment explicitly cross-referencing `severityRank` and warning against unifying the two. **Do not merge these two functions** — they encode opposite directions for the same input on purpose.

### Resolution: a generic Contents-API-at-ref primitive, shared with #1446

`github.Client.FetchFileAtRef(owner, repo, path, ref string) ([]byte, error)` (`github/contents.go`) is a new, package-agnostic primitive: a plain GitHub Contents API read at an explicit ref, base64-decoded, with 404 mapped to the existing `github.ErrNotFound` sentinel. It has no awareness of YAML, Pruefer, or any caller's semantics — it exists specifically so #1446 (repo-resident review-guidance skill, resolved at the same base ref, still open with no PR as of this ADR) can reuse it rather than building a second, parallel base-ref-fetch mechanism. `DefaultConfigPath` (`.pruefer/config.yaml`) is reused unchanged as the lookup path — the resolution *mechanism* is what's shared with #1446, not necessarily the path itself, since #1446's skill lives under a different sub-path.

`pruefer.GitHubReviewer` gains `FetchFileAtRef` as a required interface method (not type-asserted), so the base-ref security property is structural for every `GitHubReviewer` implementation — including test fakes — rather than an opt-in a future implementation could accidentally skip.

### Fetch failures degrade to operator config, never fail the review

`fetchRepoConfig` never returns an error. `gh.ErrNotFound` (no `.pruefer/config.yaml` at the base ref — the overwhelmingly common case, R3) returns a zero-value config with no warning at all — not even a compliance-drift signal, since it's the default state every repo starts in. Any other failure — a genuine fetch error, a file over `DefaultMaxRepoConfigBytes` (32 KB — generous for a handful of glob patterns and a severity tier, far below `DefaultMaxDiffBytes`'s 500 KB governing a full unified diff), invalid UTF-8, or unparseable YAML — degrades to the same zero-value fallback with a logged warning (R4). A repo can never stop its own reviews by committing a broken config file.

### Resolution happens before `Eligible()`, reordering `ReviewPR`

`ReviewPR`'s `EligibilityInput` is built from `cfg.ExcludedAuthors`/`cfg.ExcludedLabels` and evaluated before any diff fetch. Since repo-narrowed exclusions must take effect for eligibility too — not just for the size/threshold gates further down — repo config resolution is now the **first** network call in `ReviewPR`, ahead of even `PendingForceReview`/`FetchPRReviews`. Everything below that point reads the resulting effective `cfg` exactly where it read the operator's `cfg` before this issue; no other ordering invariant in `ReviewPR` (e.g., "cheap checks before any diff fetch") is affected, since the repo-config fetch is not a diff fetch.

## Consequences

**Positive:**
- A repo can express review preferences appropriate to itself (excluding its own generated/vendored paths, capping its own diff size, opting into a stricter severity gate) without operator involvement — closing the gap #1641's installation-derived discovery opened (a repo joining by installation has no other channel to express a preference).
- The narrowing-only, per-field-classified merge makes "a reviewed repo can never widen its own review" a property enforced in code (an explicit allow-list plus a narrowing check per field), not a convention.
- Resolution shares one mechanism (`FetchFileAtRef`) and one location convention (`DefaultConfigPath`) with #1446 rather than duplicating a base-ref-fetch primitive — whichever of the two issues lands second adopts the first's plumbing (this one, as of this ADR, since #1446 has no PR yet).
- Existing `review_test.go`/`daemon_test.go` suites pass unmodified with the new interface method's default fake behavior (`gh.ErrNotFound`), structurally confirming byte-identical behavior when no repo config exists (R3/AC5).

**Negative / Trade-offs:**
- `ReviewPR` now makes one additional network call (the Contents API read) on every review, even when no repo config will ever be found — an unavoidable cost of resolving before `Eligible()`, and small relative to the diff fetch and Claude invocation that follow.
- The reordering means a repo-resident config that changes eligibility (excluded authors/labels) takes effect one call earlier than the size/threshold fields conceptually would need to — this is intentional (see above), not an oversight, but is a real behavioral coupling worth remembering if `ReviewPR` is refactored later.
- A repo committing a config file it believes takes effect immediately for the same PR will be surprised that it doesn't — this is the base-ref rule's explicit, documented trade-off (see `cmd/pruefer/README.md`'s "Repo-resident config" section), not a bug.

## Related Work

- `adrs/1113-pruefer-v1-architecture.md` — origin of `--setting-sources user` and the "the PR head is untrusted input" doctrine this issue extends to a second file (`.pruefer/config.yaml` alongside `.claude/settings.json`).
- `adrs/1251-pruefer-severity-gated-request-changes.md` — defines `RequestChangesThreshold`/`severityRank`/`decideEvent`, whose existing ordering this issue's `narrowingRank` deliberately does *not* reuse (see above); that ADR's "Pruefer has no per-repo config override mechanism... out of scope here" line is superseded by this issue for `request_changes_threshold`, `max_diff_bytes`, and the `excluded_*` fields only — every other setting that ADR and this repo's `Config` struct define remains exclusively operator-scoped.
- `adrs/1427-pruefer-diff-too-large-degrade-not-block.md` and `adrs/1462-pruefer-per-file-diff-exclusion-and-trim.md` — establish the `excluded_paths`/`max_diff_bytes` gate ordering and semantics this issue does not change; it only changes where the *values* feeding those gates come from.
- `adrs/1640-pruefer-config-reload.md` — SIGHUP live-reload of the *operator's own* config, a daemon-lifetime concept orthogonal to this issue's per-review resolution; a repo-resident config is re-resolved fresh on every review regardless of any reload cycle, and reload's `reload:"live"/"restart"` field tags are not consulted for repo-narrowing at all.
- #1446 (repo-resident review-guidance skill, resolved at the base ref) — still open, no PR, as of this ADR. `FetchFileAtRef` (`github/contents.go`) is built generically so #1446 can reuse it rather than building a parallel Contents-API-at-ref primitive; #1446's ADR, when it lands, should reference this one.
- #1641 / `adrs/1641-*.md` — installation-derived repo discovery, the change that made a repo-resident config valuable in the first place (a repo joins by installation, with no other channel to express a preference); introduced `max_derived_repos`/`repo_rederivation_interval`, both correctly classified as operator-scoped (repo discovery and resource-management knobs) and un-settable from a repo-resident config.
