# ADR 1835: Merge-Train Trial Prefix Reuse

**Date**: 2026-09-24
**Status**: Accepted
**Issue**: #1835 — merge-train: reuse the valid prefix of an abandoned trial's merge chain
instead of re-assembling from base

## Context

A merge-train trial is assembled as a linear chain of merge commits — pinned base → +m1 →
+m2 → … → +mn (`assembleTrialBranch`, `engine/merge_train.go`). Whenever that trial is
abandoned — a bisection sub-trial, a re-form after ejecting a poisoner, or any other
re-assembly — the whole chain is discarded and the next assembly starts again from the
pinned base, redoing merge work (including Claude conflict resolution) for members whose
result is still valid, since every commit in the chain before the first changed input is
completely unaffected by that change.

The motivating incident (`verveguy/concept-maps`, 2026-09-20) shows this concretely: trial
#803 merged eight survivors in order, went red, and bisection's first probe was exactly the
first four members of #803's own chain — a prefix by construction. The train rebuilt it from
base anyway, re-paying 4.5 minutes of Claude conflict resolution to reproduce a commit that
had existed minutes earlier before `cleanupTrialArtifacts` deleted its only reference.

This is the direct successor to ADR-1834 (`git rerere` conflict-resolution replay) and
depends on it: without rerere, every non-prefix re-merge still re-pays Claude, and this
issue's benefit would be confined to bisect's first half. The two are complementary —
rerere replays a *resolution* cheaply from any branch context; this ADR skips the *merge
itself*, including clean merges that never needed a resolution at all.

## Decision

### 1. A content-addressed hash chain, not a structural (member-list) index

The cache (`trainPrefixCache`, `engine/merge_train_prefix.go`) records a chain of SHA-256
hashes: `h0 = chainHashSeed(trainKey, baseSHA)` — computed **per lookup** from the
`baseSHA` the caller passes (`lookup(p.baseSHA, members)`), never fixed at construction —
and `h_i =
chainHashStep(h_{i-1}, member[i].Number, member[i].headSHA)` for each member that merged
successfully. Each `h_i` maps to the resulting merge commit's SHA. A lookup for a new
member list walks the identical recurrence forward from `h0`; the last hit before the first
miss is the longest reusable prefix, and the caller resumes merging from there.

This one mechanism, with no separate invalidation logic, correctly handles every case
Requirements 3 and 4 name:

- **Bisect's first half** is by construction an exact prefix of the red trial's own chain
  (`bisect`'s `red[:mid]` never reorders `red`), so its hash walk matches every position —
  zero merges, zero Claude invocations (Acceptance 1).
- **Re-form after ejecting `mₖ`** — the survivor list is `red` with `mₖ` removed, order
  preserved. The walk matches positions `1..k-1` (whatever comes next has a different
  member number and head SHA than the hash that was recorded there), so the match stops
  exactly at `k-1`: `base..m(k-1)` is reused, and only members after the eject point are
  actually merged (Acceptance 2).
- **A changed base SHA or a re-pushed member's head SHA** simply produces a lookup miss —
  `h0` differs for a different base, and `h_i` differs from that changed position onward
  for a re-pushed head SHA — so the prefix ends there and the rest re-merges normally,
  with #1834's rerere replaying any conflict resolution that didn't involve the changed
  input (Acceptance 3). No comparison code distinguishes "base changed" from "member
  re-pushed" from "nothing recorded at all" — a miss is a miss.

An ejected member (either `assembleTrialBranch`'s mid-assembly unresolvable-conflict
ejection, or bisection's post-CI poisoner ejection) contributes no `h_i` at all — the next
successful member's step is computed against whatever hash preceded the ejected one. This
was checked by hand-tracing both ejection code paths; neither needs special-casing.

**Rejected alternative:** a structural index keyed on the literal member-number/head-SHA
list (e.g. a trie or a linear scan over recorded member lists). The hash chain was chosen
instead because it makes "does this new list start with a previously-successful prefix"
a single map lookup per position rather than a list-comparison scan, and it composes
naturally with Decision 3's ref-naming (a ref name is just a hash).

### 2. Cache scope is one `runMergeTrainWorker` goroutine invocation — never persisted

The cache and its protective git refs are both created and torn down within a single
worker invocation: constructed in `runMergeTrainWorker` right after `prepareTrainWorker`
succeeds (alongside the existing `git rerere gc` call, and skipped identically under the
`trainValidateFn` test seam, which has no real git to reuse from at all), and unconditionally
swept away — regardless of how the invocation ends — before the goroutine returns.

The evidence log's entire savings (bisect first-half, eject-reform) happen *within* one
worker invocation, where every member's `headSHA` is fetched exactly once per dispatch
(`fetchTrainMembers`) and held constant. `p.baseSHA` is pinned once in `prepareTrainWorker`
but is **not** constant for the invocation: `landOneAtATime` re-pins its local copy of
`trialParams` to the current `origin/<base>` before each singleton, and the main-moved
rebuild loop (ADR-059 D5) re-pins before re-assembling — both share the worker's one
`*trainPrefixCache`. This is why the seed is derived from the base passed at lookup time
rather than captured at construction (found in review): a construction-time seed would let
a member that was chain position 1 on the *old* base hit a stale chain and fork a singleton
(or a rebuilt trial) from a commit lacking the newly landed / newly advanced base — a
combination validated on a base it was never tested on. With the per-lookup seed, entries
recorded under different bases partition naturally and a re-pin is simply a miss. A cache
that only lives as long as the head SHAs are guaranteed stable needs no cross-invocation
staleness bookkeeping at all — "has a member landed or left Queued since this was
recorded?" is a question a fresh empty cache trivially answers correctly by finding
nothing to reuse. Requirement 6 explicitly
permits this as the default degradation, not merely as a fallback: a restart, or even the
very next poll's fresh worker dispatch, starts from a nil-equivalent cache, which by
construction can only ever reproduce today's full-reassembly behavior (Acceptance 5) —
there is no code path that could reuse a commit whose inputs weren't just verified,
because there is no persisted state to misapply.

**Rejected alternative:** an Engine-level `map[trainKey][]chainEntry` (mirroring the
existing `mergeTrainTrials` pattern), with the git refs serving as the actual persistence
mechanism across restarts. This would extend reuse across poll cycles and restarts, but
requires tracking exactly the staleness conditions above outside of live goroutine state,
and interacts with ADR-1648's concurrent per-base-partition sharing of one bare clone in
ways that are harder to reason about — for a benefit the issue's own evidence and
acceptance criteria never demonstrate a need for, since the incident's entire cost was
paid inside a single worker invocation.

### 3. Git refs are gc-protection only, never the source of truth

Every successful merge is recorded twice: once into the in-memory `entries` map (the sole
thing `lookup` ever consults), and once as a git ref under
`refs/fabrik/merge-train-prefix/<sha256(trainKey)>/<h_i>` pointing at the resulting commit
— never pushed to origin. The ref's only job is keeping that commit reachable across an
external `git gc` for as long as the in-memory entry might still be looked up (Requirement
5, Acceptance 4); its mere existence is never treated as evidence that a reuse is valid.
This keeps "is this commit still reusable" answerable from process memory alone, which
composes directly with Decision 2 — since the cache never outlives its own goroutine, there
is nothing that would need to trust a ref's content after the fact.

Because refs are pure reachability plumbing, cleanup is unconditional rather than
selective: at the end of a worker invocation (any exit path, via `defer`), every ref this
invocation's cache created is deleted — nothing will ever look up this invocation's chain
again once it's gone (Decision 2), so there is no "still matchable" case left to preserve
by keeping a ref around longer.

### 4. Ref namespace is per-trainKey, enabling a safe crash-recovery sweep

`refDir` is `refs/fabrik/merge-train-prefix/<sha256(trainKey)>` — one directory per
trainKey, independent of the pinned base SHA. At the *start* of a worker invocation (before
any lookup or record), `sweepStaleRefs` deletes everything under its own `refDir` — a
defensive mop-up for a prior invocation that crashed between creating a ref and reaching
its own deferred cleanup (bounded to at most one leaked episode per trainKey, since a clean
run always reaches its own `defer`).

This is provably safe against a concurrently-running sibling: the per-`(repo, base)`
in-flight guard (`mergeTrainInFlight`/`EnterRepoWorker`, ADR-1648) already guarantees no
other goroutine is using *this exact* trainKey right now, and a different trainKey (a
sibling partition sharing the same bare clone) hashes to a different `refDir` — the sweep
can structurally never delete a live sibling's refs, regardless of how many `(repo, base)`
partitions of the same repo are running concurrently.

### 5. `forgetPoisonerResolutions` is explicitly not rewired to consume this cache

`forgetPoisonerResolutions` (ADR-1834) already reconstructs a trial-so-far state to
replay against, by re-merging `red[:idx]` in a disposable worktree — a candidate
beneficiary of this cache, since it currently pays for that reconstruction with N real
`git merge` calls (cheap, rerere-assisted, no Claude, but not free). This issue's Scope
names the assembly paths (main-loop re-form, bisection) as what needs prefix reuse; it does
not name this hygiene helper, and rewiring it would mean threading `trialParams`'
`prefixCache` into a call site that runs *after* the poisoner has already been ejected and
the batch's trial artifacts are already gone — a different lifecycle than the one this ADR
covers. Deferred as a genuine future optimization, not an oversight.

## Consequences

- `assembleTrialBranch`'s doc comment and behavior change: it no longer unconditionally
  forks off `p.baseSHA` — it forks off the longest matching prefix's commit when one
  exists, falling back to `p.baseSHA` exactly as before when the cache is nil (disabled,
  or the `trainValidateFn` test seam) or finds no match. This is the single call site every
  re-assembly path (main-loop re-form, every bisection sub-trial) shares, so the reuse is
  automatic everywhere `assembleTrialBranch` is called — no caller-side change was needed
  in `bisect`, `handleRedBatch`, or `runMergeTrainWorker`'s re-form loop beyond
  constructing and threading the cache itself.
- A wrong prefix match (a hypothetical hashing or matching bug) is bounded by the existing
  CI trust boundary (Requirement 7, unchanged): a reused prefix commit is pushed as a new
  trial and validated by CI exactly like any freshly-built one. The worst case is a wasted
  CI run on a wrongly-composed trial, never a silently-landed incorrect result — the same
  trust-boundary framing ADR-1834 relies on for rerere replay.
- The shared bare clone across concurrent `(repo, base)` partitions (ADR-1648) now also
  receives ref writes from this mechanism. Unlike ADR-1834's accepted unmutexed `rr-cache`
  race, this is not a similar risk to accept: Decision 4's per-trainKey ref namespacing
  means two partitions' ref writes never touch the same path, so no serialization is
  needed here.
- Two new test-only observation seams exist purely to make Acceptance 1/2/6 checkable
  without timestamp or SHA-identity tricks: `trainPrefixLookupHookFn` (matched length,
  total length, and member numbers at the start of every prefix lookup) and
  `trainMergeAttemptHookFn` (fires once per member an assembly actually attempts to
  merge). The second exists because ADR-1834's rerere replay can make the Claude-invocation
  counter alone insufficient to prove "the merge never ran at all" versus "the merge ran
  but its conflict resolved for free" — they are independent, and the merge-attempt hook
  is the literal, rerere-immune "zero merges" signal Acceptance 1 asks for. Both are nil in
  production (zero cost) and a `mergeTrainPrefixReuseDisabledForTest` flag (mirroring
  `mergeTrainQueueSortDisabledForTest`, ADR-1833) lets a test demonstrate the reuse is
  non-vacuous by disabling it and observing the merges return (Acceptance 6).
- `docs/state-machine.md` gains a new subsection describing the mechanism, and the
  `USER_GUIDE.md` "Merge Train / Queued" section's existing rerere paragraph gains a
  sibling sentence: unaffected merge work itself, not just conflict resolutions, is also
  reused across bisection and re-form.
- No new configuration toggle: the issue's requirements never call for one, and — as with
  ADR-1834 — a cache that reuses nothing when nothing matches has no downside an operator
  would want to opt out of. `mergeTrainPrefixReuseDisabledForTest` exists solely for the
  AC6 non-vacuousness test and is never exposed as a CLI flag or config key.

## Rejected Alternatives

- **A structural (member-list) index instead of a hash chain.** See Decision 1 — rejected
  because a hash-chain lookup is a single map access per position, and its ref-naming
  falls out for free, whereas a structural index would need its own comparison and
  ref-naming scheme built separately.
- **An Engine-level cache persisted across polls/restarts, with refs as the durability
  mechanism.** See Decision 2 — rejected as materially more complexity (cross-invocation
  staleness tracking, harder reasoning under ADR-1648's concurrent partitions) for a
  benefit the issue's own evidence never demonstrates a need for.
- **Treating a ref's mere existence as valid-for-reuse evidence**, avoiding an in-memory
  index entirely and relying on `git for-each-ref` to reconstruct the chain after a
  restart. Rejected — see Decision 3: this would either need restart-safety re-verification
  logic per Requirement 6 (checking a resurrected ref's base/member SHAs are still current)
  that duplicates the in-memory map's job, or risk trusting a stale ref outright. The
  simpler goroutine-scoped design (Decision 2) makes this question moot by construction.
- **Rewiring `forgetPoisonerResolutions` to consume this cache.** See Decision 5 —
  deferred as out of this issue's stated Scope, not rejected outright; a plausible future
  follow-up.
- **A `merge_train_prefix_reuse`-style configuration toggle.** Rejected: no scenario
  in the issue's requirements or acceptance criteria calls for an operator-visible
  opt-out, mirroring ADR-1834's identical reasoning for rerere.
