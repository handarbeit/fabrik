# ADR 1610: Pruefer operator-only review cadence

**Status:** Accepted
**Date:** 2026-09-16
**Issue:** [#1610](https://github.com/handarbeit/fabrik/issues/1610)

## Context

Pruefer reviews a PR on every `opened`, `reopened`, `synchronize` (i.e. every push), and `ready_for_review` event, with no way for an operator to change that cadence. Per the existing `alreadyReviewedAtHead` guard (`pruefer/select.go`), the true current behavior is "one review per pushed head SHA," not "one review per commit" — a push of five commits still gets exactly one review. This issue adds an operator-configured cadence setting so an operator can reduce review noise/cost on a repo without changing what Pruefer looks for or how strict it is.

This issue was originally filed bundling three concerns. Two are already handled elsewhere: diligence/severity/path-exclusion configurability shipped via #1642 (repo-resident `.pruefer/config.yaml`, narrowing-only), and a custom review-guidance skill is fully specified by #1446, proceeding independently. This issue is scoped to cadence only.

## Decision

### Three cadence modes, resolved by `effectiveCadence`

`Config.Cadence` (global default) and `Config.RepoCadence` (per-repo override) recognize three values:

- **`every-push`** (default): one automatic review per pushed head SHA — today's behavior, unchanged (R2). An unconfigured deployment resolves to this byte-for-byte, since `LoadConfig` always fills `Cadence` with `DefaultCadence` before validation, and `effectiveCadence` additionally falls back to `DefaultCadence` for a `Config` built directly (e.g. in tests) with an empty `Cadence` field — so "unconfigured behaves as every-push" holds everywhere, not only at the CLI entry point.
- **`once`**: at most one automatic review per PR lifetime. The first eligible head triggers a review; subsequent pushes to the same PR do not trigger another automatic one. Reopening a PR does not reset this — it is the same PR lifetime.
- **`on-request`**: no automatic review is ever triggered by `opened`/`reopened`/`synchronize`/`ready_for_review`; only an explicit `/pruefer review` command reviews the PR.

`LoadConfig` fails loud at startup on any other value for either `cadence` or any `repo_cadence` entry — the same "operator typo is worth catching early" treatment `request_changes_threshold`/`review_guidance_mode` already get.

### Operator-only. Never repo-narrowable.

This is the load-bearing decision, and framing it as "narrowing" — the way ADR-1642 classifies `excluded_paths`/`max_diff_bytes`/`request_changes_threshold` — would be wrong.

ADR-1642's model is *repo may narrow, never widen*, where narrowing means narrowing **scope**: fewer paths, a smaller diff, a stricter severity tier. Scrutiny over what remains in scope is undiminished by any of those changes. Cadence is not scope — it is scrutiny **over time**, and reducing it is a widening of what can reach `main` unreviewed, not a narrowing of what gets looked at.

Concretely: a repo that could set `cadence: once` on itself would get reviewed at its first pushed state and could then push anything unreviewed afterward. That is precisely the disarm-the-reviewer risk ADR-1642's base-ref rule exists to prevent, reached by a different route the base-ref rule does not close — the setting would be legitimately read from the base ref and legitimately apply to every subsequent PR, so a single merged change to the base config would disarm the reviewer for everything after it. There is also a plainer argument: cadence spends the **operator's** Claude subscription, putting it alongside the other operator-only, cost-bearing keys (`model`, `effort`, `concurrency_cap`, `max_wall_time`).

Consequently, `cadence`/`repo_cadence` are **absent from `yamlRepoConfig`** (`pruefer/reporeconfig.go`) entirely, not merely rejected at merge time — mirroring every other operator-only field's treatment (`model`, `watched_repos`, ...). A repo-resident config has structurally nowhere for either key to land; an attempt surfaces only as a logged, ignored `RepoConfigProvenance.UnknownKeys` entry, exactly like a repo trying to set `model`. No new enforcement code was needed in `reporeconfig.go` — the schema itself is the enforcement.

### `once` semantics: a forced review also consumes the one-time quota

GitHub's PR review data records who reviewed and at what commit SHA, not *why* Pruefer submitted that review. There is no data-driven way to tell "the once-quota was spent by an automatic review" apart from "the once-quota was spent by a forced `/pruefer review`" after the fact, and no new local state is introduced to make that distinction (see below).

The decision: **a forced review consumes the `once` quota exactly like an automatic one would.** `once` means "has the bot reviewed this PR at all, by any trigger" — `select.go`'s `alreadyReviewedAtAll(reviews, botLogin)` answers this by scanning `ExistingReviews` for any review authored by `botLogin`, at any head SHA, with no head-SHA constraint at all (unlike `alreadyReviewedAtHead`, which `every-push` already relies on). This does **not** limit `/pruefer review` itself — R4's override always bypasses the cadence check on the review being requested, exactly like it already bypasses `alreadyReviewedAtHead` and the local `ReviewTracker` (#1631). It only means that *subsequent automatic* reviews stay silent afterward, same as if the forced review had been the PR's one automatic review.

This must be documented explicitly (here, and in `cmd/pruefer/README.md`) to prevent a future "I ran `/pruefer review` once and now it never reviews again" bug report — the behavior is intentional, not a defect in the guard.

### `once`'s state is fully re-derived from `FetchPRReviews` — no new persisted state

Unlike `#1631`'s `ReviewTracker` (which exists specifically to survive a `FetchPRReviews` response that comes back successful but wrongly empty during a GitHub degradation), `once`'s question — "has the bot reviewed this PR at all" — is exactly what the already-fetched `ExistingReviews` slice answers on every call, with no head-SHA constraint needed. ADR-1631 explicitly recommends against adding cadence-driven behavior to `ReviewTracker`; this ADR follows that recommendation rather than extending the tracker. The same GitHub-read risk ADR-1631 already documents (a degraded-but-successful `FetchPRReviews` response) applies here too — a spurious second automatic review under `once` is possible under the same conditions `every-push` is already exposed to — but the local tracker backstop still runs unconditionally, ahead of the cadence gate, so this is not a new or compounded risk.

### Two gate locations, chosen to avoid the poll/event-driven inconsistency trap

`pruefer/eventsink.go`'s `reviewTriggerActions` only exists in `event_source: hookdeck` mode. The default `event_source: poll` mode (`daemon.go`'s `poll()`) has no trigger-type concept at all — it calls `ReviewPR` unconditionally for every open PR on every poll cycle. A cadence gate placed anywhere near `reviewTriggerActions` would therefore be invisible to the default deployment mode. Both dispatch paths converge on `ReviewPR`, so that — and `Eligible()`, which it calls — is the only place a gate can be effective for every deployment shape:

- **`on-request`** is gated in `ReviewPR`, immediately after `forceReview` is resolved (`PendingForceReview`) — before even the local `ReviewTracker` check or `FetchPRReviews`. It needs nothing but the force-review boolean, so this is the cheapest possible cut point: a skip costs zero further GitHub calls.
- **`once`** is gated inside `Eligible()` (`pruefer/select.go`), ahead of the existing per-head-SHA check (`alreadyReviewedAtHead`), using `ExistingReviews` the caller has already fetched. If `once` and the PR has never been reviewed by the bot at all, `alreadyReviewedAtAll` reports false and the code falls through to the unchanged per-head-SHA check — which also reports false, so there is no behavioral divergence for a PR's first review.

### Per-repo override: a new, independent `repo_cadence` map, not a `watched_repos` field

The issue's own Scope section proposed attaching a per-repo cadence override to `watched_repos` entries. That would only work for operators who explicitly enumerate every repo they serve — exactly the pattern ADR-1641 made optional. Since #1641, `WatchedRepos` is merely an optional *narrowing filter* over installation-derived discovery: it can be empty (the recommended, common case), and a repo Pruefer actively reviews may have no `watched_repos` entry at all. Tying a cadence override to `watched_repos` would make it unreachable for exactly the deployments #1641 was designed to make simplest.

`RepoCadence map[string]string` is therefore a new, independent, YAML-only key (no flag/env, matching `hookdeck.*`/`reconciliation.*`'s own structured-and-occasional-use convention), keyed by exact `"owner/repo"` — the same no-fuzzy-matching convention `WatchedRepos` already uses. It works for any repo Pruefer reviews, named in `WatchedRepos` or not. `effectiveCadence(cfg, owner, repo)` resolves the per-repo override first, falling back to the global `Cadence` default, and further to `DefaultCadence` for direct `Config` construction (see above).

`Cadence`/`RepoCadence` are both tagged `reload:"live"`. `RepoCadence`, a `map[string]string`, needs no special-cased diff/apply logic the way `WatchedRepos` does (`diffRepos`) — the existing generic reflection loop in `applyConfigReload` already handles a map field correctly via `reflect.DeepEqual`, confirmed by a dedicated test rather than assumed.

## Consequences

**Positive:**
- An operator can reduce review noise/cost per repo (or globally) without touching what Pruefer looks for or how strict it is — a purely orthogonal axis to #1642's severity/path/author narrowing.
- The gate sits in the one place (`ReviewPR`/`Eligible`) both `poll` and `hookdeck` dispatch paths share, so cadence behaves identically regardless of deployment mode — there is no silent gap for poll-only operators, the majority deployment shape.
- `once`'s state needs no new persisted storage and inherits `ReviewTracker`'s existing degradation-resilience for free, since it reuses the same `ExistingReviews` fetch every other eligibility check already depends on.
- `/pruefer review` continues to work as an unconditional override under every mode — `once`/`on-request` reduce automatic review frequency, they do not provide a way to permanently silence review on a PR.
- `RepoCadence`'s independence from `WatchedRepos` means it composes cleanly with #1641's discovery model instead of fighting it.

**Negative / Trade-offs:**
- A forced review consuming the `once` quota is a real, documented surprise risk: an operator or contributor who runs `/pruefer review` once under `cadence: once` should expect no further automatic reviews on that PR. This is called out explicitly here and in the README specifically to head off a future "why did it stop reviewing" report.
- `RepoCadence`'s reload-summary rendering falls out of `fmt.Sprint`'s default map formatting (e.g. `map[owner/repo:once]`) rather than a dedicated per-repo diff like `WatchedRepos` gets from `diffRepos` — acceptable for a setting expected to change rarely; revisit only if operators report confusion.
- `once`'s "any SHA, any trigger" derivation depends on `FetchPRReviews` being complete, the same dependency `every-push`'s per-head check already has (see ADR-1631) — not a new risk, not compounded by this change, since the local tracker backstop still runs first regardless of cadence.

## Related Work

- `adrs/1642-pruefer-repo-resident-config-base-ref.md` — the narrowing-only, may/may-not classification model this ADR explicitly does *not* extend to cadence; cadence is scrutiny over time, not scope, and is excluded from the repo-resident schema entirely rather than merely defaulting to "reject as repo input."
- `adrs/1631-pruefer-local-review-tracker-backstop.md` — establishes the "have I already reviewed this" precedent `once` builds on, and explicitly recommends against adding cadence-driven behavior to `ReviewTracker` itself — followed here by deriving `once`'s state from `FetchPRReviews` instead.
- `adrs/1641-pruefer-installation-derived-repo-discovery.md` — turned `WatchedRepos` into an optional narrowing filter rather than an authoritative repo registry, which is why `RepoCadence` is a new independent map instead of a `WatchedRepos` entry field.
- `adrs/1640-pruefer-config-reload.md` — the `reload:"live|restart|skip"` classification and reflection-based `applyConfigReload` loop both new fields plug into with no special-casing required.
- #1446 (repo-resident review-guidance skill) — answers a different question ("what the review says" vs. this issue's "how often it happens"); no dependency in either direction, since #1446 doesn't touch cadence and this issue's setting is entirely operator-only.
