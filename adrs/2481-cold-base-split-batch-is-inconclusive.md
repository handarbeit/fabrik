# ADR 2481: A split merge-train batch in a live test is INCONCLUSIVE, detected from the bed log — not prevented, not a FAIL

## Status

Accepted (#2481). Builds on ADR-1973 (the INCONCLUSIVE outcome) and ADR-1772/1773 (the cold-cache base scenario). Consistent with ADR-1454: nothing moves live coverage to the sim, and an INCONCLUSIVE test stays uncovered.

## Context

On the 0.0.90 gate `TestMergeTrainColdCacheBaseMember` failed on `app/on` and passed on `pat/on`. The dev Pruefer posted a `COMMENTED` review on a seeded member's PR while the bed was down. The engine counts a review body as unprocessed feedback, so `queued-review-settle` ejected that member; the other landed alone through the singleton fast path. The test's A1 wait matched only the batch landing line (`integration PR #N, M members`), so it waited out 35 minutes and failed — although the test already had a guard that classifies a singleton landing as INCONCLUSIVE, which only ran after the per-member landing waits. Ejecting a member with an unprocessed review is intended engine behaviour; the defect is in the harness.

## Decision

1. **Detect the split from the bed log and end INCONCLUSIVE.** `waitForLandingOrSplit` polls the log and asks `trainlog.Decide`, a first-in-time classifier: a batch landing line → proceed; a singleton-fast-path landing line for the test's own train key → not batched; a `queued-review-settle` line naming one of the seeded members → not batched; none within the budget → `t.Fatalf` (a genuine failure to land stays a FAIL). A batch landing seen before an eject wins. It runs **before** the per-member landing waits, which an ejected member would otherwise strand into a 15-minute FAIL.
2. **Prevention was considered and rejected.** (a) Seeding so the review is already "addressed" is infeasible: the `review-ids-addressed` marker is keyed on the review's database id (absent at seed time) and must be authored by the engine's identity; a harness comment would itself eject the member. (b) Waiting for the reviewer before restart violates the requirement that the outcome not depend on Pruefer's timing, and dismissal of `COMMENTED` reviews is not available. The existing `pruefer:ignore` label stays as best-effort prevention; making the dev Pruefer honour it is an operations matter.
3. **A new untagged package `tests/e2e/trainlog`** holds the pure matchers (`ParseLanding`, `MemberEjected`, `Decide`) so their unit test runs in plain `go test ./...`; the `e2e`-tagged `*_parse_test.go` files are only compiled by CI. The shared `parseLandingComplete` is untouched (its pin test asserts the singleton line is rejected); the singleton form is a separate parser.
4. **Scope.** Singleton landings only count as a split in scenarios on their own throwaway partition (cold-base); on the shared default partition only an eject of one of the scenario's members is a split. The happy-path, two-base and restart tests adopt the same wait; the batch-cap test reuses `MemberEjected`. The cold-base test also re-pauses its members on failure so an INCONCLUSIVE run leaves no un-paused Queued member for the next pre-flight.

## Consequences

- A bot review of a seeded member no longer produces a FAIL or a 35-minute wait; the cell is retried by the gate (`E2E_INCONCLUSIVE_RETRIES`). If the dev Pruefer keeps reviewing `pruefer:ignore` PRs the retry can repeat — fixing Pruefer is the real prevention.
- The matchers copy the engine's format strings; `trainlog_test.go` pins them so a wording change fails loudly.
- No engine change.
