# ADR 1420: Merge-Train Ejection Diagnostics Survive Bisection as a Threaded Return Value

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1420 — merge-train ejection comments discard the combined Validate output — the only run in which the failure exists

## Context

When the merge-train ejects a member, it posts a comment stating *that* the combined
Validate failed. Before this change, the comment never said *what* failed. Reported
in #1418 (`totalslacker/concept-graph`): issue #145 was ejected six times across two
operator attempts — three of them with the issue alone in the train — producing six
byte-identical, uninformative comments over roughly eleven hours. The actual cause was
a one-line fix, and the failing check had printed the exact remedy in its output.

A merge-train failure is structurally different from an ordinary CI failure: it is, by
construction, a failure that **does not exist on the member's own branch**. It arises
only from combining the branch with a base that moved after the branch was cut — the
member's own PR checks are green, correctly, because the offending interaction is not
in the PR. The combined-Validate trial that observed the failure is therefore the
*only* place its output ever exists. Discarding that output leaves the operator with
no route back to a working state except reproducing the combination by hand.

Tracing the code confirmed the loss is total and happens across several hops. Every
layer between the check-run fetch and the ejection comment narrows its signal to
something smaller: `pollTrainCI` reads `[]gh.CheckRun` (name, status, and — before this
change — nothing else) and reduces it to a 3-value `TrainCIResult` enum
(`TrainCIPending`/`TrainCIGreen`/`TrainCIRed`); `assembleAndValidate` and the bisection
functions pass only that enum upward; `ejectMember` receives a hand-written `reason
string` with no structured content at all. `gh.CheckRun` itself carried no output
text, summary, or URL to begin with — GitHub's check-runs API returns
`output.title`/`output.summary`/`output.text` and `details_url`/`html_url`, none of
which Fabrik parsed.

### Which run's output actually matters

`bisect` recursively halves a red member set — validate half A; if red, recurse into
A; else validate half B; if red, recurse into B — until a red singleton (the poisoner)
is isolated. Tracing this by hand: the recursion's base case (`len(red) == 1`) returns
*without* issuing any further validation call. For any starting batch size, the last
`assembleAndValidate` call that actually executes is therefore always the one that
validated the poisoner **alone** against the pinned base SHA — never combined with any
surviving sibling.

This matters because bisection does not stop there: after isolating the poisoner and
ejecting it, the outer loop re-forms and re-validates the *survivor* batch to check it
is now clean. That later validation is unrelated to the ejection just posted. A naive
implementation that stashes "the most recent diagnostic" in shared or mutable state
(e.g. a field on `mergeTrainWorkerState`) would have that later, unrelated run silently
overwrite the diagnostic that actually explains the ejection just made — reintroducing
a subtler version of the exact defect this issue reports, one that would pass a
shallow-looking test asserting only that *an* ejection comment mentions *some* check
name.

### Batch-membership semantics

Because the isolating run is always a singleton (see above), "name the other batch
members" (issue requirement R4) cannot mean "the members combined in the run that
failed" — that run had none. It is read as *contextual* naming instead: which other
members were riding in this train attempt when the aggregate batch first went red,
informing the operator that the fault does not exist on their own branch before they
go looking for it there. This also matches the reporter's own wording in #1418
("fails in combination with commits merged since this branch was cut" — base-vs-branch,
not member-vs-member).

`landOneAtATime` (the one-at-a-time fallback used when bisection's cost budget is
exhausted, or when two halves are each green alone but red together — a genuine
interaction bisection cannot isolate) is the one path where this reasoning doesn't
apply: it re-pins the base to `origin/<base>` before each singleton, so a later
singleton in that loop can legitimately be red because of an **earlier-landed sibling**
in the same fallback run — a true member-vs-member interaction. Its ejection therefore
carries no batch-context list (there is no batch — the member was validated as a true
singleton), distinctly worded from the bisection case.

## Decision

### 1. `trainCIDiagnostic` is threaded as a plain return value, never shared or mutable state

`trainCIDiagnostic` (`engine/merge_train.go`) carries exactly one of: `FailedChecks
[]gh.CheckRun` (ordinary check-run failures, with output text/summary and a
details/run URL), `FailedContexts []string` (classic commit-status "required context"
failures — ADR-933 — which carry no check-run output to extract at all), or `Note
string` (a dirty `mergeable_state` with no per-check signal whatsoever). `PRNum` and
`TrialSHA` are always populated for context.

It is produced once, at the point of failure, by `pollTrainCI`, and threaded as a
return value through every hop: `assembleAndValidate` → `bisect` /
`handleRedBatch` / `landOneAtATime` → `ejectMember`. `bisect`'s base case
(`len(red) == 1`) returns the diagnostic it was *given* by its caller unchanged — it
issues no validation call of its own, so by construction it cannot manufacture or
overwrite one. Each recursive call passes forward only the diagnostic from the half it
just validated red. There is no engine field, worker-state field, or other piece of
shared state a later, unrelated validation (e.g. bisection's post-ejection survivor
re-check) could clobber — the overwrite failure mode described above is structurally
impossible, not merely avoided by convention.

`handleRedBatch` also threads its own top-level `red` parameter (the full red batch at
the start of the bisection episode) through to `ejectMember` as `otherMembers`, purely
for the R4 contextual-naming sentence — this is separate from, and does not affect,
the diagnostic-overwrite invariant above.

### 2. `gh.CheckRun` gains output/URL fields, fetched unconditionally

`OutputSummary`, `OutputText`, `DetailsURL`, and `HTMLURL` are added to `gh.CheckRun`
(`github/prs.go`) and parsed unconditionally in `FetchCheckRuns`, rather than behind a
separate, merge-train-only fetch path. `FetchCheckRuns` is shared with
`checkCIGate`/`pr_settle.go`; a second fetch path would duplicate the REST call and its
error handling for no benefit, and every construction site in the repo uses
named-field struct literals, so the addition is purely additive — no existing caller
breaks.

### 3. `ejectMember` renders the diagnostic into every ejection comment for that failure, not only the terminal pause comment

Before this change, the ejection comment gave a reason with no content, and the
pause-after-`MaxMergeTrainEjections` comment (default 3) was equally uninformative.
Both are now diagnostic-bearing: `ejectMember` renders (a) the R4 batch-context
sentence (naming other members, or explicitly stating none were present for a
single-member train), and (b) the R1/R3 diagnostic block, into **every** ejection
comment — the first exactly as informative as the last. The pause comment does not
repeat the full block; it names the failing check(s)/context(s) and links the
permalink of the ejection comment just posted (`AddComment`'s previously-discarded
comment ID is now captured for this purpose), falling back to the pre-existing generic
"resolve the underlying conflict" wording when no diagnostic is available.

The three ejection call sites outside this issue's scope — unfetchable PR/head-SHA
(`fetchTrainMembers`), and unresolvable merge conflicts (`assembleTrialBranch`) — pass
a nil diagnostic and nil batch-context list. Their ejection comments are unaffected, as
the issue's Scope section specifies.

### 4. Bounded inline output, degrading to a pointer, never "no diagnostic"

Per failing check: inline the full output up to 3000 chars; beyond that, inline 2000
chars from the head and 800 from the tail with an explicit "N chars omitted" marker,
plus a `Details:` link whenever GitHub provided one — always, not only when truncated,
since it's strictly more helpful. At most 5 failing checks get their output inlined;
any beyond that are named only. A final 15000-char hard cap truncates the whole
assembled block, mirroring the existing tail-only truncation idiom in
`formatOutputComment`/`formatReviewFeedbackComment` (`engine/pr.go`) as a
belt-and-suspenders against GitHub's ~65536-char comment limit. A `RequiredContextsFailed`
diagnostic (no check-run output at all) renders as a names-only line rather than an
empty-looking excerpt — a pointer is the minimum acceptable outcome, never nothing.

### 5. The test seam is widened in lockstep with the real path

`trainValidateFn` (`engine/engine.go`) widened from `func(ctx, []trainMember)
TrainCIResult` to also return `*trainCIDiagnostic`, and `recordingValidator`
(`engine/merge_train_test.go`) grew a synthetic default diagnostic for its red case
(real check name and output text, not empty strings) plus an optional `diagFor`
override for tests that assert specific content. This was necessary, not incidental:
acceptance criteria 1–3 require asserting on ejection-comment *body text* for a
bisection *sequence* — specifically, that the *first* ejection comment carries the
diagnostic, not only a later one — and essentially all existing bisection-sequencing
coverage goes through this seam rather than real git and a real CI poll. Without
widening the seam, those criteria could only be exercised by a much slower, real-git
equivalent of the existing sequencing tests.

## Rationale

**Threaded return value over shared state**: the overwrite failure mode this decision
rules out — a later, unrelated validation clobbering the diagnostic that explains an
already-decided ejection — is not hypothetical. It is the literal shape `bisect`'s
control flow invites (isolate, eject, then validate something else entirely). A
threaded return value makes it a compile-time impossibility rather than a runtime
correctness property to hold in your head at every future edit site.

**Contextual, not literal, batch naming**: reading R4 literally (name the isolating
run's own inputs) would produce an empty list on every bisection ejection, since that
run is always a singleton by construction. The contextual reading is the only one that
produces a coherent message for the single-member-train case, which the reporter's own
data shows is common (three of six ejections), and it is also the more operator-useful
reading — it establishes *before* the operator starts investigating that the fault is
not in their own branch.

**Unconditional field fetch over a targeted second call**: the response-size cost is a
few KB per check run on every CI poll fabrik-wide, not just merge-train polls. This was
judged an acceptable, small, constant cost against a second REST call path with its own
error handling to maintain, especially since `FetchCheckRuns` is already shared
infrastructure.

## Consequences

- Every ejection comment produced by a combined-Validate failure now names the failing
  check(s) and includes a bounded excerpt of their output, or degrades to a names-only
  line / a run-URL pointer when no richer signal exists. This directly closes the gap
  #1418 reported.
- `gh.CheckRun` is a slightly larger struct and `FetchCheckRuns`'s response parsing does
  slightly more work, repo-wide — accepted as noted above.
- `trainValidateFn`'s signature change touches exactly two assignment sites
  (`engine/merge_train_test.go`), both updated in this change; the blast radius was
  small and enumerable, not sprawling.
- `pollTrainCI`, `assembleAndValidate`, `bisect`, `handleRedBatch`, `landOneAtATime`,
  and `ejectMember` all grew a parameter and/or return value. The compiler caught every
  call site that needed updating; there was no risk of a silent gap, only mechanical
  volume.
- Out of scope, unchanged: the bisection algorithm itself, the pause-after-N policy,
  and ejection causes other than a failing combined Validate (unresolvable merge
  conflicts, missing PR head SHA, fetch failures).
