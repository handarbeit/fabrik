# ADR 1877: e2e coverage for App-auth self-recognition (mint in the harness, App legs only)

**Status:** Accepted
**Date:** 2026-09-26
**Issue:** [#1877](https://github.com/handarbeit/fabrik/issues/1877)

## Context

#1754 gave the engine a self-identity under GitHub App auth: `selfLogin()`
(`engine/github_app_auth.go`) returns `<app-slug>[bot]` under App auth and `cfg.User`
under PAT, and every "is this comment/review mine?" comparison routes through it. #1861
made the e2e gate run every scenario in an App leg, but no scenario asserts
self-recognition, so a regression would make Fabrik read its own comments as human input
(the #1083-class runaway).

## Decision

Add three live-e2e scenarios (`tests/e2e/self_recognition_test.go`) that run in the App
legs and skip in the PAT legs, test-only, with no engine or engine-logging change.

1. **Mint in the harness.** A1 needs a *plain* comment authored by the App. Every comment
   Fabrik posts itself carries the `🏭 **Fabrik` prefix, which `findNewComments` excludes
   before `filterHuman` runs, so reusing a Fabrik-posted comment cannot exercise the author
   path. The harness therefore mints an installation token from the bed-local `E2E_APP_*`
   keys, importing the `github` package's `ParseAppPrivateKey`/`BuildAppJWT`/
   `MintInstallationToken` (in-module, stdlib-only) rather than duplicating JWT code. The
   token is passed to `gh` through the environment only, never argv, never logged, and is
   redacted from every failure message. A2 does reuse Fabrik's own blocked comment.
2. **Skip decision is a pure function** `decideAppLegRun(mode, identity)`: `pat` skips
   with a reason; `app` requires the bed's startup identity and fails loudly without one;
   unset follows the identity. Missing `E2E_APP_*` skips; a partial set fails.
3. **Three sites**, each on its own issues so they run in parallel: A1 `filterHuman`/resume
   gate, A2 `findBlockedComment` in-place edit, A3 `durablyAddressedReviewIDs`. The other
   `selfLogin()` sites (`engine/mutate.go`, `engine/spawn_settle.go`) are cache
   write-through only; a GraphQL refetch overwrites the stamped author, so they are not
   observable live.
4. **A3 uses one review per PR.** GraphQL `latestReviews` keeps the latest review per
   reviewer, so a suppressed review and a control review cannot share a PR. Each arm gets its
   own item/PR. Each review is created PENDING, its id known and its marker posted, and only
   then submitted, so the marker exists before the engine can see the review (no poll race).
5. **Negative observations are positive-first.** A1 waits for the engine's own
   `none human-authored` evaluation line before its 3-poll hold; A3 waits for the control
   arm's dispatch. A bare "nothing happened" window would be vacuous.

## Non-vacuity, honestly

- **A3** discriminates #1754: pre-fix it compared `c.Author != e.cfg.User` to the REST
  author `<slug>[bot]`, ignoring the bot's marker.
- **A2**'s dependency-set assertion catches the pre-fix stale-body failure
  (`findBlockedComment(…, e.cfg.User)` matched nothing and skipped the edit); a count check
  would not.
- **A1** does **not** discriminate the #1754 delta: pre-fix `filterHuman` had no `cfg.User`
  comparison, and the defect (cache write-through stamping `cfg.User`) is transient and not
  live-observable. It guards `filterHuman`/`gh.IsBotLogin` on the real wire shape and the
  ADR-1813 resume path.

## Known gap: A1/A2 may be red on the App leg

REST reports a bot comment's author as `<slug>[bot]`, but the GraphQL comment fragment
(`github/project.go`: `author { login }`, no `__typename`) is believed to yield the bare
`<slug>`, and under App auth (webhooks refused, ADR-1752) the engine reads issue comments
through GraphQL. If confirmed, `gh.IsBotLogin("<slug>")` is false and `findBlockedComment`
never matches, so A1 and A2 fail on a live App leg because of an engine defect outside this
change (reviews are normalised at `project.go`; comments are not; ADR-1045 documents the
asymmetry). The scenarios assert the correct behaviour and are not weakened. Each logs both
wire shapes so a failure is attributable. The engine-side fix, normalising `Bot`-typed
comment authors at ingestion, is tracked in #1898 (PR #1899). Until it ships, the release
gate may be red for that reason; `cut-release.sh --skip-integration=<reason>` remains the
escape hatch.

## Consequences

- The live suite gains ~15–25 min and ~$0.10–0.60 per App leg (paid in `app/off` and
  `app/on`); documented in `tests/e2e/README.md`.
- `tests/e2e` now imports the `github` package (link dependency only).
- Log strings the scenarios scrape are pinned to the engine's own source by
  `TestSelfRecognitionStringsMatchEngineSource`, so an engine rewording fails a unit test,
  not a 30-minute live run.
