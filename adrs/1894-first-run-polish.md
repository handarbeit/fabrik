# ADR 1894: First-run polish — empty board, multi-repo banner, default stages' own notices

**Status:** Accepted
**Date:** 2026-11-07
**Issue:** [#1894](https://github.com/handarbeit/fabrik/issues/1894)
**Amends:** [ADR 1283](1283-declared-unrequested-reviewers.md) (omission-is-distinct; notice vs. drift), [ADR 1410](1410-ci-gate-liveness-not-elapsed-time.md) (the unconditional startup notice, R8)

## Context

The first engine start against a board created by `fabrik init --create-board` showed three kinds of noise, each of which makes a correct install look broken:

1. A new board has zero items. The board fetch treats zero as a possible indexer hiccup, so every fetch warned `retrying in case of indexer hiccup` twice and every poll re-ran the probe bootstrap.
2. In multi-repo mode the plain-text banner printed `repo: owner/`.
3. The shipped Review and Validate stages set `wait_for_reviews: true` without `expected_reviewers`, so every fresh install raised two "no expected_reviewers declaration" notices (log and panel), and a one-time `CIWaitTimeout` semantics notice fired on every start.

## Decisions

### 1. Empty board versus transient zero (FR-004)

The retry exists because the degraded Projects v2 indexer returns `totalCount=0` with HTTP 200 for a populated board, "at random, within a second". So the response itself cannot tell the two apart.

Signals considered:

| Signal | Verdict |
|---|---|
| `totalCount == 0` | Not a discriminator: it is exactly what the fault returns. (`rawCount == 0` with `totalCount > 0` is a real inconsistency and is still retried.) |
| Cache previously held items | Strong, but only the engine knows it, and it is lost on restart. A restart against a populated board whose first fetch is a transient zero has no such state. |
| Consistency across polls | Needs no persisted state, covers the restart case, and is available in the engine. |
| Persisted "ever had items" bit | Rejected: new persisted state (out of scope), and wrong on a fresh clone. |
| Time window since board creation | Rejected: an arbitrary threshold with no stronger evidence than counting polls. |

**Rule.** The engine (`engine/empty_board.go`) concludes "empty" when the cache has never held items and **two consecutive polls** each returned zero items with a project ID. Each of those polls has already exhausted the github layer's three-attempt retry, so the conclusion rests on about six consecutive zeros over at least one poll interval. Once items have been seen in the process lifetime the board is never concluded empty.

**What the verdict does.** It changes retry cost and logging only, never the item Store:

- one `board is empty — add issues to the board to start` line per episode;
- `Client.SetBoardRetryQuiet(true)`, so the board fetch and probe make one attempt with no backoff (an optional-interface assertion, so `GitHubClient`, `boardcache`, `simgh` and the fakes are untouched);
- any probe that returns items bootstraps normally, ends the episode and restores the full policy (FR-005, no restart).

A wrongly concluded "empty" therefore self-heals on the next non-zero probe, and the populated-cache guard in `runProbeAndDeepFetch` stays authoritative. Residual risk: a restart during an indexer outage lasting two or more polls delays discovery by about one poll.

**Retries are silent.** `retryOnEmpty` no longer warns on each failed attempt; it logs one line only when a retry recovers real items. A zero that exhausts every attempt is silent at that layer because only the engine can say whether it is plausible. A previously populated board keeps visibility through the engine's throttled `deferring bootstrap` line and `runProbeAndDeepFetch`'s "skipping" line. Retry count and timing are unchanged.

Out of scope: re-arming after a human empties a populated board (that path keeps its existing per-poll skip line).

### 2. Banner and scope wording

`engine.RepoScope(owner, repo)` returns `owner/repo`, or `(all repos on the board)` when `repo` is unset; the banner uses it. Engine log lines that printed `owner/#N` use `projectRef`. The TUI header and footer never rendered `owner/` (`ProjectInfo.Repo` is deliberately empty in multi-repo mode); a regression test pins that.

### 3. Shipped `expected_reviewers: []`, kept out of drift

The shipped Review and Validate stages declare `expected_reviewers: []`. The maintainer preferred fixing the defaults over suppressing the notice for unmodified files, which would need per-file "is this modified" detection.

- **Why `[]`:** a fresh install has no review bot, so the gate advances as soon as nothing is requested instead of waiting out the timeout. A named bot cannot be generic, and an absent bot would run the re-prompt-then-pause ladder on every PR.
- **Trade-off, stated in the stage YAML comments and the user guide:** an operator who later adds a review bot must declare it; a `[]` stage stops waiting for it.
- **Drift:** an active shipped key would otherwise add a `stage_drift` entry to every existing install and let `fabrik refresh-stages --apply` silently change gate behaviour. `expected_reviewers` is registered in `noOpKeyRules` as always-filtered. This amends ADR 1283, which kept the notice out of drift because omission is a distinct state: that stays true for the notice, while a shipped declaration is now a template rather than an update.
- `WarnUndeclaredReviewers` is unchanged, so an operator-authored or edited stage with `wait_for_reviews: true` and no declaration still gets the notice (FR-009).
- The `fabrik init` closing output (ADR 2496) lists board columns only and says nothing about reviewers, so it already agrees; a test pins it.
- Existing installs keep their on-disk stage files; they are not migrated.

### 4. The `CIWaitTimeout` semantics notice is retired

ADR 1410 R8 made the notice unconditional because the setting's meaning changed for every operator. It has shipped for about three months of releases, and a fresh install cannot be told from an upgrade without new persisted state. The notice, its call and its test are removed. **No "had the old setting" detector is added.** The meaning change remains stated in ADR 1410, `docs/USER_GUIDE.md` and `docs/state-machine.md`; ADR 1410's "stated in three places" guarantee drops to those.

## Consequences

- A fresh install against an empty board logs one empty-board line and no hiccup warnings, and an unmodified install raises no startup notices about its own defaults.
- Transient-zero protection on populated boards is unchanged.
- Test churn is limited to the retry-logging tests; the retry policy itself is untouched.

## Tests

`github/paginate_test.go`, `fetch_board_test.go`, `probe_board_test.go` (silent retries, quiet mode); `engine/empty_board_test.go`, `engine/repo_scope_test.go`; `stages/default_notices_test.go`; `cmd/init_test.go`, `cmd/cmd_extra_test.go`; sim `tests/sim/empty_board_test.go`.
