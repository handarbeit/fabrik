# simgh fidelity contract

This document is a deliverable, not a footnote. `simgh` is a fake, and the
failure mode a fake introduces is not a red test — it is a **green** one: a
scenario that passes because the model is wrong in the same direction the code
is wrong, or because the model never produces the state that would have exposed
the bug.

So every place `simgh` knowingly departs from real GitHub is recorded here,
with what the divergence costs. If you are about to rely on a sim-backed test
to cover a subtle behaviour, check this file first. If you change the model,
update this file in the same commit.

Each entry is labelled:

- **Modelled** — reproduced faithfully enough to test against.
- **Simplified** — present, but coarser than GitHub. The listed risk is real.
- **Absent** — not modelled at all. A test cannot cover it here.

---

## Git-derived answers

### Mergeability — **Modelled**

`FetchPRMergeable` / `FetchPRMergeableFields` run a real `git merge` of the head
branch into the base branch, in a throwaway detached worktree, and report the
result. There is no way for a test to declare that a PR conflicts; it must
construct commits that genuinely do.

The same `tryMerge` helper serves both the read-only probe and `MergePR`, so the
two cannot disagree — a model that could report a PR mergeable and then fail to
merge it would be worse than useless.

**Risk:** low. The main residual gap is that GitHub's merge is computed
server-side against its own snapshot of both refs, whereas the sim recomputes on
every read. In the sim a probe is always current; on GitHub it can be stale.
That is covered separately by the recompute window below.

### Merge commits — **Modelled**

`MergePR` writes a real two-parent merge commit onto the base ref, with `--no-ff`
so the merge commit exists even when the head is a strict descendant of the
base. That matches GitHub's default *Create a merge commit* strategy.

**Simplified:** squash and rebase merge strategies are **absent**. `MergePR`
takes no strategy argument (production's does not either), so every merge is a
merge commit. A scenario that depends on squash-merge history shape cannot be
written here.

**A merge that would write no commit is refused, not recorded.** `git merge
--no-ff` prints *"Already up to date."* and exits **zero** when the head is
already contained in the base, leaving the tip untouched — the state a second
merge of the same branch reaches. `tryMerge` compares the result against the
pre-merge base tip and refuses on the commit path (surfaced through `MergePR`
as `gh.ErrNotMergeable`), because publishing an unchanged ref would flip
`merged = true` having written no merge commit at all, silently contradicting
the guarantee stated above.

The read-only probe is deliberately **not** affected: nothing-to-merge is
genuinely not a conflict, so `FetchPRMergeable` still reports true. GitHub
behaves the same way — it reports no conflict, and its merge endpoint declines
to manufacture an empty merge commit rather than calling the PR unmergeable.

**Risk:** low, and it is the conservative direction — a scenario gets a loud
refusal rather than a false success. What is *not* verified against a recorded
response is GitHub's exact status code and message in this corner; only the
shape (refuse, do not fabricate) is modelled.

**A PR retargeted after the gate cleared it is refused, not merged.** `MergePR`
self-gates on the derived `mergeable_state` before touching git (see
[`mergeable_state`](#mergeable_state) below), and that gate necessarily drops
both locks: it takes `mu`, then `gitMu`, in turn, because the package's
lock-ordering rule forbids holding one across the other. A concurrent
`UpdatePRBase` can therefore land between the state the gate cleared and the
merge that follows, retargeting the PR at a base whose required contexts and
mergeability were never evaluated — the gate's whole purpose defeated silently.
`MergePR` snapshots head and base before the gate, re-reads them after, and
refuses with `gh.ErrNotMergeable` if they moved.

**Divergence:** GitHub has no such window — its merge endpoint re-checks
server-side, atomically, so a retarget is either fully before or fully after the
merge and never produces a refusal of this kind. The sim's compare-and-swap is
an approximation chosen because it needs neither lock held across the gate, and
so keeps the lock-ordering invariant that makes the two-tier design deadlock
free everywhere else. **Risk:** low. It is conservative (a refusal a scenario
retries, never a merge onto an unvetted base), and the window it guards can only
be opened by a scenario's *own* concurrent writer. An ABA retarget — away and
back again within the window — is not detected, since the snapshot compares
values rather than a version counter.

**What the compare-and-swap does *not* cover: the branches' contents.** It
compares which head and base the PR points at, not what they contain. A
`SeedCommit` onto either branch, or a check run seeded against the new head SHA,
lands inside the same window and is not caught — so the `mergeable_state` the
gate cleared can be stale by the time the merge runs, even though the refs
themselves never moved.

Two consequences, and they differ in direction:

- **The merge itself is safe.** The trial merge and the real merge run the same
  `tryMerge` helper against the tree as it is at merge time, so a conflicting
  commit landing in the window produces a reported conflict, never a merge that
  silently did the wrong thing.
- **The CI half of the gate is not re-validated, and here the sim is *laxer*
  than reality.** A required check going red in the window leaves the sim
  merging on the `clean`/`unstable` verdict the gate computed a moment earlier.
  Real GitHub re-enforces branch protection server-side at merge time and would
  refuse.

**Decision: document, do not model.** Closing this gap would mean modelling
GitHub's server-side re-check — the sim recomputing `mergeable_state`
atomically at merge time the way GitHub's merge endpoint does. That is a
deliberate non-goal, not an oversight: **this mirrors production's own
exposure.** `github.Client.MergePR` (`github/prs.go:1114`) reads `mergeable`,
then `FetchPRMergeableFields`, then `PUT .../merge` with only a `merge_method`
— GitHub's merge endpoint accepts an optional `sha` for exactly this
compare-and-swap and production does not send one. So the engine's gate verdict
is equally stale against real GitHub; what saves it there is GitHub's own
server-side re-check, which the sim has no equivalent of. Making the sim stricter
would model a guarantee production does not actually have, which would make it
*less* faithful, not more — the same reasoning applied to the recompute-window
double-drain below. If a future scenario genuinely needs "a required check went
red mid-merge" modelled (#1452's own authorship is the most likely source), that
is new work to scope separately, not a silent reopening of this decision. See
#1498.

**Risk:** low, and bounded by construction — the window only exists for a
scenario that mutates a repo concurrently with its own `MergePR` call. A
scenario that wants to exercise "a required check went red mid-merge" cannot do
it here; that needs the server-side re-check the sim does not model.

### Trial merges leave unreachable objects, bounded by a periodic gc — **Modelled, bounded**

A read-only mergeability probe runs a real `git merge --no-ff` and then discards
the result by not moving any ref, so the merge commit it wrote stays in the
object store with nothing pointing at it. Mergeability is recomputed on every
read, so a long multi-poll scenario would otherwise accumulate orphaned
objects for the life of the test process — worst exactly where merge-train
bisection lives (#1452), which re-probes mergeability across a bisection
sequence on top of a per-trial-SHA `FetchCheckRuns` poll.

**Decision:** bound it with a periodic housekeeping `git gc`, not by
restructuring the probe to avoid writing a commit object in the first place
(e.g. `git merge-tree`). The latter would be more faithful in the sense of
"no garbage at all," but requires git ≥ 2.38 (no version floor exists in this
codebase today) and has no precedent here — and, more importantly, it would
touch `tryMerge`'s single shared implementation, which is deliberately used by
both the probe and `MergePR` so the two can never disagree about whether a PR
merges cleanly. Periodic `git gc` is pure post-hoc housekeeping on
already-unreferenced objects instead: it runs on every 25th probe
(`probeGCThreshold`), under the same `gitMu` already held, and is invisible to
every read — it cannot change an answer, only reclaim disk and inodes. The
threshold has no real-GitHub correlate to model against; it is a bookkeeping
judgement call, tuned low enough that `TestMergeableProbeBoundsUnreferencedObjects`
runs in seconds.

It runs `git gc --quiet --prune=now` rather than plain `git gc`, which by
default only expires unreachable objects older than a 2-week grace period —
protection against pruning something a concurrent writer elsewhere in the repo
might still need. That race cannot happen here: `gitMu` serialises every git
subprocess call against one bare repo, so nothing else can be mid-write when
the gc runs, and every object it reclaims was orphaned the instant its own
probe returned, never "possibly still wanted."

**Risk:** low. Bounded rather than unbounded, and it is disk and inode cost
rather than a wrong answer either way — the objects are unreachable, so no
read can observe them regardless of when (or whether) the gc runs. Fixed in
#1498; `TestMergeableProbeBoundsUnreferencedObjects` pins that the loose-object
count is reclaimed after crossing the threshold and that the counter resets.

### A PR whose head branch is gone errors — **Simplified**

`gitFacts` fails outright when a PR's head branch no longer exists in the
backing repo, so `FetchPRMergeableFields`, `FetchPRMergeableState`,
`FetchPRDetails`, and `MergePR` all return an error rather than resolving to a
determinate `mergeable_state`.

**Unverified:** what real GitHub reports for a PR whose head ref was deleted is
not captured here — plausibly `unknown`, plausibly the PR is closed first. The
model errors rather than guessing a state, which is loud rather than silently
wrong, but it means a scenario cannot exercise the deleted-head-branch path at
all. Note the deliberate asymmetry: `headSHA` itself treats a missing branch as
a legitimate state (empty string, no error) — it is `gitFacts` that decides
mergeability cannot be computed without one.

### Commits behind — **Modelled**

`FetchCommitsBehind(base, head)` is `git rev-list --count <head>..<base>` —
commits on the base that the head lacks. This matches production, which reads
`behind_by` from the REST compare endpoint (`compare/base...head`,
`github/prs.go`).

### Head SHAs — **Modelled**

A PR's head SHA is resolved from the backing repo on every read, never frozen at
PR creation. Seeding a new commit on the head branch changes it immediately, the
way a push does. This matters because check runs are keyed by SHA: a stale head
SHA would silently read the wrong CI results.

---

## `mergeable_state`

### The six derived values — **Modelled, with a stated precedence**

`FetchPRMergeableState` derives `clean`, `unstable`, `blocked`, `behind`,
`dirty`, and `draft` from the whole model, in this order:

1. `unknown` — the PR is merged or closed, or a recompute window is pending
2. `draft` — the PR is a draft
3. `dirty` — the trial merge genuinely conflicts
4. `behind` — the base requires up-to-date heads *and* has advanced
5. `blocked` — a required context is missing, pending, or failing
6. `unstable` — a non-required context is pending or failing
7. `clean` — none of the above

All six matter because the engine branches hard on them:
`github.MergeableStateAccepted` admits only `{clean, unstable}`, and
`github.ErrNotMergeableCI` fires specifically on `blocked`/`unknown` and must
**not** be routed into the `fabrik:rebase-needed` path. A model that could only
say dirty-or-clean would make four values unreachable and ADR-072's merge-safety
incident unreproducible here.

**Simplified:** this precedence is the sim's own deliberate choice, **not** a
reverse-engineering of GitHub's internal algorithm for every combination of
conditions. A draft PR that also conflicts resolves to `draft`, not `dirty`. If
a scenario depends on a specific combination, verify the real behaviour against
a recorded GitHub response rather than trusting the sim's ordering.

**Risk:** medium, and concentrated in *which* state an untested combination
resolves to rather than in the ordering itself. Each single-condition case has
its own test, and `mergeable_precedence_test.go` additionally pins every
adjacent link in the chain — draft > dirty > behind > blocked > unstable >
clean — with two conditions arranged at once, so the order is constrained
transitively rather than assumed. What those tests cannot tell you is whether
this order is the order *GitHub* would use for the same combination; that
remains a design choice, so verify against a recorded real response before
relying on a combination in a load-bearing scenario.

### `has_hooks` and `unknown`-from-slowness — **Absent**

`has_hooks` is never produced. Production treats it as not-accepted
(conservatively falling through to per-check classification), and no scenario
needs to distinguish it from `blocked`. `unknown` is produced only for
merged/closed PRs and during a seeded recompute window — never spontaneously,
the way GitHub emits it while it is simply slow.

### `behind` requires branch protection — **Modelled, deliberately**

This is the model's most consequential judgement call, so it is called out
explicitly.

Real GitHub reports `behind` **only** where branch protection's *"Require
branches to be up to date before merging"* is enabled. Without that setting, a
conflict-free but out-of-date PR is `clean`. `simgh` mirrors this: `behind`
requires `SeedRequireUpToDate(repo, branch, true)`.

A model that reported `behind` whenever `commitsBehind > 0` would be simpler,
but would make `clean` nearly unreachable for any scenario where the base
branch moves, and would push the engine into the rebase path constantly — a
confidently-wrong test bed for exactly the logic most likely to have bugs.

### `mergeable: null` — the recompute window — **Modelled, seeded**

Production's `FetchPRMergeable` returns `*bool`, and GitHub returns `null` while
it is still recomputing mergeability — typically shortly after a push or PR
creation. `MergePR` returns `ErrNotMergeable` on a null read, and that window is
what once made a healthy issue (#1087) appear wedged.

`simgh` makes the window reachable rather than merely documented:
`PRSeed.MergeableRecomputeReads` (or `SeedMergeableRecomputePending`) opens it
for a given number of reads. While open, `mergeable` reports nil and
`mergeableState` reports `unknown` — together, as GitHub reports them. Each read
drains one; afterwards the real git-derived answer surfaces.

**Simplified:** the window is a **read counter, not a timer**, and it opens only
when seeded. The sim never opens one spontaneously after a commit is pushed to a
head branch, which is when GitHub actually does. A scenario that pushes and then
immediately reads will see a resolved answer here and might not on GitHub.

---

## Check runs and commit statuses


**Each read drains one unit, including the two single-field accessors.**
`FetchPRMergeable` and `FetchPRMergeableState` are independent calls into
`FetchPRMergeableFields`, so a caller that wants both — which is the natural
thing to want, and what `FetchPRDetails` sanctions for itself — burns two units
rather than one, and can see the flag and the state resolve at different points.
Real GitHub reports both from a single response, so they resolve together.

**Risk:** low but real for a scenario that seeds a *short* window (1–2 reads) and
then reads both fields; the window drains faster than the scenario intends. The
sim does not share a drain between them because doing so would require the two
accessors to know they are part of one logical read, which the interface does not
express. Prefer `FetchPRMergeableFields` when a scenario needs both, exactly as
production's single-PR endpoint does. See #1498.
### Two separate collections — **Modelled**

Check runs (`FetchCheckRuns`) and classic commit statuses (`FetchCombinedStatus`)
are genuinely distinct SHA-keyed collections. Production distinguishes them: a
required context can be posted through the classic Statuses API rather than as
an Actions check run, and `FetchCombinedStatus` is Fabrik's only visibility into
that case (ADR-933). Both feed the `mergeable_state` derivation.

### Required contexts — **Simplified**

Branch protection's required-check configuration is modelled as a per-branch
list of context names (`SeedRequiredContexts`). Real branch protection is richer:
required checks can be scoped to an app ID, wildcards and rulesets exist, and
the configuration is readable only with elevated permissions (which is why
Fabrik has its own `RequiredStatusContexts` config in the first place).

**Risk:** low for the engine's purposes — it only ever asks "is this context
required", which the list answers.

### Verdict mapping — **Simplified**

A check run's `neutral` and `skipped` conclusions are treated as success,
matching GitHub's treatment of them as non-blocking. A non-required context that
is merely *pending* yields `unstable`, not `clean`.

### Repeated contexts reduce latest-wins, not worst-of — **Modelled**

A context reported more than once for a SHA is reduced to **one** verdict before
the derivation looks at it, and the rule is *latest wins within each collection*:

- **Check runs** reduce by **highest ID**, matching production's
  `latestCheckRunsByName` (`github/checkruns.go`) and real branch protection,
  which clears a required check once its newest run passes. IDs are the
  ordering, not seed order — a scenario may seed a rerun before the run it
  supersedes.
- **Commit statuses** reduce to the **last one posted** for a context, because
  the classic Statuses API supersedes rather than accumulates.

This was originally worst-of, and that was wrong rather than merely
conservative: it classified a `check failed → rerun passed` flow off the failed
run, reporting a PR permanently `blocked` that GitHub calls `clean`. It also
defeated the check-run ID reservation machinery in `seed.go`, which exists
precisely so that flow is representable, and contradicted production's own
classifier — the two doc comments disagreed about how GitHub behaves.

**Risk: low.** The rule now matches production's classifier and the endpoint
semantics it was derived from.

### A context in *both* collections keeps the worse verdict — **Simplified**

Worst-of survives in exactly one place: a name reported both as a check run and
as a classic commit status. Real GitHub matches required contexts by name across
both spaces, and what it does when the two disagree has **not** been verified
against a recorded response here — the model takes the worse verdict as a
conservative choice of its own.

**Risk: low, but unverified.** Confirm against a recorded real response before
building a scenario that depends on it;
`TestMergeableStateCrossCollectionKeepsWorstVerdict` is the test to change if it
turns out otherwise. Simplest avoidance: do not seed one context name into both
collections at one SHA.

---

## Reviews

### `ResolveReviewThread` marks only the first comment on a thread — **Simplified**

A thread is modelled as comments sharing a `reviewThreadID`, and resolving it
flips `threadResolved` on the first match rather than on every comment in the
thread. Board projections surface unresolved thread comments individually, so a
thread with more than one comment reads as partially unresolved after being
resolved. **Risk:** low — the engine's progress detection reads the resolved
*count*, and no scenario in the downstream chain seeds multi-comment threads —
but a scenario that does would see the wrong shape. See #1498.


### `reviewDecision` under branch protection — **Simplified**

`FetchPRReviewDecision` returns `""` unless the base branch has a seeded
approval requirement (`SeedRequiredApprovals`). This is the important part:
GraphQL's `reviewDecision` is **null** unless branch protection actually
requires reviews, which is exactly why the engine's authoritative review gate
(ADR-1250) prefers `reviewDecision` where it exists and falls back to its own
no-`CHANGES_REQUESTED` computation otherwise. A model that always returned a
decision would hide that fallback entirely.

With a requirement configured, the decision is computed from each reviewer's
**latest** `APPROVED`/`CHANGES_REQUESTED` review; `COMMENTED` and dismissed
reviews do not participate.

**Simplified:** code-owner requirements, review dismissal on push, and stale-review
invalidation are **absent**. `DISMISSED` is not a state the model produces.

**The decision shares one reduction with `FetchPRReviews`.** Both roll up
through `latestReviewsByAuthor`, so a `DISMISSED` submission supersedes that
author's earlier verdict in the decision exactly as it does in the review list.
Filtering the raw history down to `APPROVED`/`CHANGES_REQUESTED` *before*
collapsing would skip a dismissal instead, leaving a dismissed approval counted
— and `reviewGateAuthorityVerdict` trusts an `APPROVED` decision outright, so a
scenario that dismisses an approval and expects the landing gate to hold would
watch it clear.

**A zero approval requirement is refused, not modelled.** `SeedRequiredApprovals`
rejects a non-positive count. With zero, the approvals-satisfied branch is
reached vacuously and a PR with no reviews at all reports `APPROVED` — not a
reading real GitHub produces. The state that call would seem to express, "no
review requirement", is already representable and is the *absence* of the call:
an unseeded branch returns `""`, which is the case the engine's fallback above
depends on. Refusing keeps the two from collapsing into one silently.

GitHub's `required_approving_review_count` does accept `0` alongside "require a
pull request before merging", and what `reviewDecision` reports in that
configuration is **not verified here** — which is the second reason to refuse
rather than guess a semantic for it.

### `FetchPRReviews` returns the latest review per author — **Modelled**

Not the raw submission history. `github.Client.FetchPRReviews` reads a REST
endpoint that returns every submission and reduces it to one entry per author,
so its result matches GraphQL's `latestReviews`; the engine's review-gate call
sites consume it assuming that reduction already happened. The model reproduces
the rule exactly, including its one exception: a `COMMENTED` follow-up never
supersedes an author's existing formal verdict (`APPROVED`,
`CHANGES_REQUESTED`, `DISMISSED`), because GitHub treats a comment as
informational rather than a state transition. Only an author's *first*
submission being `COMMENTED` makes it their entry.

The board projection's `LinkedPRReviews` applies the same reduction, since
production sources that field from `latestReviews`, which is one-per-author by
definition. Two reads of one model must not report two verdicts — and
`FetchPRReviewDecision` has always reduced correctly, so a raw list here would
have put the package in contradiction with itself.

### Review requests and self-submitting bots — **Modelled**

`FetchPRReviewRequests` returns only reviewers actually requested. The model
never synthesises an entry for a self-submitting bot (Pruefer, Gemini,
CodeRabbit, Copilot), because real GitHub never lists them either — they are
never formally requested. That absence is load-bearing: it is the whole reason
stages must declare `expected_reviewers` (ADR-1283).

Bot classification reuses production's own `github.IsBotLogin`, rather than
reimplementing the heuristic.

---

## Auto-merge and merge queue

### Native auto-merge — **Simplified (flag only)**

`EnablePullRequestAutoMerge` / `DisablePullRequestAutoMerge` set and clear a
flag, surfaced as `PRDetails.AutoMergeEnabled`. Enabling it is refused when the
repo's `AllowAutoMerge` is false, as GitHub refuses it.

**The sim never acts on the flag.** It does not watch checks and merge the PR
when they go green. A scenario that enables auto-merge and then turns CI green
will find the PR still open; it must call `MergePR` to make the merge happen.

**Risk:** medium. Any engine behaviour that depends on GitHub completing an
auto-merge asynchronously cannot be tested here.

### Merge queue — **Simplified (bookkeeping only)**

`EnqueuePullRequest` / `DequeuePullRequest` record queue membership and assign a
position, and `EnqueuePullRequest` rejects a stale `expectedHeadOID` (as GitHub
does, which is how a race with a concurrent push is caught).

**The queue never advances.** It does not run checks, never reorders itself,
never merges anything, and dequeuing does not renumber the PRs behind it.
`MergeQueueEntry.State` is always `QUEUED`; `AWAITING_CHECKS`, `MERGEABLE`, and
`UNMERGEABLE` are never produced.

**The `expectedHeadOID` check is not atomic with the enqueue.**
`EnqueuePullRequest` resolves the head SHA under `gitMu`, releases it, then takes
`mu` to record membership — so a concurrent push landing between the two would
let the enqueue proceed against a head that no longer matches. GitHub performs
that compare-and-swap server-side, atomically; the sim cannot.

This is deliberate rather than overlooked. Closing it would mean holding `gitMu`
and `mu` simultaneously, which the package's locking invariant forbids outright
(see `sim.go`); that invariant is what makes the two-tier design deadlock-free
everywhere else, and it is not worth trading for a window only a scenario's own
concurrent seeding can open. `MergePR` does **not** have this gap for the merge
itself: its trial merge and its real merge run the same `tryMerge` helper, so if
the tree changed underneath it the merge fails and the model reports the
conflict rather than recording a merge that did not happen. It *does* share the
gap for its gate's CI verdict, which is a separate matter — see "A PR retargeted
after the gate cleared it" under [Merge commits](#merge-commits) for what that
covers and what it leaves open.

Two risk ratings apply here, at different scopes — the narrower one sits inside
the broader one, so they do not compete:

**Risk — merge-queue coverage generally: high.** The queue is bookkeeping only,
so treat merge-queue coverage here as **absent**. Any scenario whose outcome
depends on the queue advancing, reordering itself, or merging anything cannot be
written against this model, and a green sim-backed test says nothing about it.

**Risk — the `expectedHeadOID` race specifically: low.** This is a strictly
narrower window *within* that already-absent surface, so it adds little on top:
to hit it, a scenario would have to push to a PR's head branch from one goroutine
while enqueuing it from another.

---

## Rate limits and transport

### Rate limiting — **Injectable per call, static as a budget**

Two halves, and they behave differently.

**Per-call rate-limit failures are injectable.** Wrap a `Sim` with
`Instrument` and any interface method can be made to return a rate-limit,
secondary-rate-limit or abuse-detection error — once, N times, always, on the
Kth call, or only for calls matching a predicate. Use the `ghfault`
constructors (re-exported as `ErrRateLimit`, `ErrSecondaryRateLimit`,
`ErrAbuseDetection`, `ErrTooManyRequests`, …) rather than an ad-hoc
`errors.New`: the engine classifies these by substring match against
`err.Error()` (`engine/item.go`'s `rateLimitErrorPatterns`), so an unrecognised
message exercises the *escalation* branch while the scenario believes it is
exercising the *defer* branch. `engine/simgh_fault_classification_test.go` pins
each constructor against the real, unexported classifier, so a rewording on
either side fails loudly.

**The budgets themselves remain static.** `RateLimitStats` reports whatever
`WithRateLimits` or `SeedRateLimits` set, and the numbers never deplete as
calls are made. Nothing counts requests. A scenario that wants the engine's
budget-ratio thresholds (`engine/backoff.go` consumes remaining/limit as a
ratio) must *script* the budget with `SeedRateLimits`, not expect it to fall on
its own.

**`RateLimitStats` is the one method fault injection cannot fail.** It has no
`error` return (`engine/interfaces.go`), so there is no channel through which
an injected failure could surface. Registering a fault against it **panics**
rather than sitting inert — a silently-inert fault is a false-pass generator,
and the whole point of this layer is to remove those. Scripting the budget is
the controllability that surface actually has.

Timestamps (`Reset`, `UpdatedAt`) still come from the injected clock, so a test
controlling time sees coherent values.

**Risk — retry-after: absent.** No method returns a `Retry-After` header or its
equivalent, because the model has no transport. Any engine behaviour keyed on a
server-supplied backoff duration is unreachable here.

### Scripted verdicts are unconstrained by the repository's real state — **Divergence, deliberate**

Every scripted surface — check runs, commit statuses, reviews, review requests,
required contexts, required approvals — is set directly, with no cross-checking
against anything else in the model. That is the point, and it is also a way to
write a test that passes against a world GitHub could never produce.

Concretely, a scenario can construct all of these, and the model will report
them without complaint:

- A **required context that no check run or status ever posts**, or conversely a
  passing check named for a context branch protection does not require.
- A **review from an author who is not a collaborator**, or an `APPROVED`
  review on a PR whose author is that same person — GitHub forbids self-approval.
- `SeedRequiredApprovals(repo, branch, 5)` on a repo with **one** reviewer, so
  `reviewDecision` reports `REVIEW_REQUIRED` forever.
- A **`DISMISSED` review with no dismissing actor** and no corresponding
  dismissal event.
- Check runs on a **SHA that is not the head of any branch or PR**, which real
  CI would have had no commit to run against.

None of these are bugs in the model: refusing them would make large parts of
the engine's own defensive handling untestable, which is the opposite of why
this layer exists. But a green sim-backed test proves the engine behaves *given
that input*, not that the input is one GitHub would ever hand it. When a
scenario's setup starts to look exotic, check it against a real board before
concluding the engine is correct.

### Clock-driven schedules have no real-GitHub correlate — **Divergence, deliberate**

The `Seed*At` / `Seed*After` methods enqueue a mutation for a future instant on
the injected clock: "this SHA goes red at T", "the reviewer responds forty
minutes in". Nothing on GitHub works this way — CI finishes when it finishes,
and a reviewer responds when they respond. A schedule is a test instrument, not
a model of anything.

Two properties are worth stating because scenarios will rely on them:

- **A step is a pending mutation applied on read, not a read filter.** When its
  time arrives it *writes into the model*, permanently. So repeated reads at a
  single clock instant are stable, and — the reason for the choice — engine
  mutations to the same surface remain observable afterwards. A filter
  overriding `reviewRequests` would mask `AddReviewRequest`, which is the bot
  re-prompt ladder's own mutation (ADR-1283): the engine would make the call,
  the model would accept it, and the next read would deny it had happened.
- **Steps fire at `at <= now`, in time order, and in seeding order within one
  instant.** Nothing else about their relative ordering is contractual.

Because a step is a write, draining it is a side effect of *reading*. Every
read path that touches a scheduled collection therefore calls `drainCI` or
`drainReviews` first (`schedule.go`). A scenario that reads through a path
which forgot to drain would see stale state — which is why
`TestScheduleVisibleThroughEveryReadPath` builds a **fresh `Sim` per read
path**, so one path's drain cannot cover for another's.

### Sequencing is clock-driven; read-count sequencing survives in one place — **Modelled, with a caveat**

The general mechanism is the injected clock, not a read counter, because **one
poll is not one read**. `settleAwaitingCIScan` primes the store with a live
check-run read (`RefreshCheckRunsLive`) and the handler chain it then runs
reaches `checkCIGate`, which reads check-run state again: two reads of one SHA
in a single poll, before `MaxConcurrent` workers are considered. Worse, the
first of those reads is guarded on the engine having a `boardcache` wired,
which is a harness configuration decision rather than an engine invariant. A
read-count sequence would therefore not correspond to poll boundaries at all,
and would shift under an edit to the handler chain — the thing under test.

**The exception is `prRecord.mergeableRecomputeReads`** (see *`mergeable: null`
— the recompute window* above), which is kept as a read counter because it
models a genuine GitHub behaviour — a recompute that resolves after some reads
— rather than a test's notion of elapsed time.

**Risk — the single-reader caveat is live for that field.** A read-count
sequence is only reproducible when exactly *one* call site reads the surface,
and `FetchPRMergeableFields` has three: `FetchPRMergeable`,
`FetchPRMergeableState`, and `MergePR`'s own gate (`prs.go`). A scenario that
seeds `MergeableRecomputeReads: 2` and then calls `MergePR` will find the
window drained by the gate's own read. Count the reads the code actually makes,
not the polls you intend.

### `reviewDecision` is derived, not scriptable — **Modelled, deliberately**

There is no `SeedReviewDecision`. `FetchPRReviewDecision` computes its answer
from `latestReviewsByAuthor` plus `requiredApprovals`, sharing the reduction
with `FetchPRReviews` so the two reads cannot disagree (see *`reviewDecision`
under branch protection* above). Exposing a direct setter would reintroduce
exactly the bug that sharing exists to prevent: two reads of one model
reporting two different verdicts. Script `SeedReview` (or `SeedReviewsAt`) and
`SeedRequiredApprovals`; the decision follows.

### The mutation log records attempts, and its ordering is exact only within a goroutine — **Modelled, with a stated contract**

`MutationLog` (`mutationlog.go`) records **every intercepted call with its
outcome** — reads as well as mutations, failures as well as successes,
injected failures distinguished from model failures. Its name is narrower than
its contract, deliberately: a log of mutations that happened could not show
"N failed attempts followed by the success", which is the whole point of
asserting against an injected fault. `Mutations()` is the narrower view.

Entries are appended in two phases — a slot reserved *before* the underlying
call, completed after it. That gives:

- **Exact ordering within a goroutine.** Every real ordering assertion is of
  this kind: ADR-060's "`fabrik:awaiting-done` is the very first mutation" is a
  claim about one worker's sequential code.
- **Reserve-order, best effort, across goroutines.** Two workers whose calls
  interleave inside the model are ordered by which reserved its slot first.
  That is as close to a happens-before as an out-of-band log can get.

**Risk — do not write a cross-goroutine ordering assertion.** Two calls made
concurrently from different workers have no contractual order in the log, and
an assertion that happens to hold today is a flaky test. `Precedes` errors
rather than answering `false` when either predicate matches nothing, so a query
that has gone stale fails loudly instead of passing for the wrong reason — but
nothing can rescue an assertion that was never well-defined.

### Snapshot and restore — **Modelled, with quiescence assumed**

`Sim.Snapshot(stagingDir)` / `Restore(snap, baseDir, opts...)` round-trip the
whole model plus a directory copy of every backing bare repository;
`Instrumented.Snapshot` / `RestoreInstrumented` additionally carry the fault
schedule and the mutation log, so a restart scenario's GitHub does not quietly
heal and an ordering assertion may span the restart. The `Clock` is *not*
carried — it is never model-owned — and is re-supplied as an `Option`, matching
`New`.

**Simplified — snapshot assumes quiescence.** Git state and model state are
copied in two phases, and no lock is held across both; the package's
mu-before-gitMu invariant forbids it. Each half is internally consistent, but a
snapshot taken while engine goroutines are running may catch a mutation in one
half and not the other. Take one the way a restart scenario naturally would:
between polls, with nothing in flight.

**Risk — stale git worktree admin entries.** `git worktree add` runs with
`cmd.Dir = bareDir`, so git's admin entries live inside the copied directory
and hold *absolute* paths into throwaway checkouts under the **old** `baseDir`
— which `t.TempDir()` guarantees differs on every run. `Snapshot` runs
`git worktree prune` under `gitMu` before copying, which clears them.
`snapshot_test.go` asserts a git-derived answer (commits-behind, mergeable
state, and a real merge) recomputes correctly against the restored repository,
because that is what would actually fail if a stale entry survived.

### HTTP / GraphQL / REST wire behaviour — **Absent**

`simgh` sits at the `engine.GitHubClient` Go interface, not on the wire. Query
documents, mutation names, JSON field mappings, pagination, and status-code
handling are all invisible to it. This is a permanent, structural blind spot,
not a gap to be filled later — see [`../README.md`](../README.md).

### Webhooks — **Absent**

There is no webhook subsystem. `DeleteForwardingHooks` always succeeds for a
seeded repo because there is never anything to delete; a test cannot use it to
prove hooks were cleaned up. `boardcache`'s webhook delta functions are not
driven by this layer.

---

## Issues, PRs, and the board

### Shallow board reads return deep-phase fields — **Simplified**

`FetchProjectBoard` returns fully-populated `ProjectItem`s. Production's board
query is deliberately *shallow*: it fetches only what the engine needs to
pre-filter, and leaves `Body`, `URL`, `Author`, `Assignees`, `BlockedBy`, and
`Comments` empty until `FetchItemDetails` fills them in a second, deep phase
(`github/project.go`, the `itemNode` doc comment; pinned by
`github/fetch_board_test.go`). Labels *are* retained shallowly, for
`cleanupClosedIssueLocks`. The sim populates everything in one pass, and
`FetchItemDetails` is consequently closer to a refresh than a fill.

**Risk: medium — the highest-rated entry in this document, and the one most
likely to produce a confidently-green test.** The engine's admission logic
(`itemMayNeedWork`, `selectDeepFetchCandidates`) runs against the *shallow*
snapshot, so a scenario asserting that an item was admitted or skipped on the
strength of its body or comments would be reading a signal the engine does not
actually have at that point. The divergence is safe in the lax direction only:
the sim never withholds a field production would have supplied, so nothing
fails that would have passed.

A test that needs a deep-phase field should call `FetchItemDetails` before
asserting on it, even though the sim does not require it — that keeps the test
correct if this path is later narrowed to match production, rather than
silently vacuous. `TestAssigneesAreObservableFromBothPaths` does this.

Closing the gap means splitting `buildProjectItem` into shallow and deep
projections; it is left open here because it is a behavioural change to the
model rather than a documentation one, and belongs with the harness work that
first depends on the distinction (#1457).

### Comment edits bump the parent's updatedAt — **Modelled**

`UpdateComment` bumps the parent's `updatedAt`, as GitHub does when a comment is
edited — the issue's for an issue comment, the PR's for a PR or review-thread
comment. This is not incidental: the engine rewrites an existing stage comment
rather than posting a new one (`engine/comments.go`, `engine/dependencies.go`),
so an unbumped timestamp would hide a real edit from any read that watches
`updatedAt` to decide something changed. The PR side is observable too —
`buildProjectItem` folds a linked PR's `updatedAt` into the board item's, and
`ProbeProjectBoard` surfaces it as `LinkedPRUpdatedAt`.

Adding a *reaction* deliberately bumps nothing, matching GitHub, which does not
treat a reaction as an update to the parent.

### Assignees — **Modelled**

Stored per issue and surfaced on `ProjectItem.Assignees`. Settable from both
paths: `IssueSeed.Assignees` and `CreateIssue`'s `assignees` argument, the
latter matching production, which posts the field only when non-empty. This is
load-bearing rather than decorative — `engine/spawn.go` assigns every spawned
child issue to the configured user, so a child-spawn scenario asserts on a real
value. Assignee *permissions* (GitHub silently drops an assignee who lacks
repository access) are **absent**: the sim stores whatever it is given.

### PR-to-issue linkage — **Modelled**

`FindPRForIssue` and `FetchLinkedPR` match on the head branch `fabrik/issue-<N>`,
which is exactly how production discovers the link (a `pulls?head=owner:branch`
query). The `issueNumber` argument to `CreateDraftPR` is accepted and ignored for
linkage, as production ignores it.

Matching on a stored back-reference instead would be laxer than production, and
would let a test pass on linkage GitHub would never find.

When more than one PR shares the head branch, the **most recently created** one
wins. Production's query is `pulls?head=owner:branch&state=all&per_page=1`, and
that endpoint defaults to created-descending, so GitHub returns the newest. This
is not a hypothetical: Fabrik reuses `fabrik/issue-<N>`, so a PR closed without
merging leaves a stale record on the branch its successor reuses, and
`engine/prcreate.go` decides whether to open a PR on exactly this answer. The
board projection (`findPRByHeadLocked`) applies the same rule, so the two reads
can never report different linked PRs for one issue.

### Issue and PR numbers share one sequence — **Modelled**

GitHub allocates issue and pull-request numbers from a single per-repo counter,
so `#7` is either an issue or a PR and never both. The model uses one shared
sequence, and seeding an explicit number that the other kind already holds is a
seeding error.

This is load-bearing rather than cosmetic. GitHub's issue-comment endpoint is
itself shared between issues and PRs — which is why `AddComment` resolves a
number against both collections — so two independent sequences would let issue
`#1` and PR `#1` coexist and silently route a PR comment onto the same-numbered
issue. The engine posts stage output that way (`engine/pr.go`,
`engine/comments.go`), so the output would vanish from the PR while a test still
observed "a comment was posted" and passed.

### `FetchLinkedPR` omits `mergeable_state` — **Modelled, deliberately**

`FetchLinkedPR` returns `MergeableState: ""` and no mergeable flag, because
production reaches it through the pulls *list* endpoint, which omits the field.
Returning a computed value would let a test pass on a signal production never
receives.

**Simplified:** the list endpoint's `merged` field is also unreliable on real
GitHub — it reports `merged: false` for several seconds after a merge, which is
why `FetchPRMerged` exists as a separate single-PR call. The sim reports `merged`
truthfully from `FetchLinkedPR`. A regression that depends on that lag would not
be caught here.

### `FetchPRDetails` reports `mergeable_state`; `FetchLinkedPR` does not — **Modelled**

`FetchPRDetails` reads the *single-PR* endpoint, which does return
`mergeable_state`, so it reports the same derivation `FetchPRMergeableState`
does — recompute window included: a read here drains one, because on real GitHub
this is exactly the read that would.

It leaves `HeadRefName`, `BaseRef`, `Author`, and `Labels` zero, because
production's implementation does not parse them from that endpoint. `FetchLinkedPR`
is *not* consistent with that stance — it populates those fields even though the
list endpoint omits them. That inconsistency is known and deliberately left for
the harness follow-on (#1457), which may need them; nothing in the engine reads
them off a `FetchLinkedPR` result today.

### Closing keywords — **Simplified**

`FetchPRClosingIssues` scans the PR body with a regex covering
`close/closes/closed`, `fix/fixes/fixed`, `resolve/resolves/resolved` followed by
`#N`, plus the issue number passed to `CreateDraftPR`. Cross-repo references
(`owner/repo#N`) and full issue URLs are **not** matched, and GitHub's full
closing-keyword grammar is not reproduced.

### Merge auto-close — **Modelled**

Merging a PR closes the issues it references with a closing keyword, but **only**
when the merge lands on the repo's default branch — mirroring GitHub, and
mirroring why Fabrik must close issues itself on a non-default base (ADR-1096).

### Issue state casing — **Modelled**

The model stores GraphQL's uppercase enum (`OPEN`/`CLOSED`) and `FetchIssue`
converts to REST's lowercase (`open`/`closed`), matching the shapes production
reads from each API.

### Project identity — **Modelled**

Projects are keyed by `(owner, number)`, because Projects v2 is owner-scoped, not
repo-scoped. The `repo` argument on a board fetch is a query input, not part of
the project's identity.

### Archiving and re-adding a card — **Modelled**

`ArchiveProjectItem` sets a flag; archived cards are filtered out of every board
read (`liveItemRefs`, `ProbeProjectBoard`, the status batch) while the
underlying issue is untouched.

Re-adding a card that is already on the board **revives** it: un-archived, with
both the card's and the project's updated-at bumped. Both entry points route
through one helper (`reviveCardLocked`), because they had drifted into doing
complementary halves of it — seeding reset status and the timestamps but left
the card archived, so moving an archived card to a new column left it invisible
to every board read with no error; the runtime API cleared the flag but bumped
neither timestamp, so a card reappearing did not advance
`FetchProjectUpdatedAt`, which the engine's poll reads to notice the board
changed. The two differ in exactly one respect, deliberately:
`SeedProjectItem` moves the card to the named column, while
`AddProjectV2ItemById` leaves status alone, since re-adding a card already on a
board does not move it between columns.

**Re-adding a card that is already live is a true no-op** — neither timestamp
moves. `FetchProjectUpdatedAt` gates whether the engine's idle poll looks at the
board at all, so bumping here would be a wake signal real GitHub does not
produce, and one a scenario cannot tell from a real change. The seeding path
treats a *column move* as a change (it moves the card; the runtime API does
not), so it bumps for that and not for a re-seed into the same column. Same
convention as `AddLabelToIssue` and `AddReviewRequest`.

**Risk:** low, but note this is the sim's own choice rather than a behaviour
derived from a recorded real response — GitHub's `addProjectV2ItemById` against
an already-archived item has not been captured here. A scenario that turns on
whether re-adding un-archives should verify the real response first.

### Board projections read card state live — **Modelled**

A board read snapshots its cards under one lock acquisition and builds each
projection under later ones, so the snapshot is a set of *identities*, not a set
of *values*. `buildProjectItem` re-reads the card's own Status, kind, and
updated-at from the model at projection time, and drops a card that has been
archived or removed since the snapshot. Projecting from the snapshot instead
would report a stale column, or return an archived card on a board fetch, in
precisely the situation this package exists to make trustworthy — and would
contradict both the "never cached" guarantee at the top of `board.go` and R1.

`ProbeProjectBoard` does the same, and it matters more there: the probe is what
the engine's idle poll consults to decide whether anything changed, so a stale
column is a poll that does not notice work it should, and an archived card left
in the probe output is work the poll invents.

### Archived cards: board reads hide them, a direct ID read does not — **Simplified, unverified**

The two are deliberately asymmetric, and the asymmetry follows production's two
query shapes rather than an internal style rule:

- `FetchProjectBoard`, `ProbeProjectBoard`, `FetchProjectItem`,
  `LookupIssueProjectItem`, and `FetchProjectItemStatusBatch` all skip archived
  cards.
- `FetchProjectItemStatus` returns an archived card's last-known Status rather
  than reporting it absent. Production issues this as a direct
  `node(id:) { fieldValueByName }` query (`github/project.go:1299`), and an
  archived `ProjectV2Item` is still a node with a field value, so answering is
  the closer match.

**Unverified, in both directions.** Neither behaviour is derived from a recorded
real response. The batch side is the weaker claim of the two: production reads
it as `items(first:100)` on the project (`github/project.go:1339`) with no
archived filter, so real GitHub may well include archived items there — in which
case the sim's skip is the divergence, not the single-item read. **Risk:** low
for the single read (a scenario that has archived a card is not usually asking
for its column), higher for the batch if a scenario ever depends on an archived
card being *absent* from it. Capture a real response before relying on either.

### PR cards on a board — **Absent**

A project board can hold pull-request cards as well as issue cards, but the
model's board projections (`buildProjectItem`, `ProbeProjectBoard`) resolve a
card's content as an issue. A PR card would therefore read back as nothing —
silently omitted from every board fetch, with no error.

**Both** entry points refuse a PR card rather than accepting one and dropping it
later: `SeedProjectItem` rejects `isPR: true`, and `AddProjectV2ItemById` rejects
a `pr:` content node ID. The seeding and runtime APIs deliberately agree here —
a loud failure on one path and a silent no-op on the other would be worse than
either alone, because a scenario would conclude the card exists.

A scenario that needs the engine's PR-card handling (which `itemMayNeedWork`
skips) cannot be written on this layer.

**A card must point at an issue that exists.** The same reasoning applies to the
card's target, not just its kind: `buildProjectItem` resolves content through the
repo's issues and returns nil when it finds nothing, so a card seeded for a
mistyped or not-yet-seeded number was recorded and then omitted from every board
read, with no error. `SeedProjectItem` now requires the repo to be seeded and the
issue to exist, matching what `AddProjectV2ItemById` already enforced on the
runtime path and what `SeedBlockedBy`/`AddBlockedByIssue` enforce for dependency
targets. Seed the issue before the card.

### Dependency links — **Modelled, with existence enforced**

`AddBlockedByIssue` and `SeedBlockedBy` both require the blocker to resolve to a
seeded issue in a seeded repo, and refuse otherwise. This is stricter than a
fake strictly needs to be, and deliberately so: `resolveDependenciesLocked`
reports any blocker it cannot resolve as `State: "OPEN"`, so a dangling blocker
would leave the dependent issue reading as permanently blocked with no
diagnostic anywhere — a wrong ID accepted silently, which is the bug class this
layer exists to catch. Cross-repo blockers are supported; the blocker's live
state is resolved on every read, so closing it unblocks the dependent issue.

**Absent:** being blocked *by a PR*, and GitHub's permission rules for creating
a dependency across repos.

### Dependency cycles — **Self-block refused, longer cycles absent**

`AddBlockedByIssue` and `SeedBlockedBy` both refuse an issue blocked by itself,
as GitHub does: a self-block is unsatisfiable by construction, so the engine's
dependency gate would hold the issue forever with nothing to diagnose.

**Absent:** longer cycles (A blocks B blocks A) are *not* detected — that needs a
graph traversal the model does not perform, and GitHub does reject them.
**Risk:** low. A scenario that builds a cycle gets a permanently blocked pair
rather than a refusal, which is visible as a stuck scenario rather than a green
one; but do not use this layer to test cycle *rejection*.

### Check run IDs — **Modelled, unique across the sim**

GitHub's check run IDs are unique and monotonically increasing, and production
depends on the ordering: `latestCheckRunsByName` (`github/checkruns.go`) reduces
same-named runs to the one with the highest ID, treating it as the most recent
rerun, and keeps whichever it saw first on a tie. The sim therefore reserves an
explicitly seeded ID against later auto-assignment — the same rule
`reserveNumber` applies to the shared issue-and-PR sequence — and refuses a
duplicate outright. Without the reservation, seeding one run as `ID: 5000` (the
auto-assign counter's starting value) and the next with `ID: 0` produced two
runs sharing an ID, and a "check failed, then the rerun passed" scenario would
be classified off the *failed* run with nothing to indicate why.

**Simplified:** IDs are assigned from a single counter per `Sim` rather than
globally across a GitHub instance, and nothing ties them to creation time.

### `SeedPR{Merged: true}` performs the real merge — **Modelled**

**Decision:** model it, not merely flag it. A directly-seeded merged PR now
writes the same real merge commit onto the base branch `MergePR` would (via
the same `tryMerge` helper), and runs the same closing-keyword auto-close loop
— including its default-branch restriction (ADR-1096) — so a seeded "already
landed" PR is not a different world from one merged at runtime.

Seeding stays authoritative about *state*, not about git history it cannot
represent: `Merged: true` against branches that genuinely conflict, or where
the head is already fully contained in the base (nothing left to merge), is
refused via the sticky error rather than silently recorded — GitHub could
never have produced that PR as merged either. This mirrors the existing
"Seeded PRs and issues must be shapes GitHub can produce" precedent below.

**Risk:** low. `TestSeedPRMergedTruePerformsRealMerge` pins the merge commit
and the auto-close together; `TestSeedPRMergedTrueRefusesConflict` pins the
refusal. Fixed in #1498 — this was previously the one seeding shape whose
*consequences* went unmodelled (medium risk), which made a merge-train
scenario's "this member already landed" seed assert against unmerged git
history and open issues that `assembleTrialBranch` (pure real git) would then
faithfully build on top of, passing for the wrong reason.

### Seeded PRs and issues must be shapes GitHub can produce — **Modelled**

`SeedPR` refuses a merged draft, a PR that is merged *and* open (a merged PR is
always closed; `Merged` with the state unspecified resolves to closed), and a PR
whose head is its base — GitHub answers the last with "No commits between …",
and the shape is degenerate here too, collapsing the trial merge to the
nothing-to-merge path. `createPR` refuses head-equals-base on the runtime path
for the same reason.

Both also check the state string against GitHub's own enum (`OPEN`/`CLOSED` for
issues, `open`/`closed` for PRs) rather than merely for emptiness: every
downstream read compares it exactly, so `"closed"` on an issue would be stored
verbatim and then never match, silently leaving it classified open.

`SeedIssue` and `SeedPR` both refuse a negative explicit number — `reserveNumber`
is a no-op below `nextNumber`, so one would otherwise be accepted silently and
embedded in node IDs and every projection.

The point is not tidiness: a scenario built on a state production can never
deliver exercises engine logic against an input it will never see, and passes.
That is the "confidently green for the wrong reason" failure this whole document
exists to prevent, arriving through the seed API rather than through the model.

### Seeding is single-threaded, except `SeedRepo` — **Modelled**

The `Seed*` builder chain is designed to be driven by one goroutine during
setup, and most of it is safe only because `Sim.mu` guards each individual call.
`SeedRepo` is the exception that needed real work: it is the one seeding call
that runs git *before* publishing its state, so a check-then-publish under `mu`
alone let concurrent callers for one key both clear the check, race inside
`initBare` against a single directory with no `gitMu` yet in existence, and then
clobber each other's `repoState` — replacing a live `gitMu` while earlier
callers still held the old one, which silently voids the per-repo git
serialisation everything else depends on. It now takes its own creation lock for
the whole sequence, so concurrent callers for one key get a clean "already
seeded" refusal and never reach git.

**Not a licence to seed concurrently.** This makes `SeedRepo` safe; it does not
make the rest of the builder chain a concurrent API. Seed on one goroutine.

### `SeedBranch` creates, it does not repoint — **Modelled**

`SeedBranch` refuses a branch that already exists, matching GitHub's create-ref
endpoint (422 *"Reference already exists"*). The refusal matters more here than
the API parity: the `update-ref` underneath would happily move the branch, so
`SeedBranch` after a `SeedCommit` on the same branch discarded the seeded
commits and still reported success — a scenario author would only notice by
wondering where their commits went. Growing an existing branch is `SeedCommit`'s
job, and `commitFiles` already forks-or-appends deliberately.

**Absent:** there is no seeding equivalent of a force-push — no way to move an
existing branch to a different tip. Nothing in the engine's surface needs one;
add it as an explicit, differently-named call rather than by relaxing this
refusal.

### Repo directory sandboxing — **Modelled**

`owner` and `repo` are validated as single path-safe names, because `repoDir`
joins them straight into a directory under `baseDir`. Without that check,
`SeedRepo("../evil/pwned")` split into owner `../evil` and created the bare repo
as a *sibling* of the `t.TempDir()` this package promises to keep everything
inside — outside the sandbox and outside the test framework's cleanup. Real
GitHub owner and repo names cannot contain a separator either, so nothing valid
is rejected.

Owner and repo are also kept as **separate path segments** rather than joined
with a separator. Joining is not collision-free — `acme-widgets/foo` and
`acme/widgets-foo` flatten to the same name — and since the already-seeded check
is keyed on the full `owner/repo` string, both would be accepted and would
produce two `repoState` values, each with its own `gitMu`, over one physical
repository. That silently voids the per-repo git serialisation the whole locking
design rests on, and lets commits from one logical repo land in the other.

The same rule is enforced on the other door into the sandbox: the keys of a
`SeedCommit` files map, which `commitFiles` joins onto the throwaway worktree
path. A key like `../../outside.txt` wrote *above* the worktree, and did so
without a trace — git only tracks what is inside the worktree, so the write
never appeared in the commit and the seed reported success. Repository paths
legitimately contain separators (`docs/USER_GUIDE.md`), so this is a different
check from the owner/repo one: the *cleaned* path must stay at or below the repo
root, and absolute paths are refused rather than silently relocated by
`filepath.Join`'s cleaning. Validation runs before any git work, so a bad key
refuses the whole commit and leaves the branch tip untouched rather than writing
the map's other files first.

Throwaway worktrees are the third door, and are handled by placement rather
than validation: `withWorktree` creates them under `baseDir`, not the OS temp
dir. Its deferred cleanup removes them on every ordinary path, but a process
killed mid-merge (OOM, SIGKILL, a CI timeout) never runs deferred code, and an
orphan under the OS temp dir would outlive both this sandbox and
`t.TempDir()`'s cleanup. Keeping it inside `baseDir` makes the test framework
the backstop.

**Risk (all three): low.** Seed data is authored by test writers, not
attacker-controlled input, so this is a sandbox-hygiene guarantee — everything
this package creates stays inside the `baseDir` you gave it — rather than a
security boundary. Do not treat `simgh` as safe against hostile input.

### `ownerType` — **Simplified**

`FetchProjectBoard` accepts `ownerType` and echoes it back but does not use it.
Production uses it to choose between the `organization` and `user` GraphQL
roots, and falls back from one to the other when it is empty. The sim resolves
the project by owner regardless, so that fallback path is not exercised.

### Node IDs — **Simplified**

Node IDs are readable synthesised strings (`issue:owner/repo#42`,
`project:acme:2`) rather than opaque base64 blobs. Production never parses a node
ID it receives — it only round-trips them — so this is an implementation
convenience with no behavioural consequence. Do not write a test that depends on
the format.

### Label vocabulary — **Simplified**

`SeedLabels` records which labels exist in the repo, but `AddLabelToIssue` does
**not** enforce the vocabulary: applying a label that was never created
succeeds. Real GitHub creates the label implicitly in some paths and errors in
others. The engine's own `ensureLabel` behaviour is therefore not exercised.

### Removing an absent label errors — **Modelled**

`RemoveLabelFromIssue` returns a wrapped `gh.ErrNotFound` when the label was not
applied, because GitHub answers the DELETE with a 404 and production propagates
it. Roughly a dozen engine call sites branch on `errors.Is(err, gh.ErrNotFound)`
from this exact call; returning nil would make every one of them unreachable in
a sim-backed test. That the engine *tolerates* the error is not a reason to hide
it — tolerating it is the behaviour under test.

### Label applied-at — **Modelled**

`FetchLabelAppliedAt` returns the instant the label was applied, from the
injected clock. Re-applying an already-present label does **not** refresh it,
matching GitHub (which emits no new `labeled` event for a label already there).
This is load-bearing: the review-gate timeout and bot re-prompt ladder
(`engine/reviews.go`), the CI settle scan (`engine/ci_settle.go`), and the
Done-archive scan (`engine/archive_done_settle.go`) all anchor deadlines on it.

An absent label returns the zero time rather than an error, mirroring
production's "no such event found".

---

## Time

### Clock — **Modelled**

Every time-bearing value the model produces reads from the `Clock` injected at
construction: label applied-at, comment `CreatedAt`, `FetchProjectUpdatedAt`, and
`RateLimitStats`. `time.Now` is never called outside the default clock. A test
can therefore drive the engine's time-anchored gates without wall-clock waiting.

### One mutation, one clock read — **Modelled**

A mutation that stamps several fields reads the clock **once** and writes that
one value everywhere: a status move stamps the card and its project alike, a
label application stamps applied-at and the issue's updated-at alike, a comment
stamps its created-at and its parent alike, and a merge stamps the PR and every
issue it auto-closes alike. GitHub reports each of these as one event, and the
engine reads exactly these pairs against each other — `FetchProjectUpdatedAt`
gates whether a poll looks at the board at all, and `FetchLabelAppliedAt` is the
timing anchor for the review gate, the CI settle scan, and the Done-archive
scan.

This is easy to regress and hard to see: under a fixed clock every ordering
produces equal timestamps, so a per-field clock read looks correct in every test
that uses `fakeClock`. It is *not* correct under the default `realClock`, which
advances between reads. `TestOneMutationStampsOneTimestamp` therefore runs on a
deliberately advancing clock.

### Ordering and timestamps of concurrent writes — **Simplified**

Two writes made in the same clock instant are indistinguishable by timestamp.
The model preserves insertion order for comments and board items, but a scenario
that depends on distinguishable timestamps must advance the clock between
writes.

---

## Concurrency

### Model state — **Modelled**

All in-memory state is guarded by a single mutex; the package is `-race` clean
under concurrent access from multiple goroutines, which is what the engine's
worker dispatch produces.

### Git — **Modelled**

Git subprocess calls are serialised per repository. This is genuinely necessary,
not defensive: `MergePR` performs a read-modify-write on the base ref (check it
out, merge onto it, move the ref), so two interleaved merges onto one base both
fork from the same tip and the first is silently lost — with every call still
returning nil. `TestConcurrentMergesOntoOneBase` pins this.

Read-only trial merges are independently safe, since each runs in its own
throwaway worktree and moves no ref.

**Simplified:** serialisation is per repository, so cross-repo operations run
concurrently. That matches the isolation real repositories have.

---

## Verifying this document

Two mechanisms keep it from drifting into fiction:

1. **The interface assertion.** `var _ engine.GitHubClient = (*Sim)(nil)` breaks
   the build the moment `engine.GitHubClient` grows a method. A new method must
   get a real implementation and, if it diverges, an entry here.

2. **The non-vacuity sweep.** `bash tests/sim/simgh/nonvacuity.sh` neutralises
   each modelled behaviour in turn and asserts the suite goes red. A behaviour
   claimed as **Modelled** above that survives its mutation is a claim this
   package cannot back up. The sweep currently catches all 146 mutations, and
   fails on any mutation that never applied — an unrun mutation proves nothing.

   The instrumentation layer's own mutations are in there too, and one of them
   is the case this kind of code fails at most characteristically: **an
   injector that silently succeeds**. A fault-injection test that passes
   because nothing failed asserts something about a call that was never made.

Neither mechanism can tell you whether a **Modelled** entry matches *real
GitHub* — only that the model does what this document says. For anything subtle
and load-bearing, prefer deriving the behaviour from a recorded real response
over reasoning about it.
