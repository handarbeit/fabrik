# ADR 2066: Pruefer skips a pure merge-train catch-up the engine has vouched for

## Status

Accepted

## Context

The Fabrik merge train (#2044) can push a catch-up merge commit of the pinned base onto a Queued member's own PR branch. Pruefer reviews on every `synchronize`, so each catch-up got a full review (subscription quota, wall-clock, and a review body a human must read) even when the push only brought in base commits already reviewed and merged. The engine already treats bot findings on a pure catch-up head as non-actionable; ADR 2044 states that whether Pruefer should skip one is a separate Pruefer change. This is that change.

A skip suppresses a review, so a forged signal would mute Pruefer. The `Fabrik-Train-Catch-Up:` commit trailer is typable by anyone with push access and is not trusted (ADR 2044 says the same for the engine).

## Decision

1. **Trust anchor: the engine's marker comment.** `<!-- fabrik:train-catch-up head=<sha> base=<sha> pure=<bool> -->`, used as built. It is trusted only when authored by a login in the new operator-only list `catch_up_marker_authors` (empty by default, so the skip never fires; live-reloadable). It is not in the repo-resident allowlist (ADR 1642): a reviewed repo must not name its own trusted author. A list covers PAT versus App mode and several engines. Under PAT auth the "engine login" is a human account, so authenticity is only as good as that account's access; this is the same model as the engine side.
2. **Decision point: `ReviewPR`, after `Eligible` and before `FetchPRDiff`** (`pruefer/catchup.go`). Event dispatch, poll and the reconciliation sweep all go through it (a skip in `eventsink.go` alone would be undone by the next poll). The webhook payload carries no trusted previous head (ADR 1113) so it is not used. `/pruefer review` bypasses the skip.
3. **Verification, all required:** a trusted marker for exactly the live head with `pure=true`, full-length SHAs; the head has exactly two parents (new `FetchCommitParents`, `GET /git/commits/{sha}`); the second parent equals the marker's `base` and is on the PR's base branch (`FetchCommitsBehind(parent2, baseRef) == 0`); the first parent is a head Pruefer already reviewed (R6). The marker has no previous-head field, so the reviewed-set check against `FetchPRReviews` stands in for it. `pure` is inherited from the engine (#2044's definition), not re-derived.
4. **Chains.** A skipped head leaves no review, so consecutive catch-ups are followed back to a reviewed head, each link verified the same way, capped at `maxCatchUpChain = 4`. The cap bounds API calls and forces a periodic real review. A PR Pruefer never reviewed is reviewed at its first catch-up.
5. **Fail toward reviewing (R4).** Any missing, ambiguous or conflicting input reviews as before (disagreeing or `pure=false` markers, abbreviated SHAs, wrong parent count or order, unreviewed previous head). A tolerated read failure (comments, parents, ancestry, cancelled re-check) sets `degraded`, so that review is not memoised (ADR 1631/1952 discipline). Clean verdicts, including a skip, are conclusive.
6. **Marker timing (R8).** The engine posts the marker after the push, so the event can arrive first. For a head with two parents whose second parent is on the base branch and whose commit message carries the `Fabrik-Train-Catch-Up:` trailer (a hint that a marker is coming, never trusted as evidence for the skip), comments are re-read up to three times (2s, 3s, 3s), `ctx`-aware, for the current head only. Ordinary pushes, unrelated merges and a developer's own `git merge main` (no trailer) never wait. The wait holds an event slot for at most about 8s. If still absent the head is reviewed, and the poll skips it once the marker exists (the marker bumps `updated_at`, so the memo re-evaluates).
7. **No PR comment.** A skip only logs a `select` line, unlike the diff-too-large skips.
8. **Wire contract.** Pruefer keeps its own copy of the marker regex (it does not import `engine`). `engine/catchup_feedback_contract_test.go` renders a marker with the engine's formatter and parses it with `pruefer.ParseCatchUpMarker`, so a format change fails CI.

## Consequences

- Deploying with the setting unset changes nothing and makes no extra API calls. Enabling it is a live config reload once the engine login is known. Pruefer serves in-flight projects, so roll the new binary out in a quiet window.
- A missed marker costs quota, never correctness.
- Known limitation, recorded rather than fixed (no engine change in scope): the marker carries no previous head, so Pruefer uses the reviewed-set check instead.
- Context: #2044 (mentioned for context only).
