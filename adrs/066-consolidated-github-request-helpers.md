# ADR 066: Consolidated GitHub-Client Request Helpers (REST core, paginator, boardcache mutation/heal)

## Status

Accepted

## Context

The `github` client and the `boardcache` layer accumulated copy-pasted request/response scaffolding — lower-risk than the engine-side mutation duplication fixed in [ADR-065](065-consolidated-mutation-helpers.md), but it inflated two of the largest files in the codebase (`github/project.go`, `boardcache/delta.go`) and meant cross-cutting changes (auth headers, retry policy, negative-cache protocol) required edits in many places:

- Four `github/prs.go` mutations (`MarkPRReady`, `EnablePullRequestAutoMerge`, `EnqueuePullRequest`, `DequeuePullRequest`) each opened with a byte-for-byte identical ~28-line "fetch PR node id via GraphQL" block.
- The six `github/rest.go` REST helpers each repeated `NewRequest` → set headers → `Do` → `defer Close` → `updateRestStats` → status-code handling, with an *asymmetric* sentinel mapping: only some helpers mapped 405/422, only some mapped 404, none did both consistently.
- `fetchProjectBoardOnce`, `probeProjectBoardOnce`, and `FetchProjectItemStatusBatch` each hand-rolled the cursor-pagination loop; `fetchProjectBoard` and `probeProjectBoard` each hand-rolled an identical 3-attempt retry-on-empty wrapper around their `*Once` sibling.
- The comment-selection GraphQL fragment was hand-written 5 times across `project.go`; `*MergeQueueEntry` construction was duplicated at 2 sites.
- Six `boardcache.CacheImpl` mutators shared an identical `parseItemKey` → invalid-key log → phantom-guard → `store.Apply` → change-check → bump-sequence preamble.
- Four `boardcache/delta.go` auto-heal handlers each repeated a ~20-line negative-cache-miss → `resolvePRLinkage` → `store.Get`-outside-`c.mu` confirmation → heal-log sequence.

This is the same "helper doesn't exist yet, so every call site reinvents it slightly differently" problem ADR-065 addressed for the engine's GitHub-mutation idiom, applied here to the client and cache layers.

## Decision

Introduce six focused helpers, one per duplication point, and route the existing call sites through them mechanically — no change to request semantics, retry counts, or on-wire behavior beyond the two additive exceptions called out below.

### `c.prNodeID` (github/prs.go)

A single `c.prNodeID(owner, repo string, prNumber int) (string, error)` replaces the four copy-pasted "fetch PR node id" blocks. All four mutations (`MarkPRReady`, `EnablePullRequestAutoMerge`, `EnqueuePullRequest`, `DequeuePullRequest`) call it before their one-line mutation.

### `c.do()` REST core (github/rest.go) — uniform sentinel mapping

`c.do(method, url string, body interface{}) (*http.Response, []byte, error)` is the single place that sets headers, executes the request, records rate-limit stats, and maps status codes to sentinels (404 → `ErrNotFound`, 405 → `ErrMethodNotAllowed`, 422 → `ErrUnprocessableEntity`). All six typed REST helpers (`restRequest`, `restGetJSON`, `restGet`, `restPostWithResponse`, `restPutWithResponse`, `restDelete`) become thin decode wrappers around it.

This **uniformly applies the full sentinel mapping to all six helpers**, including the four that previously mapped none or only some of the three codes. This is the one deliberate, additive-only behavior change in this ADR: no existing caller relied on *not* getting a sentinel-wrapped error (verified — no `errors.Is` call anywhere assumed a generic error from one of the previously-unmapped paths), so no `errors.Is` check anywhere changes behavior, and no on-wire request changes. It is the literal reading of the parent issue's acceptance criterion that "the 405/422/404 sentinel mapping lives in exactly one place."

`graphqlRequest` (`github/client.go`) is **not** routed through `do()`. It diverges structurally enough that forcing it through the same primitive would cost more than it saves: it always POSTs to `/graphql` only, never sets an `Accept` header (adding one would be a real on-wire change, out of scope), treats `!= 200` as its success boundary rather than `>= 400`, and must additionally unmarshal the body a second time looking for a GraphQL-level `errors[]` array even on HTTP 200 — something no REST helper needs. `graphqlRequest` already shares `updateRestStats`/`parseRateLimitHeaders`/`authErrorHint` with the REST path; the one piece left that it can't safely absorb is exactly the piece that differs.

`do()` sets `Content-Type` only when `body != nil`, exactly reproducing each wrapper's pre-existing header behavior, and each wrapper keeps its own pre-existing decode-error-wrapping asymmetry (`restGetJSON` stays unwrapped; the others stay wrapped) rather than standardizing it — that asymmetry wasn't in scope.

### `paginateGraphQL[T]` / `retryOnEmpty[T]` (github/project.go) — split, not one generic paginator

Rather than one fully-generic paginator that understands every response envelope, this is **two separable generics**:

- `paginateGraphQL[T any](opLabel string, fetchPage func(cursor string) (nodes []T, hasNextPage bool, endCursor string, totalCount int, err error)) ([]T, int, error)` — loop-only. It accumulates nodes, tracks the largest `totalCount` observed, and verifies the `hasNextPage`/`endCursor` invariant. It has no knowledge of GraphQL response envelopes; each call site (`fetchProjectBoardOnce`, `probeProjectBoardOnce`, `FetchProjectItemStatusBatch`) keeps its own query text, response struct, and per-node post-processing, and passes a closure that unwraps *its own* envelope into the four generic return values.
- `retryOnEmpty[T any](opLabel string, fetch func(attempt int) (result T, rawCount, totalCount int, err error)) (T, error)` — the pre-existing 3-attempt linear-backoff wrapper, now shared by `fetchProjectBoard` and `probeProjectBoard` instead of hand-rolled twice. `probeProjectBoard` returns two values where `retryOnEmpty` expects one `T`; a local `probeBoardResult{Items []BoardProbeItem; ProjectID string}` closes that gap without widening the generic's contract.

The split exists because `fetchProjectBoardOnce`, `probeProjectBoardOnce`, and `FetchProjectItemStatusBatch` build materially different GraphQL queries and populate materially different response structs (full item fields with labels vs. minimal probe fields vs. a bare `node(id:)` lookup with no `totalCount` at all). Forcing all three into one struct-aware paginator would have required an artificial common envelope wide enough to be harder to read than the status quo — the Research/Plan stages for this issue flagged this as the highest-effort, highest-risk item, and the loop-only/envelope-separate split is what kept it from needing a child issue. `FetchProjectItemStatusBatch` gets `paginateGraphQL` but not `retryOnEmpty` — it never had a retry wrapper before, and adding one is a bigger behavior change than a mechanical routing pass should make silently.

### `commentSelectionFragment` + `toMergeQueueEntry` (github/project.go)

`commentSelectionFragment` is a single-line `const` GraphQL field-selection string, concatenated into the 5 query sites that previously hand-wrote it (GraphQL is whitespace-insensitive, so this is not an on-wire change). `mergeQueueEntryData` promotes the two previously-inline anonymous `MergeQueueEntry` struct types to one named type, and `toMergeQueueEntry(*mergeQueueEntryData) *MergeQueueEntry` replaces the 2 duplicated construction sites. `FetchItemDetails` itself is not restructured — only its inline query/struct fragments are routed through the new helpers, per the parent issue's explicit scope boundary (the function's own decomposition is a separate, later issue).

### `c.applyKeyedMutation` (boardcache/boardcache.go) — 5-of-6 scope, not 6-of-6

```go
func (c *CacheImpl) applyKeyedMutation(key, opName string, build func(repo string, number int) itemstate.Mutation)
```

`UpdateItemStatus`, `ApplyLabelAdded`, `ApplyLabelRemoved`, `ApplyIssueClosed`, and `ApplyCommentAdded` collapse to one-line calls through this helper. `ApplyStatusBatch` is **deliberately excluded**: it resolves its cache key from the mutation's own returned snapshot, once per map entry, rather than from an input key — it never had the `parseItemKey` → phantom-guard shape the other five share, so forcing it through `applyKeyedMutation` would mean inventing a fake key argument to fit a helper contract it doesn't need. The parent issue's prose lumps "the six mutators" together loosely; this ADR's scoping call follows the actual code shape, not the issue's rounding.

As part of this change, the invalid-key log wording is standardized to `"invalid"` across all 5 routed methods (previously only `UpdateItemStatus` said `"not found"`; the other four already said `"invalid"`). This is a small, sanctioned wording normalization — no test asserted on the old string, and it follows the same "call out the one deliberate tweak explicitly" pattern ADR-065 established for its `removeLabel` ErrNotFound change.

### `c.resolveOrHealPRLinkage` (boardcache/delta.go) — scope boundary against the AST lock-invariant test

```go
func (c *CacheImpl) resolveOrHealPRLinkage(owner, repoName, repo string, prNum int, missCacheKey, notFoundMsg string) (key string, issNum int, healed, ok bool)
```

`applyPullRequestDelta`, `applyPullRequestReviewDelta`, `applyPullRequestReviewCommentDelta`, and `applyCheckRunDelta` each repeated: call `resolvePRLinkage` → on not-found, record a negative-cache miss and log a drop message → on found, confirm the resolved issue still exists in the Store via `store.Get` performed **outside** `c.mu` (a pre-existing fix from a prior code-health pass, preserved here) → on confirm-failure, record another negative-cache miss. `resolveOrHealPRLinkage` extracts exactly this block. Each handler still owns 100% of its own post-resolution `store.Apply` calls and `healed`-branch logging — those differ per handler and stay inline.

Two scope boundaries were deliberate, not oversights:

1. **The PR-scoped negative-cache pre-check stays inline in each caller**, rather than folding into the shared helper. `applyPullRequestDelta`, `applyPullRequestReviewDelta`, and `applyPullRequestReviewCommentDelta` all have one (checking `recentMissCache[missKey(repo, prNum)]` before ever calling `resolvePRLinkage`); `applyCheckRunDelta` does not, because it already performs its own SHA-keyed negative-cache check earlier, before its `FetchPRsForSHA` call. Folding the PR-scoped pre-check into `resolveOrHealPRLinkage` would silently add a check-run code path that doesn't exist today.
2. **The extraction moves a `c.mu`-adjacent sequence out of the four named functions that `boardcache/delta_lock_invariant_test.go`'s `TestDeltaHealPaths_DoNotCallStoreWhileLocked` statically walks.** That test parses `delta.go` and inspects only the literal AST bodies of the four handlers for `c.store.*` calls made while `c.mu` is (locally) tracked as held — it does not recurse into helpers those bodies call. Left unaddressed, extracting the `store.Get`-outside-`c.mu` sequence into `resolveOrHealPRLinkage` would silently stop the test from observing it: not a false pass, but a coverage gap in a test whose entire purpose is guarding this exact invariant. The fix is one line — `resolveOrHealPRLinkage` is added to the test's `funcNames` slice, so the AST walk now also covers the extracted helper directly. This keeps the invariant meaningfully enforced against the code that actually performs the lock-sensitive sequence now, rather than against code that used to but no longer does.

## Consequences

**Benefits:**
- The 404/405/422 sentinel mapping now lives in exactly one place (`do()`), closing the pre-existing asymmetry where different REST helpers mapped different subsets of status codes.
- `prNodeID`, `applyKeyedMutation`, and `resolveOrHealPRLinkage` mean new call sites default to the correct request/mutation/heal idiom by construction rather than by copy-pasting an existing site and hoping nothing was missed.
- The paginator split keeps the largest single extraction in this issue reviewable as "did the loop body move without changing" rather than "did three materially different functions get redesigned into one."
- Every commit in the implementation sequence left `go build`, `go vet`, and `go test -race` green — no "helper added but nothing routed" intermediate state.

**Drawbacks / risks:**
- `do()`'s uniform sentinel mapping, while additive-only today, means any future REST helper added to `github/rest.go` inherits sentinel mapping automatically — a future author who wants to *not* get a sentinel-wrapped error for some new endpoint needs to know to bypass `do()` deliberately, not just add a new thin wrapper.
- `applyKeyedMutation`'s 5-of-6 scope and `resolveOrHealPRLinkage`'s pre-check/lock-sequence boundary are both judgment calls a future contributor could get wrong by assuming the helper covers more than it does. This ADR, plus each helper's doc comment, is the reference for exactly where the boundary sits and why.
- `paginateGraphQL`/`retryOnEmpty` being two generics instead of one means a future fourth pagination call site must decide which one (or both) it needs, rather than reaching for a single paginator — a small extra decision, traded for not forcing three already-divergent envelopes into one artificial shape.

## Related ADRs

- [ADR-034: Boardcache Event-Sourced Delta Architecture](034-boardcache-event-sourced-delta.md) — defines the delta-function architecture `resolveOrHealPRLinkage` operates within (auto-heal via REST/regex fallback when the authoritative `prToKey` index misses); this ADR is a pure internal refactor of that already-decided architecture, not a change to it.
- [ADR-065: Consolidated GitHub-Mutation Helpers (label/comment/pause)](065-consolidated-mutation-helpers.md) — the immediately-preceding code-health issue in the same audit, establishing the house style this ADR follows: layered private-core/public-wrapper helpers, an explicit "byte-for-byte identical behavior" mandate for the mechanical routing pass, and calling out each deliberate behavior tweak by name rather than letting it hide in a diff.
