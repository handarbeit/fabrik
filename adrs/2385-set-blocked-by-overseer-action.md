# ADR 2385: Overseer `fabrik_set_blocked_by` — order work by adding and removing `blockedBy` edges

Status: accepted (issue #2385; extends [ADR-1966-c](1966-c-fabrik-mcp-actions.md), parent initiative #1966; builds on [ADR-1419](1419-cross-repo-spawn-servability-and-midflight-recognition.md) and [ADR-1783](1783-deterministic-post-spawn-dependency-gate.md))

## Context

An overseer sequencing a milestone has to order work, and it must do so **before** an item receives an autonomy label or a pipeline column, or Fabrik races ahead of the intended order. The `fabrik` MCP could promote, advance, park, set autonomy, revalidate and clear the Claude limit, but not set a `blockedBy` edge. That meant raw GraphQL under a second token, no Fabrik audit comment, and no Store write-through, so the cached `BlockedBy` (and with it `checkDependencies`, `fabrik_status` and the attention view) stayed stale until the next deep fetch.

ADR-1966-c fixed the rules an action follows: it reuses a transition the engine has, writes first and then posts one audit comment, is attributable, refuses with the item's current state, and never lifts a pause or posts a free-form comment. `set_blocked_by` is a seventh action under those rules. The decisions below are the ones a contributor would otherwise have to rediscover.

## Decisions

### 1. All guards for all edges run before the first write

The request is a list of edges, not one transition. Every guard (self-block, a blocker in both lists, blocker resolution, cycle check) is evaluated for every edge first, so a refusal never leaves a partial change; only a GitHub failure mid-loop can. That case follows `set_autonomy`'s precedent: edges already written stay, the audit comment is posted worded as a partial application, and the error names what was and was not applied. The Store holds exactly what was written. Adds are written before removes, so a failure leaves the more constrained state.

### 2. The cycle check trusts cached edges only, and an unknown edge refuses

`blockerCycle` walks the proposed blocker's cached `BlockedBy` transitively looking for the target, through `Store.Peek`. It is deliberately not `detectCycle` (`engine/dependencies.go`): that helper calls `Store.Get`, whose fallback fetcher reads GitHub, and it skips a node it cannot find, which would turn "unknown" into "no cycle". An unknown fact refuses, as everywhere in the overseer actions: a node the Store does not hold, or one that has never been deep fetched (`LastDeepFetchAt` zero), refuses with the issue named. The test is the deep fetch alone, not `fabrik_status`'s looser "zero and no cached edges": a write-through add (this action's or spawn's) leaves a never-fetched item with one cached edge, and that partial list must not read as complete, or a later removal of an unseen real edge would be reported as a no-op and a cycle through unseen edges as no cycle. `Dependency.Repo` comparison in the Store is case-insensitive, as repository names are on GitHub. A definite cycle wins over an unknown node elsewhere, and a **closed** node is a leaf, since the dependency gate treats a closed blocker as satisfied and its own edges cannot hold the target.

Consequence, stated plainly: a card parked in an unmanaged column is never deep fetched (#778), so its edges are unknown. A parked item can be the **target** of an add (its own edges are never walked) but a parked **blocker**, or an uncached cross-repo blocker, is refused until the daemon has fetched it. Live reads for cycle detection are out of scope for this issue; if the refusal proves too strict in practice, reading the unknown node's edges with one live `FetchItemDetails` (the ADR-1419 precedent) is the follow-up, and a failed read would still refuse. `checkDependencies`' own cycle pause remains the backstop for a stale cache (ADR-957).

The same polarity applies to a **remove**: a remove of an edge that is not cached is a no-op only when the target's own edges are known. For a target with unknown edges the empty cache would hide a real edge, so the request is refused rather than reported as done.

### 3. A blocker is resolved to a writable issue, from the cache first

`AddBlockedBy` / `RemoveBlockedBy` take node IDs and accept issues only. The blocker is looked up with `Store.Peek` (`ItemState.ID`, `IsPR`, `IsClosed`); a miss makes one live `FetchProjectItem` (REST `GET /repos/{o}/{r}/issues/{n}`). That endpoint also serves pull requests, so `ProjectItem` gained `IsPR`, set from the `pull_request` key (the sim's projection sets it too). A pull request, a read error, a missing item or a missing node ID refuses before any write, so a PR blocker cannot fail halfway through a multi-edge call. No new `GitHubClient` method was needed for resolution. The boardcache fallback (`ensureIssueInStore`) shares that projection and now records the PR as a PR rather than as an issue, which is correct.

A bare `N` in a blocker means the **target's** repo, not the daemon's single repo as for the `issue` argument, so the blocker parser is separate from `resolveIssue`. A closed blocker may be added: it is already satisfied and `checkDependencies` decides what that means.

### 4. The tool never writes `fabrik:blocked`; the only nudge is a cooldown

`fabrik:blocked` stays with `checkDependencies`' live read (ADR-1419). The structural scan bans every label write, `checkDependencies(` and `moveItemToStatus(` in `engine/overseer_blockedby.go`. An **add** needs no help: `checkDependencies` reads the written-through cache on first dispatch (the #1783 pattern). A **removal** does: `BlockedByChanged` is in neither the wake nor the cycle-set flags, and Path 2 of `PushUnblockObserver` returns early on an empty list, so an item already carrying `fabrik:blocked` would wait out the whole `githubRecheckInterval` — and removal does not bump `updatedAt` (#977), so no probe would notice. After a removal on an item that still carries `fabrik:blocked`, the action records an **already-due** `dep-blocked` cooldown (`CooldownRecorded` with `Until = now`). That is an in-memory admission hint: the item is re-admitted on the next poll and `checkDependencies` decides from a live read, so a wrong nudge costs one extra evaluation. Adding `BlockedByChanged` to the wake flags was rejected because it would change every writer of that mutation, spawn included. The sim scenario fails without the nudge.

### 5. A new Store removal mutation, and one definition of "the same edge"

`BlockedByEdgeRemoved` mirrors `BlockedByEdgeAdded` and emits `BlockedByChanged`. `Dependency.Repo` is empty for a same-repo blocker when fetched from GitHub but the write paths record the full `owner/repo`; the old add compared the raw strings and would have stored a duplicate beside a fetched edge. Both mutations now compare through `sameDependency` (empty means the item's own repo), which also fixes spawn's resume path.

### 6. Per-edge results are typed

`ActionResult.Edges []EdgeResult{Blocker, Op, Outcome, Reason}` is additive and `omitempty`. `Outcome` is `applied`, `noop` or `not_applied`; blockers are always written `owner/repo#N`, in the result and in the audit comment. A no-op is judged against the cached edges, and the reason says so. When nothing was written the audit comment is `none`.

## Consequences

- An overseer can order work through the MCP alone, with an audit trail and an immediate cache update.
- Adding an edge to a parked card works; adding a parked card (or any uncached issue) as a blocker is refused until the daemon has fetched it. This is the main usability cost of the cached-only rule (Decision 2).
- `GitHubClient` and `localapi.Actor` each gained a method, so every test double was updated.
- The wire contract validates `removeBlockedBy` against the vendored schema, with the same two neutralisation proofs as `addBlockedBy`.
- `simgh.RemoveBlockedByIssue` does not bump `updatedAt`, matching real GitHub (#977); removing an absent edge is a silent no-op there, which the engine never relies on.
