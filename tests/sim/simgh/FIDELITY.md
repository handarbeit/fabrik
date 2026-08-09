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

**This mirrors production's own exposure, and deliberately stops short of
fixing it.** `github.Client.MergePR` (`github/prs.go:1114`) reads `mergeable`,
then `FetchPRMergeableFields`, then `PUT .../merge` with only a `merge_method`
— GitHub's merge endpoint accepts an optional `sha` for exactly this
compare-and-swap and production does not send one. So the engine's gate verdict
is equally stale against real GitHub; what saves it there is GitHub's own
server-side re-check, which the sim has no equivalent of. Making the sim stricter
would model a guarantee production does not actually have.

**Risk:** low, and bounded by construction — the window only exists for a
scenario that mutates a repo concurrently with its own `MergePR` call. A
scenario that wants to exercise "a required check went red mid-merge" cannot do
it here; that needs the server-side re-check the sim does not model.

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

### Rate limiting — **Absent**

`RateLimitStats` reports static budgets that never deplete. No method ever fails
with a rate-limit or secondary-rate-limit error, and there is no retry-after
behaviour. The engine's backoff and rate-limit-exhaustion paths
(`engine/backoff.go`, `engine/terminal.go`) **cannot** be exercised through this
layer.

Timestamps (`Reset`, `UpdatedAt`) still come from the injected clock, so a test
controlling time sees coherent values.

Fault injection generally — including rate-limit error kinds — is deferred to
the follow-on instrumentation layer (#1457).

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
   package cannot back up. The sweep currently catches all 71 mutations, and
   fails on any mutation that never applied — an unrun mutation proves nothing.

Neither mechanism can tell you whether a **Modelled** entry matches *real
GitHub* — only that the model does what this document says. For anything subtle
and load-bearing, prefer deriving the behaviour from a recorded real response
over reasoning about it.
