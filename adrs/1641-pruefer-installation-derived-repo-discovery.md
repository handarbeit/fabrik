# ADR 1641: Installation-Derived Repo Discovery

**Date**: 2026-08-27
**Status**: Accepted
**Issue**: #1641 — derive the watched repo set from App installations instead of a hand-maintained list

## Context

Which repos Pruefer reviews was decided by a hand-maintained list, `watched_repos` in
`.pruefer/config.yaml`. That list had to be kept in sync by hand with a second source
of truth — where the GitHub App is actually installed — and the two drifted silently:
installing the App on a new repo did nothing until an operator remembered to edit the
config and restart or reload.

Until #1253 merged, `watched_repos` was not merely a convenience list — it was the
**containment boundary**. Pruefer's GitHub App was, at the time, shared and public
(`handarbeit-pruefer`, installable by anyone); ADR-1233 made the set of installations
Pruefer ever tokenized equal exactly the set of owners named in `watched_repos`,
structurally, so a stranger installing the public App on their own account was never
contacted — their owner never appeared in this operator's config. Inverting the model
(install-derived discovery) would have removed that protection by construction: a
stranger's installation would have enlisted *this operator's* daemon.

#1253 changed the premise: each operator now bootstraps and authenticates as their
own dedicated App via the manifest flow (`internal/githubauth.Reconciler`), superseding
the shared-App model entirely. "Installed on it" and "mine to review" became the same
statement for every operator, unblocking this issue.

## Decisions

### 1. `Reconciler.Derive` becomes the single source of truth for "which repos does Pruefer review"

`internal/githubauth/derive.go` (new) adds `Reconciler.Derive(ctx, filter []string,
maxRepos int, logf) (DerivedRepoSet, []DetachedAuth, error)`, replacing `Reconcile`'s
former owner-scoped discovery loop (which only ever iterated owners already present in
`watched_repos`). `Derive` instead:

1. Calls `gh.FetchAppInstallations` with **no owner list as input** — every installation
   of the operator's own App, discovered unconditionally.
2. For each installation not already known (by lower-cased account), mints a token
   (`mintAuth`) and registers it via `registerOwnerAuth` (a helper factored out of
   `CommitOwnerAuth`'s registration bookkeeping — see below for why `Derive` doesn't
   call `CommitOwnerAuth` itself). An *already*-known owner's existing client/`Auth`
   is reused unchanged — no re-mint — so a repo gained or lost under an already-known
   installation never disturbs that installation's running refresh loop.
   Whether the newly-registered `Auth`'s refresh loop starts immediately depends on
   an unexported `startLoopsForNewOwners` parameter: true for every public `Derive`
   call (an installation webhook event, the periodic ticker, or a SIGHUP
   `watched_repos` edit — nothing else will ever start a loop for an owner one of
   those newly discovers), but false specifically for `Reconcile`'s own first,
   internal call. That distinction exists because `Reconcile`'s caller
   (`pruefer/execute.go`) always calls `Reconciler.RunRefreshLoops` itself immediately
   after `Reconcile` returns, to start every discovered owner's refresh loop as one
   batch — if `Derive` had also started a loop right away (via `CommitOwnerAuth`, as
   an earlier revision of this change did), every installation would get **two**
   independent refresh-loop goroutines: `Auth.startRefreshLoop`'s `cancel` field holds
   only the most recent loop's cancel func, so the second start silently orphans the
   first — un-stoppable by `Auth.Stop`/`drainThenStopAuth`/`RemoveOwners` short of a
   process restart — while also doubling every installation's token-mint API traffic.
   Caught by a data race in the existing test suite (two independently-running,
   `context.Background()`-rooted refresh loops touching shared test state) and fixed
   during this issue's own review; see
   `TestReconcile_InitialDiscovery_DoesNotDoubleStartRefreshLoops`.
3. Calls `gh.FetchInstallationRepositories` for **every** installation, regardless of
   `repository_selection` — that endpoint already returns the full accessible set for
   `"all"`-mode installations too; the pre-#1641 code only called it for `"selected"`
   mode because it only ever needed a watched-repo verification, not full enumeration.
   Calling it unconditionally is what makes org-level and account-level derivation
   (AC2) work with no special-casing, and is also what "future repos become pollable
   with no restart" (AC2/AC3) requires: this call is never cached, only ever a live
   re-fetch, on every `Derive` call.
4. Unions every installation's repos into the **installation grant**.
5. If `filter` (the caller's `watched_repos`) is non-empty, intersects it in — **never
   widening** beyond the grant — and reports (via `DerivedRepoSet.FilteredOut`) any
   filter entry the grant doesn't cover, rather than silently dropping or including it
   (R3/AC4).
6. Sorts the result deterministically (lower-cased `owner/repo` ascending) and, if
   `maxRepos > 0` and the result exceeds it, caps it there (R5) — same repos dropped
   consistently across calls, not an arbitrary API-order cut.
7. Detects owners that had a client before this call but no longer have a matching
   installation, and detaches them via the existing `RemoveOwners` (returned as
   `[]DetachedAuth` for the caller to drain-then-stop, exactly `RemoveOwners`' existing
   contract).

`Reconcile`'s own (first-run) call and every later re-derivation trigger (R2) call the
same `Derive` method — it is safe to call repeatedly specifically because steps 2–3
above never trust a cache.

**Pinned-installation mode (`AppInstallationID != 0`) is fully exempt** — `Derive`
short-circuits to `derivedSetForPinned`, building a `DerivedRepoSet` directly from
`filter` with the pinned installation ID attributed to every entry, preserving
ADR-1233 Decision 4/ADR-1253 Decision 6's byte-for-byte compat guarantee. There is
nothing to discover when the operator has already asserted one installation covers
everything.

### 2. `watched_repos` stays the config key; semantics invert to an optional intersection filter (R3)

Renaming was considered and rejected: the field is deeply threaded through `Config`,
`yamlConfig`, `reload:"live"` tagging, `TestConfig_AllFieldsClassified`, and the
README. Keeping the name with redefined, clearly-documented semantics — absent means
"everything the installations grant"; present means "the intersection, never wider" —
was judged materially lower-risk than a rename-plus-migration for the same outcome.

### 3. `FetchAppInstallations`/`FetchInstallationRepositories` gain real pagination

Both `github/app.go` functions previously capped at 100 results (GitHub's max
`per_page`) with only a package-level log line noting the possible truncation —
adequate when these were a verification check against a short, operator-typed list
(ADR-1233 Decision 6 explicitly deferred fixing this). Once installation enumeration
became the *primary* discovery path, silently under-deriving past 100 items became a
materially worse failure mode: previously a missed entry mis-reported one
`watched_repos` line as "not authorized" (a visible, per-repo symptom); post-#1641 the
same gap would have silently omitted repos from the derived set with no watched_repos
entry ever having flagged the discrepancy.

Both functions now follow real page-number pagination (`?page=N`, not the Link
header — every caller already builds page-numbered URLs directly) up to a fixed
50-page ceiling (≈5,000 items) — a sanity bound distinct from R5's own operator-facing
cap, guarding only against an unbounded loop against a pathological or misbehaving
server. Both return a `truncated bool` alongside their result, used to warn (not
silently under-derive) when even that ceiling is hit.

### 4. R5's cap (`max_derived_repos`, default 200) is a separate knob from pagination

Applied last, after the full union and any `watched_repos` filter — over the same
deterministic sort pagination itself doesn't need but capping does, so the same repos
are dropped consistently across re-derivations. `<= 0` disables the cap entirely (an
explicit operator opt-out, not the default). A loud warning is logged
(`derived N repo(s)... (capped from M by max_derived_repos=200)`) and surfaced via
`DerivedRepoSetEvent.Capped`/`CapApplied` in the TUI. Raising the cap or narrowing via
`watched_repos` are both documented as the operator's ways out.

### 5. Derived state lives on `Daemon`, not `Config`

`Daemon.derived githubauth.DerivedRepoSet` (guarded by the existing `cfgMu`, alongside
`Config`/`Clients`) holds the result of the most recent `rederiveRepos` call — this is
what `poll()` and `isWatchedRepo` (`eventsink.go`) now read, replacing their direct
`Config.WatchedRepos` reads. Putting it on `Config` instead would have made
`applyConfigReload`'s generic per-field reflection loop compare it against
`LoadConfig`'s always-empty candidate value on every SIGHUP, incorrectly
reporting/clobbering a field no operator ever sets directly.

`ApplyDerivedRepos(set, clients)` — the write side, mirroring `ApplyReload`'s existing
concurrency-safety shape — rebuilds `Daemon.Clients` **wholesale** from the newly
derived installation set, rather than diffing added/removed clients incrementally.
Installation counts for one operator's own dedicated App are small (their own
orgs/repos, not a shared-App fleet), so a full rebuild on every re-derivation is cheap
and removes an entire class of incremental-diff bugs.

### 6. One uniform re-derivation ticker for both poll-mode and event-driven mode (R2)

`Daemon.runRederivationTicker`, driven by `Config.RepoRederivationInterval` (default
10 minutes), is started unconditionally in `Daemon.Run` — before either
`runPollOnly` or `runEventDriven` — rather than as a mode-specific mechanism.
Poll-mode had zero installation-change awareness at all before this issue and needed
one from scratch; event-driven mode already had `ReconciliationFallbackInterval` as a
low-frequency PR-listing safety net, but growing that ticker a second, differently-scoped
responsibility (re-derivation, not just re-polling) was judged more confusing than a
second, purpose-built ticker.

The `installation`/`installation_repositories` webhook event
(`pruefer/eventsink.go`'s `installEventTypes`) and the ticker both converge on the
same `Daemon.triggerRederivation` entry point — a `CompareAndSwap`-guarded coalescing
wrapper (mirroring `triggerReconciliationPoll`'s existing shape) around
`rederiveRepos`, which itself chains into `triggerReconciliationPoll` afterward so a
newly-derived repo becomes pollable in the same cycle (AC3), not only on the next
fallback tick.

### 7. SIGHUP `watched_repos` changes no longer mint or remove any owner's auth

Since `Derive` now mints every installation unconditionally (Decision 1), a SIGHUP
`watched_repos` edit (`pruefer/execute.go`'s `handleReload`) no longer needs the
two-phase `MintOwnerAuth`/`CommitOwnerAuth` mint-then-commit or the
`RemoveOwners`/`drainThenStopAuth` detach-then-drain sequence ADR-1253's 2026-08-26
addendum built for exactly this trigger. `handleReload` now only applies the merged
`Config` (`ApplyReload(merged, nil, nil)`) and, if `watched_repos` or
`max_derived_repos` changed, calls `daemon.triggerRederivation` — the same
re-derivation path an installation webhook event or the ticker uses. This does still
make a live call to re-list each installation's accessible repos (not a purely local
re-intersection against a cached grant), since `Derive`'s own contract is "always
re-fetch, never trust a cache" — accepted as a reasonable cost for a rare,
operator-initiated event, and it reuses the exact same machinery as every other
re-derivation trigger rather than a bespoke local-only path.

`MintOwnerAuth` and `internal/githubauth/installations.go`'s `verifyRepoAccess` have
no remaining production caller after this change. They are **deliberately retained**
rather than deleted in this same change — both still compile, are still exercised by
their own existing unit tests, and remain a correct, documented (if now-unused)
mechanism. Full removal was judged a separable cleanup not worth the additional
churn/review-diff size in a change already touching this much surface area; see
Consequences below.

### 8. Containment (AC7) is structural, not an added check

`FetchAppInstallations` is JWT-scoped to the calling App's own identity by GitHub's
own API contract — it is physically incapable of returning another App's
installations. No owner-allowlist, denylist, or "trusted accounts" list was
introduced to compensate, per the issue's own explicit instruction not to
reintroduce the hand-maintained list this issue exists to remove while looking like
it has been removed. The regression test
(`TestDerive_Containment_NeverContactsOtherAppsInstallations`,
`internal/githubauth/derive_test.go`) simulates two distinct App identities against
one fake server, differentiated by the JWT's `iss` claim, and asserts a `Reconciler`
built from App A's own key never sees or contacts App B's installations or mints a
token against them — proving the property structurally rather than by convention.

### 9. R4 observability: log lines plus a TUI `DerivedRepoSetEvent`

Every `Derive` call's result is logged (`logRederivedRepos`, `pruefer/daemon.go`): one
line per installation found (account, `repository_selection`, repo count), a summary
line (total repos derived, installation count, capped/truncated warnings), and one
line per `watched_repos` entry the grant doesn't cover. The same result is re-expressed
as a TUI event — `ptui.DerivedRepoSetEvent` carrying `[]DerivedRepoEntry{Repo,
InstallationID}` (per-repo provenance) and `[]DerivedInstallationSummary`
(per-installation aggregate), kept as plain, dependency-free types so `pruefer/tui`
never imports `internal/githubauth` (see ADR-1253's Decision 8 amendment). The repos
pane (`RepoPaneComponent`) re-seeds itself from this event — adding newly-derived
repos, dropping ones no longer granted, and preserving poll status (last poll time, PR
count, last error) for every repo still present — rather than a naive full replace,
which would have discarded in-flight poll state on every re-derivation tick.

## Consequences

**Positive:**
- Installing the App on a new repo, org, or account is picked up automatically — no
  config edit, no restart, satisfying AC1/AC2/AC3 directly.
- `watched_repos` becomes genuinely optional operator preference (R3), no longer a
  required, hand-maintained, silently-drifting list.
- The pagination gap flagged as a known, deferred limitation in ADR-1233 Decision 6 is
  finally addressed, specifically because it became a correctness issue (not just an
  inert gap) once discovery, not verification, depended on it.
- R5's cap and R4's observability ship in the same change as the discovery inversion,
  rather than as follow-ups — directly mitigating the issue's own top-flagged risk (an
  operator's effective review set silently expanding the moment this ships).

**Negative / Trade-offs:**
- **Behavior change for existing deployments**: an operator currently relying on
  `watched_repos` to narrow a broad `"all"`-mode installation sees their effective set
  *expand* to everything the installation grants the moment this ships, unless they
  already relied on `watched_repos`' new intersection semantics being correct (they
  are, and are covered by AC4's own test coverage) — this is the deliberate, accepted
  outcome of R1, not an oversight; R4's visibility exists specifically so this
  expansion is never silent.
- **SIGHUP `watched_repos` reload now makes a live GitHub call** (re-listing every
  installation's accessible repos via `Derive`) rather than a purely local
  re-intersection against an already-known grant, as the original Plan for this issue
  briefly considered. Judged an acceptable cost for a rare, operator-initiated event
  in exchange for reusing one re-derivation code path everywhere, instead of a second,
  divergent one for this trigger alone.
- **`MintOwnerAuth`/`verifyRepoAccess` are dead code as of this change** (see Decision
  7) — a real, if scoped and explicitly-noted, deviation from immediately deleting
  now-unused code. A future cleanup issue can remove them (and their dedicated tests)
  once this change has soaked.
- **`Derive`'s per-installation enumeration cost scales with installation count**,
  each re-derivation trigger requiring a `FetchInstallationRepositories` call per
  known installation (plus a mint for any newly-discovered one). Small for one
  operator's own dedicated App; a misconfigured `repo_rederivation_interval` set very
  low could still add unnecessary GitHub API load — documented with its default and
  rationale in `cmd/pruefer/README.md`.
- **`ApplyDerivedRepos`'s wholesale `Clients` rebuild** briefly replaces the map under
  the `cfgMu` write lock rather than diffing it — a reader racing exactly that window
  sees a fully-swapped map, never a partially-updated one (the same guarantee
  `ApplyReload` already provides for its own incremental merge), covered by an
  explicit concurrency test (`TestDaemon_ConcurrentReloadDuringActivePoll`) exercising
  both write paths against an active poll cycle simultaneously under `-race`.

## Related Work

- ADR-1233 (`adrs/1233-pruefer-multi-installation-auth.md`) — the
  owner-derived-from-`watched_repos` discovery direction this issue reverses; amended
  in the same change to mark Decision 1's security framing superseded while noting
  Decisions 2–4/6 retained and reused.
- ADR-1253 (`adrs/1253-github-app-manifest-auth-reconciler.md`) — the per-operator App
  reconciler (`internal/githubauth.Reconciler`) this issue's `Derive` method extends,
  and the prerequisite (#1253) that made installation-derived discovery safe in the
  first place; amended in the same change (Decision 8's narrow TUI exception, and a
  new addendum describing the SIGHUP-reload behavior change).
- ADR-1640 (`adrs/1640-pruefer-config-reload.md`) — the `cfgMu`/`config()`/`client()`/
  `ApplyReload` concurrency boundary and "diff at the right granularity, apply
  atomically, log a summary" discipline this issue's re-derivation machinery reuses
  (`ApplyDerivedRepos` alongside `ApplyReload`, not replacing it).
- #1428, #1563 — prior "silent state change with no record" failure modes R4's
  log-plus-TUI observability convention is modeled on.
- `cmd/pruefer/README.md` — Setup/config-reference/TUI sections rewritten in the same
  change to document the inverted model, the new `max_derived_repos`/
  `repo_rederivation_interval` config keys, and the new per-repo installation
  provenance the TUI now shows.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
