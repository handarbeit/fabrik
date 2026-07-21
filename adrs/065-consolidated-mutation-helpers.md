# ADR 065: Consolidated GitHub-Mutation Helpers (label/comment/pause)

## Status

Accepted

## Context

Every state change the engine makes to GitHub follows the same three-beat idiom: mutate on GitHub, mirror the result into the board cache (`boardcache.CacheImpl`), and register a webhook echo so the mutation echo-check (ADR-042) can detect stream failures. Before this change, that idiom was hand-typed at every call site rather than encapsulated behind a helper:

- ~170 inline `if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok { ... }` type assertions
- ~115 `ApplyLabelAdded`/`ApplyLabelRemoved` write-through call sites
- ~81 `RegisterEcho` call sites
- ~44 `ApplyCommentAdded` call sites
- 11 `pauseFor*` functions sharing an ~25-line tail (post a comment, add `fabrik:paused` + `fabrik:awaiting-input`), plus 2 `escalate*` functions with a related but non-identical tail

Because no helper enforced the sequence, hand-copied sites drifted from each other. The most damaging drift was inconsistent repo-key derivation: some sites resolved the repo via `item.Repo` directly, others via `itemOwnerRepo(item, e.defaultRepo())`, and a mismatch between the two meant a mutation could be applied to one repo while the cache write used a different key — silently corrupting cache state for multi-repo boards. This was the root cause of several correctness bugs fixed in the P0 issue immediately preceding this one in the code-health chain.

## Decision

Introduce a small set of `Engine` methods in `engine/mutate.go` that encapsulate the mutate → cache-write-through → webhook-echo idiom, and route existing call sites through them.

### Layered design: private echo-parameterized cores + always-on public wrappers

- `e.cache() *boardcache.CacheImpl` — the cast-or-nil accessor, replacing every inline type assertion.
- `e.applyLabelAdd(item, label, echo bool)` / `e.applyLabelRemove(item, label, echo bool)` — private cores performing the GitHub mutation, cache write-through, and a webhook echo *conditional on `echo`*. `applyLabelRemove` treats `gh.ErrNotFound` as success and still syncs the cache, but never echoes on `ErrNotFound` (nothing changed on GitHub to echo).
- `e.addLabel(item, label)` / `e.removeLabel(item, label)` — the always-echoing public wrappers (`echo: true`), used by the great majority of call sites.
- `e.syncLabelAdd(item, label, echo bool)` / `e.syncLabelRemoval(item, label, echo bool)` — the cache-write-through-plus-conditional-echo *tail* only, split out of the cores above. Some call sites must perform the GitHub mutation themselves because other logic (e.g. a local `lockAcquired` boolean, a retry-with-backoff loop) is gated on the mutation's own success/failure — those sites call the raw client method directly and then call the shared tail instead of re-inlining it.
- `e.postComment(item, body string, react, echo bool) (int, error)` / `e.postItemComment(item, body string, react bool) int` — the same core/wrapper split for comment posting: `AddComment` → cache write-through via `ApplyCommentAdded` → conditional `RegisterEcho("issue_comment", "created", ...)` → optional rocket reaction.

The `echo`-parameterized cores exist for two reasons: (1) sites that never had a `RegisterEcho` call must not gain one silently — a "behavior must stay byte-for-byte identical" mandate — and (2) `pauseIssue` (below) needs to suppress echo entirely at some call sites.

The repo string is resolved exactly once, inside each helper, via `itemOwnerRepo(item, e.defaultRepo())` — the same canonical resolution used everywhere else in the engine. A caller only ever passes `item`; it cannot accidentally use `item.Repo` in one place and a separately-resolved `owner, repo` in another. This structurally forecloses the repo-key divergence bug class described above. Where an existing call site had this exact divergence (e.g. one site posted a comment via `e.cfg.Owner`/`e.cfg.Repo` while the cache write already used the item-resolved owner/repo), routing it through the helper both fixes the divergence and satisfies the acceptance criterion that such a site can no longer recur.

### `pauseIssue` and the `pauseOpts` escape hatch

`pauseIssue(item, comment string, opts pauseOpts)` collapses the shared tail of the 11 `pauseFor*` functions and the pause portion of 2 `escalate*` functions: post the comment, add `fabrik:paused`, optionally add `fabrik:awaiting-input`, optionally remove `fabrik:auto-merge-enabled`.

A single `bool` cannot describe every call site's behavior, because reading all 13+ call sites end-to-end turned up three genuinely distinct, pre-existing patterns:

| Pattern | Sites | `awaiting-input` | comment reaction | label echo | comment echo |
|---|---|---|---|---|---|
| A | most `pauseFor*` functions | yes | yes | no | no |
| B | the 2 `escalate*` functions | no | yes | yes | yes |
| C | `pauseForBrokenLinkage` | no | no | yes | no |

`pauseOpts` makes each axis an explicit field (`awaitingInput`, `reactRocket`, `labelEcho`, `commentEcho`, `removeAutoMerge`), so every call site passes a literal matching its own row in the table above — auditable, and impossible to silently unify. The `RegisterEcho` asymmetry between `pauseFor*` (never echoes) and `escalate*` (always echoes) is a genuine pre-existing inconsistency, not something this refactor is chartered to fix; `pauseOpts` preserves it exactly rather than picking a side.

A fourth axis surfaced during review: *order*. Pattern A's `pauseFor*` functions historically posted their comment before adding `fabrik:paused`; Patterns B and C added the label first. `pauseOpts.labelFirst` (default `false`, matching Pattern A) reproduces each site's original ordering — Patterns B and C set it to `true`. `TestPauseIssue_PatternA_CommentBeforeLabel` and the order assertions in the Pattern B/C tests pin this down.

### `bumpLocalDeltaAt` re-inlining

Separately, `boardcache/boardcache.go` had 7 mutator methods that re-inlined the `now := time.Now(); c.mu.Lock(); c.localDeltaAt[key] = now; c.mu.Unlock()` sequence instead of calling the existing `bumpLocalDeltaAt` helper (already used correctly 23 times in `boardcache/delta.go`). These were collapsed to `c.bumpLocalDeltaAt(key)` calls as part of the same cleanup, since it is the same "helper exists but isn't used everywhere" problem this ADR addresses, just for a cache-internal idiom rather than an engine-to-GitHub one.

## Consequences

**Benefits:**
- The `item.Repo`-vs-`owner/repo` repo-key divergence bug class is structurally impossible at any routed call site — the helper resolves the repo once, internally.
- New call sites default to the correct three-beat idiom by construction instead of by developer diligence.
- The mechanical diff is large (~20 files touched) but every commit in the sequence leaves the tree building, vetting, and testing green — there is no "helpers added but nothing routed yet" intermediate state that's broken.

**Drawbacks / risks:**
- `pauseOpts` is an extra layer of indirection a future contributor must learn before adding a 14th pause site; the per-pattern table above (and `pauseOpts`'s doc comment in `engine/mutate.go`) is the reference.
- A handful of call sites could not be routed through the always-echoing public wrappers because they either (a) have local control flow gated on the raw mutation's success/failure, or (b) never echoed/reacted before and must not gain that behavior silently. These use the private `applyLabelAdd`/`applyLabelRemove`/`postComment` cores directly with an explicit `echo`/`react` argument — slightly more verbose than the wrapper, but still eliminates the repeated cache-assertion boilerplate and keeps repo resolution canonical.
- One deliberate, small, sanctioned behavior change: `removeLabel`'s ErrNotFound-still-syncs-the-cache behavior is now applied uniformly, including at sites that previously skipped the cache sync on `ErrNotFound` entirely. This was called out explicitly as in-scope by the parent issue (it's the same bug class the issue exists to eliminate), unlike the `pauseFor*`/`escalate*` echo asymmetry, which is preserved rather than unified.

## Related ADRs

- [ADR-042: Mutation Echo-Check for Webhook Health Detection](042-mutation-echo-check.md) — defines *why* `RegisterEcho` calls matter and what a missed one costs (delayed silent-webhook-failure detection); this ADR defines *how* to call it consistently so call sites can't drift out of sync with that mechanism.
