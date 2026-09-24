# ADR 1841: Merge-Train Conflict Turn-Limit Handling

**Date**: 2026-09-24
**Status**: Accepted
**Issue**: #1841 — a small merge-train conflict exhausts the resolution turn budget and ejects the member as "cannot resolve", discarding real progress and reporting nothing useful

## Context

Reported in #1826 (a community report, kept open until a release ships): a merge-train
member was ejected as "cannot resolve conflict" over what turned out, on hand
inspection, to be a single conflicting import line. Preserved production logs
(`verveguy/concept-maps`, 2026-09-20) show two episodes:

| member | content-conflicted files | outcome |
|---|---|---|
| #516 | 1 (`RelationshipEdge.tsx`) | 51 turns, 22m35s, `error_max_turns` → ejected |
| #780 | 3 | 51 turns, 4m53s, $4.45 → ejected |

Both hit the *same* 51-turn cap regardless of conflict size — a strong structural
signal that the sink is something turn-count-driven and roughly constant per
invocation, not proportional to the conflict's actual size. The original #516
transcript no longer exists (deleted with its trial, per ADR-1834's cleanup, and
unreproducible after the fact since it conflicted against an accumulated trial state
that itself no longer exists — see #1834/#1835). Two structural facts were available
without a live trace: `resolveConflictWithClaude` treated *any* non-usage-limit error —
including a turn-cap exit — as unconditionally unresolvable, discarding whatever
worktree state resulted; and `buildTrainConflictComment`, the synthetic comment sent to
Claude, unconditionally instructed "Run the project's build + test commands (`go build
./...` and `go vet ./...` at minimum)" — a Go-specific command hardcoded into a prompt
meant for any target repo, including `concept-maps`, which is TypeScript/React and has
no `go.mod` at all.

Requirement 1 of the issue explicitly forbids fixing this from structural inference
alone ("must not assume"). The rest of this ADR is organized around: (1) the live
reproduction that confirms the turn-sink hypothesis, (2) the prompt/budget change that
follows from it, (3) the post-exit resolution check that stops discarding completed
work, (4) the ejection-message honesty fix, and (5) the incidental `NoResume` fix.

## 1. Turn-sink diagnosis (live reproduction, not assumption)

A scratch git repository was built to mirror `concept-maps`' actual shape: a
TypeScript/React project (`package.json` with `build`/`test`/`typecheck` scripts, no
`go.mod`), with two branches independently editing the same JSX line in a single file
(`RelationshipEdge.tsx`-equivalent) — a guaranteed single-file content conflict, sized
like the #516 episode. The real `claude` CLI (not a mock) was invoked twice against
independent copies of the identical conflict, using the same CLI flags
`buildClaudeArgs`/`runClaude` construct in production (`--permission-mode dontAsk`,
`--output-format`, the default allowed-tools set including `Bash(git:*)`/`Bash(go:*)`/
`Bash(npm:*)`), capped at `--max-turns 25` to bound the diagnostic's own cost:

- **Before** (today's prompt, including the `go build ./... && go vet ./...` step):
  **10 turns**. The transcript shows the extra turns going exactly where the
  structural hypothesis predicted: `ls` plus a `package.json`/`go.mod` existence check,
  `git log --all --oneline --graph -20` (unrelated history), then — after resolving and
  committing — `npm run build && npm run test && npm run typecheck` substituted for the
  inapplicable Go commands. Claude's own final response flagged the mismatch
  explicitly: *"the instructions said to verify with `go build ./...` / `go vet
  ./...`, but this repo has no `go.mod` — it's a JS/TS project... I ran the actual
  project scripts instead."*
- **After** (the scoped prompt below, no verification instruction): **5 turns** — read
  the conflicted file, resolve, stage, commit, done. No exploratory detour.

This is a 2x reduction on a *trivial* toy conflict where the substituted npm scripts
were `echo` stubs costing nothing to run. On the real `concept-maps` repository, the
same substitution would mean an actual `npm install` (if `node_modules` isn't already
present in the trial worktree — it generally isn't, since each trial is a fresh
`EnsureTrainWorktreeAt` checkout) followed by a real build and test suite, each
individually capable of costing several tool-call turns on its own, on top of the
exploration turns already observed. This is consistent with — and sufficient to
explain — the measured 51-turn/22-minute production episode, without needing to
reproduce that exact episode byte-for-byte (which is structurally impossible; see
Context above). The turn sink is the unconditional, Go-specific verification
instruction, not a hidden defect in Claude's actual conflict-resolution reasoning.

## 2. Decision: remove the verification instruction, don't touch the turn cap

`buildTrainConflictComment`'s step 5 ("Run the project's build + test commands...") is
replaced, in both the plain and mixed (generated-path) prompt variants, with: *"Do not
run the project's build, test, lint, or install commands, and do not explore files
outside the conflicted path(s) — the trial's own CI validates the result after this
step completes. Resolve only the conflict markers in the listed file(s) and stop."*

This is safe because removing the instruction does not remove the safety net it
existed for: `assembleAndValidateInner` unconditionally opens a draft CI PR and polls
full CI (`pollTrainCI`) after every trial assembly, regardless of how a member's
conflict was resolved. A syntactically broken resolution is still caught by real CI
before landing — the same trust boundary an ordinary Claude-resolved conflict already
relies on today. The change removes a redundant, expensive, and — critically — often
*inapplicable* verification step, not the only verification step.

**The configured turn budget (`commentMaxTurns(holdingStg)`, default 50) is left
unchanged.** Both measured production episodes hit the identical 51-turn cap
regardless of conflict size (1 file vs. 3 files) — exactly what an open-ended,
exploration-inducing instruction produces, not what an intrinsically larger conflict
needs. Lowering the numeric cap in addition to removing the instruction would risk
prematurely turn-capping a legitimately complex multi-file conflict, for no
demonstrated benefit; the budget is also process-wide (ADR-1648 — shared by every
concurrently running (repo, base) partition), so a cut affects every future
conflict-resolution invocation across the whole engine, not just small ones.

## 3. Decision: a turn-limit exit is inspected, not assumed unresolvable

Before this change, `resolveConflictWithClaude` branched only on `err == nil` vs.
`err != nil`: any non-nil error other than the ADR-1120 usage-limit sentinel returned
`(false, nil)` immediately, skipping entirely the worktree-inspection logic that
already existed for the success path (remaining-conflict-marker check, the
`MERGE_HEAD`/`preMergeHEAD` abort-vs-resolution disambiguation, `git diff --check`, and
the commit). This is the literal root cause of the issue: the machinery to detect
"actually resolved, just capped" already existed — it was only reachable from the
wrong branch.

That inspection logic is extracted into a new helper, `finalizeConflictResolution`, and
both the clean-exit path and a new turn-limited-exit path call it identically.
`errors.As(err, &turnLimitErr)` detects a `claudeTurnLimitError` (CLI subtype
`error_max_turns`) exactly as ADR-1178 established elsewhere in the engine. The
rationale mirrors ADR-1120's account-wide usage-limit principle, applied at the
per-invocation level instead: exhausting a turn budget means the dispatch *reached and
ran* Claude — the same "Claude is healthy" signal a clean success carries — so it says
nothing on its own about whether the conflict itself is resolvable. The account-wide
Claude-suspension-clearing call (`clearClaudeSuspension`) fires on this path too, for
the same reason it already fires on a clean success.

If `finalizeConflictResolution` finds no remaining non-generated conflict markers (and,
for the plain case, a clean `git diff --check`), the resolution is committed and the
member **stays in the batch** — exactly as a clean success would, and exactly what
Requirement 2 and Acceptance 2 require. Only when conflicts genuinely remain does the
turn-limited exit fall through to ejection.

**Interaction with the mixed (generated-path) case:** the mixed case's early return in
`finalizeConflictResolution` (return once non-generated markers are clear, deferring
`git diff --check` and the commit to `regenerateAndCommit`) is untouched by this
change — a turn-limited-but-resolved mixed conflict flows through identically to a
successful one, reaching `regenerateAndCommit` exactly as before.

**Interaction with `git rerere` (ADR-1834) and generated-file regeneration
(ADR-1235):** neither is touched. `resolveTrainConflict`'s three-way dispatch (rerere
already fully replayed → direct commit; confined to declared generated paths →
regenerate; otherwise → Claude) runs entirely *before* `resolveConflictWithClaude` is
ever called for the plain and mixed cases — this issue's changes are inside that third
branch only, and never change the dispatch decision itself. A conflict rerere fully
replays still short-circuits with no Claude invocation at all, exactly as before.

**Hardening found in review: a "resolved" index doesn't prove marker-free content.**
`unmergedPaths` (`git status --porcelain`) clears a path's `UU` status as soon as it is
`git add`ed — regardless of whether the staged content still contains literal
conflict-marker text. By the point `finalizeConflictResolution` runs its plain-case
`git diff --check` (no `--cached`), every originally conflicted path has therefore
already been staged (that's what cleared its `UU` status), so the working tree and the
index are already identical and the check compares two copies that can never differ —
it is a structural no-op precisely when it matters most, and more likely to matter now
that a turn-limited exit (Claude cut off mid-resolution) also reaches this code path.
`regenerateAndCommit` already worked around the staged half of this with `git diff
--cached --check` (see its own doc comment); this issue's plain case additionally needed
to cover the *already-committed* case, since the plain-case prompt's own step 4
instructs Claude to commit itself, which is the most common path here. The fix is a
direct on-disk content scan, `pathsStillContainConflictMarkers`, checked against
`originalNonGeneratedPaths` right after the abort-disambiguation block (so it covers
both the plain and mixed cases with one check, ahead of the mixed case's early return) —
immune to whether the content has since been staged or committed, since it reads the
file itself rather than a git diff. A regression test
(`TestMergeTrainWorker_ConflictStagedMarkersNotCommitted`) reproduces the exact gap
(Claude stages and commits a file that still contains marker text) and was confirmed
non-vacuous the same way as the rest of this issue's tests: disabling the new check
makes it fail.

## 4. Decision: thread a conflict-specific diagnostic, don't reuse `trainCIDiagnostic`

When a conflict genuinely remains unresolved — whether from a clean-but-incomplete
exit or a turn-limited one — the caller needs to know *why* and *what* to build an
honest ejection comment (Requirements 3/4). A new `conflictEjectionDiagnostic` struct
(`TurnLimited bool`, `NumTurns int`, `RemainingPaths []string`, `Reason string`) is
threaded as a plain return value — never shared or mutable state, following ADR-1420's
precedent exactly — from `resolveConflictWithClaude` through `resolveTrainConflict` to
`assembleTrialBranch`'s ejection call site.

This is deliberately **not** layered onto the existing `trainCIDiagnostic` type
(`FailedChecks`/`FailedContexts`/`Note`), which ADR-1420 built specifically for CI
check-run/context failures and explicitly scoped away from the conflict-resolution
ejection path. Reusing it as-is would be a type mismatch (a conflicted-file list is not
a failed-check list), and `ejectMember`'s own `diag *trainCIDiagnostic` parameter and
its purpose-built rendering path (`renderDiagnosticBlock`, truncation policy) have no
need for this smaller, differently-shaped value. Instead, `buildConflictEjectionReason`
renders the new diagnostic directly into `ejectMember`'s existing plain-string `reason`
parameter — `ejectMember`'s signature, `renderDiagnosticBlock`, and every CI-ejection
call site are completely untouched by this issue.

The rendered reason distinguishes, per Requirement 4:
- **Turn-limited**: *"the invocation ran out of turns (num_turns=N) rather than judging
  the conflict unresolvable"* — never claims a judgment was made.
- **Judged unresolvable** (the pre-existing, non-turn-limited path): *"conflict judged
  unresolvable"* — this wording is new; before this issue the message was always a
  generic "unresolvable conflict (PR SHA ...)" with no file names, for *both* cases.
- Either way, when files are known: *"conflict markers remain in: `file1`, `file2`"*.

**File-level detail only, not hunk-level.** Requirement 3's prose mentions "files and
hunks," but Acceptance 3 only requires naming files. `unmergedPaths` (`git status
--porcelain`) is file-level; hunk-level detail would require parsing `<<<<<<<` marker
regions inside each conflicted file's content — new machinery with no existing
precedent in this codebase. This is an explicit, documented non-goal: files-only
satisfies the acceptance bar without speculative scope growth. A future issue can add
hunk-level detail if it proves necessary in practice.

A `nil` diagnostic (the `unmergedPaths`-error fallback path inside `resolveTrainConflict`,
which by construction has no conflicted-path list to report — the same git-level
failure that triggered the fallback) still produces the original pre-#1841 generic
message, via `buildConflictEjectionReason`'s nil case.

## 5. Decision: fix the spurious "resume requested but none exists" warning with `InvokeOptions.NoResume`, not an interface change

`InvokeClaudeForComments` hardcoded `resume := true` unconditionally — correct for its
other call sites (real PR/issue comment review, which always follows a genuine prior
stage dispatch that created a session), but wrong for merge-train conflict resolution:
the holding stage ("Queued") is never dispatched as a real stage (`holding_stage:
true`, "items are never dispatched individually" per `CLAUDE.md`), so there is no
genuine prior "Queued" session to resume. On a member's first conflict-resolution
encounter, the session file structurally cannot exist yet, so `resolveResumeSessionID`
logs "resume requested but none exists" on every single occurrence — not an anomaly,
but a guaranteed structural artifact of asking to resume a session that was never
going to exist.

Beyond the log noise, resuming is also conceptually wrong even on a *later* attempt for
the same member: each conflict-resolution invocation runs in a fresh, ephemeral trial
worktree (`EnsureTrainWorktreeAt`) against whatever the *current* accumulated conflict
happens to be — a session recorded from an earlier trial cycle or bisection sub-trial
may concern an entirely different conflict. Resuming it has no demonstrated benefit and
a plausible cost: irrelevant prior context consuming part of the turn budget (one of
the issue's own named candidate turn sinks, though the live reproduction in §1 above
identified the verification instruction as the dominant one).

The fix is a new, additive `InvokeOptions.NoResume bool` field, set only at the
merge-train call site (`assembleTrialBranch`'s `InvokeOptions{...}` construction).
`InvokeClaudeForComments` passes `!opts.NoResume` to `resolveResumeSessionID` instead of
a hardcoded `true`. This was chosen over adding a `resume bool` parameter to the
`ClaudeInvoker.InvokeForComments` interface method (mirroring `Invoke`, which already
takes one) because `InvokeOptions` is the established, already-extensible pattern for
exactly this kind of per-call-site override — see `MaxResumeFailures`'s identical
shape — and an interface change would touch every mock implementing `ClaudeInvoker` in
the test suite for a behavior only one call site needs.

## Consequences

- A small merge-train conflict resolves in a small number of turns (§1: 5 vs. 10 turns
  on the reproduction; expected to be a substantially larger reduction on a real
  project repo, where the removed instruction would otherwise trigger a real
  install/build/test cycle rather than `echo` stubs).
- A turn-limited exit whose worktree is actually resolved no longer discards completed
  work or ejects the member — it commits and the member stays in the batch.
- Every conflict-resolution ejection comment — turn-limited or not — now names the
  still-conflicted file(s) and states plainly whether the invocation ran out of budget
  or judged the conflict unresolvable, closing a gap ADR-1420 explicitly left open for
  this ejection path.
- The "resume requested but none exists" warning no longer fires for merge-train
  conflict resolution.
- `resolveConflictWithClaude`'s and `resolveTrainConflict`'s signatures both widened
  from `(bool, error)`/`(bool, string, error)` to `(bool, *conflictEjectionDiagnostic,
  error)`. The compiler caught every call site needing an update (three inside
  `resolveTrainConflict`, one in `assembleTrialBranch`, two in existing unit tests) —
  mechanical volume, not a silent-gap risk.
- **Explicit non-goals**, deliberately out of scope: no numeric turn-budget reduction
  (§2); no hunk-level ejection detail (§4); no persistence of conflict resolutions
  across trials (ADR-1834/#1834, #1835's territory — this issue only changes what
  happens within a single resolution attempt); no change to landing strategy or to
  resolving on a member's own branch (the issue's own Scope section, proposal 1 in
  #1826, not adopted); no change to `ejectMember`'s `MaxMergeTrainEjections` counter or
  the generated-file exclusion (ADR-1235), both verified unchanged by existing tests
  continuing to pass unmodified.
