# ADR 1952: Pruefer idle polling scales with changes, not open-PR count

**Status:** Accepted
**Date:** 2026-09-29
**Issue:** [#1952](https://github.com/handarbeit/fabrik/issues/1952)

## Context

Every `poll_interval_seconds` (120s by default), Pruefer lists each derived repo's open PRs and hands every one to `ReviewPR`. Before it can decide *not* to review a PR, `ReviewPR` makes three reads: the repo-resident `.pruefer/config.yaml` at the PR's base ref (#1642, deliberately the first call), the issue comments (looking for `/pruefer review`), and the PR's reviews (has the bot already reviewed this head?). The in-process `ReviewTracker` (#1631) short-circuits the last one, but only for heads reviewed during this process's lifetime and only after the first two calls have already been paid. PRs skipped for any other reason (draft, excluded author/label/path, cadence) never enter the tracker at all.

So idle cost scaled with **open PRs × poll frequency**, not with activity. On the `verveguy` installation (11 watched repos, one shared REST budget) that was roughly 2,400 log lines and 3 reviews in the hour before the budget ran out. There was no conditional-request support anywhere in `github/`, and `ListOpenPRs` discarded each PR's `updated_at`.

## Decision

Two complementary mechanisms. Neither may cause a missed review.

### 1. A per-PR "conclusively evaluated" memo, checked before dispatch (R1)

`prMemo` (`pruefer/pr_memo.go`) holds, per `(owner, repo, PR number)`, the head SHA and a `prStamp` at which the last evaluation reached a *conclusive* outcome. `Daemon.poll` consults it for each listed PR **before** `reviewOne`, so a hit costs no semaphore slot, goroutine, PR gate, or API call. `ListOpenPRs` now surfaces `updated_at` (`PRDetails.UpdatedAt`, kept verbatim and compared for equality only).

A hit requires the head SHA **and** the whole stamp to match:

| Stamp field | Covers |
|---|---|
| `UpdatedAt` (from the listing that started the evaluation) | pushes, comments (so a pending `/pruefer review`), reviews, label changes, draft/ready transitions |
| `OpGen` (operator-config generation, bumped by `ApplyReload`) | `excluded_authors/labels/paths`, `max_diff_bytes`, `cadence`, `repo_cadence` — anything an operator reload can change without a PR moving |
| `RepoCfg` (fingerprint of `.pruefer/config.yaml` at the PR's base ref) | repo-resident narrowing, which changes when the *base branch* does, not when a PR does |

Any unknown component — empty head, empty `updated_at`, or a repo-config fingerprint of `""` (a fetch error) — is never recorded and never matched: the PR is evaluated.

**Why a generation counter rather than a fingerprint of the skip-relevant fields.** It is conservative (one re-evaluation sweep per reload) and cannot silently go stale when a future skip input is added. A reload is rare; a missed review is not acceptable.

**Repo-config fingerprint cost.** It is resolved once per `(repo, base ref)` per poll — a sha256 of the file bytes, `"absent"` on 404, `""` on any other error — so the per-PR config read becomes a per-repo read (and a free `304` when the file exists). The stamp is taken before evaluation and the evaluation re-reads the config itself; a change in between only makes the stored stamp *older* than the config used, so the next poll re-evaluates. Every race in this design resolves in that direction.

**`updated_at` timing.** The stored value is the one from the listing that started the evaluation, never re-read afterwards. A comment arriving mid-evaluation therefore leaves the memo stale-by-construction, and the next poll re-evaluates. Pruefer's own review submission bumps `updated_at`, so each review costs exactly one extra evaluation (a cheap "already reviewed" skip, after which the PR is memoised again). This is bounded per review, not per poll, and accepted.

**What may be memoised: `ReviewOutcome.Conclusive`.** `ReviewPR` reports `Conclusive = !degraded && Err == nil && (Reviewed || Skipped)`, computed in one deferred place so a future return site that forgets to think about it is non-conclusive by construction. `degraded` is set wherever `ReviewPR` deliberately tolerates a failure that could have shaped the outcome, none of which surfaces in `Err`:

- `PendingForceReview` failing (treated as "no forced review" — a cadence `on-request` skip made on that basis looks conclusive but is not);
- the repo-config fetch failing on transport (`RepoConfigProvenance.FetchFailed`, distinct from a deterministic *invalid* file, which is conclusive);
- the `FetchPRFiles` fallback failing after a `too_large` 406 (the 406 is deterministic; the fallback failure is not);
- a diff-unavailable or diff-too-large notice failing to post (nothing would retry it);
- `AcknowledgeForceReview` or `MarkForceReviewsProcessed` failing (the `/pruefer review` comment would stay pending).

A non-conclusive outcome also *forgets* any older entry for that PR. Advisory reads (review threads, repo guidance) are not degradation: a review submitted without them is still a review at that head.

**Records only in `executeReview`**, under the per-PR gate, from the poll path. The event path (`ReviewFromEvent`) passes a zero stamp: it neither consults nor records the memo (it has no listing to stamp from; any PR it evaluates has a changed `updated_at`, so the next fallback poll re-evaluates it once).

**Eviction and failed reads.** After a *successful* `ListOpenPRs`, entries for PRs absent from the list are dropped; at the end of each poll, entries for repos no longer in the derived set are dropped. A failed listing neither records nor evicts — absence from a failed read is not absence from GitHub. Unlike `ReviewTracker`, the memo is bounded.

**Not stored locally (ADR-1113).** The memo is in-memory, process-lifetime, and additive in exactly the way ADR-1631's tracker is: a restart clears it and Pruefer falls back to today's behavior, re-evaluating every open PR once. A cold start never causes a review storm, because the GitHub-derived and tracker checks inside `ReviewPR` are untouched and still decide whether a review happens.

### 2. Conditional requests where reads remain (R2)

`Client.EnableConditionalRequests()` turns on `If-None-Match` for the typed REST reads that go through `condGetJSON`: every page of `paginateREST` (so `ListOpenPRs`, `FetchIssueComments`, `FetchPRReviews`) and `FetchFileAtRef`. GitHub does not count a `304` against the primary rate limit.

- **Opt-in per `*Client`.** Only `internal/githubauth.mintAuth` — Pruefer's per-installation clients — calls it. The Fabrik engine's clients never do, so the engine's read path is byte-for-byte unchanged; `doWithAccept` is a thin wrapper over the new `doWithHeaders` passing no extra headers.
- **Exact-URL keys.** An entry is reused only on a `304` for the same request URL (and `Accept`), and a `200` always replaces it. A `304` for a request that sent no `If-None-Match` is an error, never an empty result.
- **Per-page, not per-collection.** Every page is still requested every time, each with its own ETag, so a `304` on page 1 cannot hide a change on page 2. Cached chunks are copied into the accumulator and never mutated or returned directly; `FetchPRReviews`' latest-per-author collapse re-runs over the cached pages.
- **Decoded values, not bytes.** A `304` is served from the cached parsed result with no re-parse.
- **Bounded.** An LRU capped at 2048 entries and 32 MiB of body bytes, so closed PRs age out.
- **Token identity.** `SetToken` clears the cache when the token actually changes (one cold sweep per roughly-hourly installation-token rotation). A generation counter stops a request that began under the old token from repopulating the cache after the swap. Correctness does not depend on GitHub's `Vary: Authorization`.
- **Not touched:** `FetchPRDiff` (different `Accept`; only reached by PRs that pass eligibility).

### Request accounting (R4)

`Client` counts every request it sends and every `304` (`RequestStats()`, cumulative). `poll()` differences them per owner and logs one line per cycle: `owner=… requests=… not_modified=… prs=… memo_skipped=… evaluated=…`. `RepoPollEvent` carries `MemoSkipped` for the TUI. `RequestCounter` is an optional interface, type-asserted like `RateLimitReporter`, so test fakes need not implement it.

## Measurement

`TestIdlePollCost_ScalesWithChangesNotOpenPRs` polls a real `Daemon` through a real `*github.Client` against a synthetic GitHub (`httptest`, ETag-honouring) with 11 repos, every PR already reviewed at its head. Server- and client-side counts are asserted equal. (`req/304/billed`; billed = requests − 304s.)

| config | open PRs | cold | idle | one PR commented |
|---|---|---|---|---|
| baseline (pre-#1952) | 55 | 176/0/176 | 176/0/176 | 176/0/176 |
| conditional only | 55 | 176/0/176 | 176/121/55 | 176/119/57 |
| **memo + conditional** | 55 | 187/0/187 | **22/11/11** | 25/11/14 |
| baseline (pre-#1952) | 220 | 671/0/671 | 671/0/671 | 671/0/671 |
| conditional only | 220 | 671/0/671 | 671/451/220 | 671/449/222 |
| **memo + conditional** | 220 | 682/0/682 | **22/11/11** | 25/11/14 |

Idle cost with the memo is one listing plus one repo-config read per repo — independent of the number of open PRs (asserted: 55 and 220 PRs cost the same) — and one changed PR adds a small constant. The cold start costs one extra request per repo (the fingerprint).

A supplementary read-only live check against `handarbeit/fabrik` (2026-09-29) confirmed the assumptions the synthetic server encodes: `GET /pulls?state=open&per_page=100` returns a (weak) `ETag`; replaying it with `If-None-Match` returned `304 Not Modified` and `X-RateLimit-Remaining` did not decrease across two consecutive `304`s. `GET /contents/<file>?ref=main` returns a strong `ETag` that is a 40-hex blob-style identifier and also answers `304`. A `404` (a missing file) carries **no** `ETag` and still costs a request.

**Verified live:** ETag/`304` on list and contents endpoints, `304` not consuming `X-RateLimit-Remaining`, `404` carrying no `ETag`. **Assumed (GitHub-documented, not exercised live because doing so would mutate real PRs):** `updated_at` advances on pushes, comments, reviews, label changes, and draft/ready transitions; and the contents `ETag` is stable across unrelated pushes to the base branch (if it is not, the config read returns `200` more often — still correct, only less saving).

## Consequences

- **The residual floor is one `404` per repo per poll** for repos with no `.pruefer/config.yaml` (the overwhelmingly common case): a `404` carries no `ETag`, so it cannot be a free `304`. That is now half of the remaining idle billed cost (11 of 22 in the table above) and independent of PR count. A negative-cache TTL would remove it but delays noticing a newly added repo config by the TTL, which is the stale-skip class this design refuses; recorded as a possible follow-up, not done here.
- **Known edge:** a reaction-only change on an old comment (removing a 🚀 from a `/pruefer review` comment to re-arm it) does not bump the PR's `updated_at`, so it is not picked up until something else changes the PR. The documented trigger is a *new* `/pruefer review` comment, which does bump it.
- **A reload costs one re-evaluation sweep.** Acceptable: reloads are operator-initiated and rare.
- **TUI:** memo-skipped PRs emit no per-PR events, so the TUI shows a PR's last real event; `MemoSkipped` and the poll log line make the skips visible.
- **Cost of a wrong stamp component is a re-evaluation, never a skipped review.** Every ambiguity — unknown fingerprint, empty `updated_at`, a config change mid-evaluation, a failed listing — resolves toward evaluating.

## R5: does event-driven mode make this moot?

No. `event_source: hookdeck` already exists, with a low-frequency reconciliation fallback poll — but `reconciliation.fallback_interval` **defaults to `2m`** (`DefaultReconciliationFallbackInterval`), the same as `poll_interval_seconds` (`DefaultPollInterval`, 120s). Out of the box, enabling Hookdeck therefore does not reduce sweep frequency at all, and the fallback sweep still paid the full per-PR cost. Event mode only reduces sweeps if the operator also raises the fallback interval, and it adds an external dependency (Hookdeck). The startup sweep (`reconciliation.startup: true`) and every reconnect-triggered `triggerReconciliationPoll` are also full sweeps. R1/R2 therefore benefit the fallback sweep, the startup sweep, and every poll-mode deployment, and event mode remains an *additional* lever, not a substitute.

**Recommendation (not changed here — defaults are out of scope):** polling remains the default; operators who enable Hookdeck should raise `reconciliation.fallback_interval` well above `2m` to realise the saving, and may reasonably do so *because* R1/R2 make the fallback sweep cheap enough that the trade is safe.

## Alternatives Considered

- **Memo check inside `ReviewPR`.** One place for both entry points, but the goroutine, semaphore slot and PR gate are already paid, and it needs a signature change on a function with about 45 call sites. `Conclusive` is a new outcome *field* instead.
- **Ignoring repo config in the memo key.** A stale-skip: a base-branch config change moves nothing on the PR.
- **A per-PR repo-config read to key the memo.** Defeats the purpose.
- **A fingerprint of the skip-relevant operator fields instead of a generation counter.** More precise, easier to get wrong (see above).
- **Key ETags by a token generation instead of clearing on `SetToken`.** Equivalent in effect, more moving parts.
- **Memoising on the event path.** Would need `UpdatedAt` on `FetchPRDetails` and a base-ref fingerprint per event; the bounded cost is one re-evaluation per event-touched PR at the next sweep.
- **Persisting the memo.** A cold start re-evaluating every PR once is acceptable per the issue, and persistence would contradict ADR-1113 for little gain.
