#!/usr/bin/env bash
# Non-vacuity sweep for tests/sim/simgh.
#
# A stateful fake is exactly the kind of code whose tests can pass for the
# wrong reason, so every behaviour this package claims to model is neutralised
# here in turn and the suite re-run. A mutation that leaves the suite green is
# a test that proves nothing.
#
# Usage:  bash tests/sim/simgh/nonvacuity.sh
# Requires a clean working tree under tests/sim/simgh (it edits sources in
# place and restores them with git checkout after each case).

set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

PKG=./tests/sim/simgh
DIR=tests/sim/simgh
FAILED_TO_CATCH=()
# A skipped mutation proves nothing: its edit never applied, so the test it
# targets was never challenged. Track skips separately and fail on them too,
# or a typo in a perl expression silently erodes coverage forever while the
# sweep still reports success.
SKIPPED=()

# The sweep restores sources with `git checkout`, which would silently discard
# uncommitted work in this directory. Refuse to run rather than eat it.
if ! git diff --quiet -- "$DIR" || ! git diff --cached --quiet -- "$DIR"; then
  echo "error: $DIR has uncommitted changes; commit or stash them first." >&2
  echo "       This script restores sources with 'git checkout' and would discard them." >&2
  exit 2
fi

restore() { git checkout -- "$DIR" 2>/dev/null; }
trap restore EXIT

# mutate <name> <test-regex> <perl-expression...>
# Applies the perl edits, runs the named tests, and expects them to FAIL.
mutate() { RACE="" _mutate "$@"; }

# mutate_race is mutate with the race detector on, for mutations whose only
# symptom is a data race rather than a wrong answer.
mutate_race() { RACE="-race" _mutate "$@"; }

_mutate() {
  local name="$1"; shift
  local tests="$1"; shift
  restore

  for expr in "$@"; do
    local file="${expr%%::*}"
    local script="${expr#*::}"
    if ! perl -0777 -pi -e "$script" "$DIR/$file"; then
      echo "SKIP  $name (perl edit failed on $file)"
      SKIPPED+=("$name (perl edit failed on $file)")
      return
    fi
  done

  local out
  if ! out=$(go build "$PKG" 2>&1); then
    echo "SKIP  $name (mutation does not compile: $(echo "$out" | head -1))"
    SKIPPED+=("$name (does not compile)")
    return
  fi

  if go test -count=1 -timeout 5m $RACE -run "$tests" "$PKG" >/dev/null 2>&1; then
    echo "GREEN $name  <-- NOT CAUGHT"
    FAILED_TO_CATCH+=("$name")
  else
    echo "CAUGHT $name"
  fi
}

echo "=== simgh non-vacuity sweep ==="

mutate "label add is a no-op" \
  'TestLabelAddRemoveIsObservable' \
  'labels.go::s{(iss\.labels = append\(iss\.labels, labelName\))}{// $1}'

mutate "label applied-at is refreshed on re-add" \
  'TestLabelAddRemoveIsObservable' \
  'labels.go::s{if contains\(iss\.labels, labelName\) \{\n\t\treturn nil\n\t\}}{if contains(iss.labels, labelName) { iss.labelAppliedAt[labelName] = s.now(); return nil }}'

mutate "label remove is a no-op" \
  'TestLabelAddRemoveIsObservable' \
  'labels.go::s{iss\.labels = updated}{_ = updated}'

mutate "comment reaction is dropped" \
  'TestCommentAddAndReactionAreObservable' \
  'comments.go::s{(c\.reactions\[content\] = 1)}{// $1}'

mutate "comment edit is dropped" \
  'TestCommentAddAndReactionAreObservable' \
  'comments.go::s{^\tc\.body = body$}{\t_ = c}m'

mutate "issue body update is dropped" \
  'TestIssueBodyUpdateIsObservable' \
  'issues.go::s{^\tiss\.body = body$}{\t// neutralised}m'

mutate "project status update is dropped" \
  'TestProjectStatusUpdateIsObservable' \
  'board.go::s{^\t\tit\.status = name$}{\t\t_ = name}m'

mutate "status option ID is not validated" \
  'TestProjectStatusUpdateIsObservable' \
  'board.go::s{return fmt\.Errorf\("simgh: no status option %q in project %q", statusOptionID, projectID\)}{return nil}'

mutate "MarkPRReady is a no-op" \
  'TestPRCreateReadyMergeIsObservable' \
  'prs.go::s{^\tpr\.draft = false$}{\t// neutralised}m'

mutate "MergePR does not record the merge" \
  'TestPRCreateReadyMergeIsObservable|TestMergePRSucceedsOnUnstable' \
  'prs.go::s{^\tpr\.merged = true$}{\t// neutralised}m'

mutate "merged PR does not auto-close its issue" \
  'TestPRCreateReadyMergeIsObservable' \
  'prs.go::s{if base == r\.defaultBranch \{}{if false \{}'

mutate "CloseIssue is a no-op" \
  'TestCloseIssueIsObservable' \
  'issues.go::s{^\tiss\.state = "CLOSED"$}{\t// neutralised}m'

mutate "blockedBy state is frozen at seed time" \
  'TestBlockedByResolvesBlockerStateLive' \
  'board.go::s{resolved\.State = iss\.state}{_ = iss; resolved.State = "OPEN"}'

mutate "ResolveReviewThread is a no-op" \
  'TestReviewThreadResolutionIsObservable' \
  'comments.go::s{^\t\t\t\tc\.threadResolved = true$}{\t\t\t\t// neutralised}m'

mutate "reviewDecision ignores missing branch protection" \
  'TestReviewDecisionEmptyWithoutBranchProtection' \
  'reviews.go::s{required, ok := r\.requiredApprovals\[pr\.base\]}{required, ok := r.requiredApprovals[pr.base]; ok = true; _ = ok}'

mutate "trial merge never detects a conflict" \
  'TestFetchPRMergeableFalseOnRealConflict|TestMergeableStateDirtyOnRealConflict|TestConcurrentTrialMergesOnOneRepo' \
  'git.go::s{^\t\t\t\tconflict = true$}{\t\t\t\tconflict = false}m'

mutate "mergeability is declared rather than computed" \
  'TestFetchPRMergeableFalseOnRealConflict|TestMergeableStateDirtyOnRealConflict' \
  'prs.go::s{mergeable := !facts\.conflict}{mergeable := true}'

mutate "MergePR does not write a merge commit" \
  'TestMergePRProducesRealMergeCommit' \
  'git.go::s{if _, err := runGit\(r\.bareDir, "update-ref", "refs/heads/"\+base, sha\); err != nil \{}{if false \{ _ = sha}'

mutate "merge is fast-forwarded instead of a merge commit" \
  'TestMergePRProducesRealMergeCommit|TestMergePRCreatesMergeCommitEvenWhenFastForwardable' \
  'git.go::s{"merge", "--no-ff", "-m", msg, headSHA}{"merge", "--ff", "-m", msg, headSHA}'

mutate "commits-behind always reports zero" \
  'TestFetchCommitsBehindCountsRealCommits|TestMergePRProducesRealMergeCommit|TestMergeableStateBehindRequiresBranchProtection' \
  'git.go::s{^\treturn n, nil$}{\t_ = n; return 0, nil}m'

mutate "head SHA is not re-resolved after a push" \
  'TestHeadSHATracksNewCommits' \
  'git.go::s{^\tsha, err := r\.resolveRef\("refs/heads/" \+ branch\)$}{\tsha, err := r.resolveRef("refs/heads/" + r.defaultBranch)}m'

mutate "required contexts never block (blocked no longer outranks unstable)" \
  'TestMergeableStateBlockedOnFailingRequiredContext|TestMergeableStateBlockedOnMissingRequiredContext|TestMergeableStateBlockedOnPendingRequiredContext|TestMergeableStateBlockedViaClassicCommitStatus|TestMergePRGatesOnDerivedState|TestPrecedenceBlockedOverUnstable' \
  'prs.go::s{^\t\t\treturn stateBlocked$}{\t\t\t_ = name}m'

mutate "non-required failures never surface as unstable" \
  'TestMergeableStateUnstableOnFailingNonRequiredContext|TestMergePRSucceedsOnUnstable|TestPrecedenceUnstableOverClean' \
  'prs.go::s{^\t\t\treturn stateUnstable$}{\t\t\t_ = name}m'

mutate "commit statuses are ignored by the derivation" \
  'TestMergeableStateBlockedViaClassicCommitStatus|TestMergeableStateCleanViaClassicCommitStatus|TestMergeableStateUnstableViaNonRequiredCommitStatus' \
  'ci.go::s{statuses\[st\.Context\] = statusVerdict\(st\)}{_ = st}'

# The reduction rule is the whole point of reserving check-run IDs, so each half
# is mutated separately: a check run reduced by worst-of, and a commit status
# accumulated rather than superseded.
mutate "same-named check runs reduce by worst verdict instead of latest ID" \
  'TestMergeableStateRerunSupersedesFailedRun|TestMergeableStateRerunOrderFollowsIDNotSeedOrder' \
  'ci.go::s{\t\tif prev, ok := latest\[run\.Name\]; ok && run\.ID <= prev\.ID \{\n\t\t\tcontinue\n\t\t\}}{\t\tif prev, ok := latest[run.Name]; ok \&\& checkVerdict(prev) != "success" \{\n\t\t\tcontinue\n\t\t\}}m'

mutate "an earlier commit status is not superseded by a later one" \
  'TestMergeableStateLatestCommitStatusSupersedes' \
  'ci.go::s{\tfor _, st := range r\.commitStatuses\[sha\] \{\n\t\tstatuses\[st\.Context\] = statusVerdict\(st\)\n\t\}}{\tfor _, st := range r.commitStatuses[sha] \{\n\t\tstatuses[st.Context] = worseVerdict(statuses[st.Context], statusVerdict(st))\n\t\}}m'

mutate "a name in both collections does not keep the worse verdict" \
  'TestMergeableStateCrossCollectionKeepsWorstVerdict' \
  'ci.go::s{\t\t\tout\[name\] = worseVerdict\(prev, v\)}{\t\t\tout[name] = prev}'

mutate "FetchPRDetails reports no mergeable_state" \
  'TestFetchPRDetailsReportsDerivedMergeableState' \
  'prs.go::s{\t\tMergeableState:   state,}{\t\tMergeableState:   state[:0],}'

mutate "behind is reported without branch protection" \
  'TestMergeableStateBehindRequiresBranchProtection' \
  'prs.go::s{if facts\.behind > 0 && r\.requireUpToDate\[base\] \{}{if facts.behind > 0 \{}'

mutate "draft precedence is dropped" \
  'TestMergeableStateDraftTakesPrecedence' \
  'prs.go::s{^\tif draft \{\n\t\treturn stateDraft\n\t\}$}{}m'

mutate "recompute window never opens" \
  'TestMergeableRecomputeWindow|TestMergePRRefusesDuringRecomputeWindow' \
  'prs.go::s{if pr\.mergeableRecomputeReads > 0 \{}{if false \{}'

mutate "MergePR skips its mergeable_state gate" \
  'TestMergePRGatesOnDerivedState' \
  'prs.go::s{if !gh\.MergeableStateAccepted\(state\) \{}{if false \{}'

mutate "MergePR skips its mergeable gate" \
  'TestMergePRRefusesRealConflict|TestMergePRRefusesDuringRecomputeWindow' \
  'prs.go::s{if mergeable == nil \|\| !\*mergeable \{}{_ = mergeable; if false \{}'

mutate "merged PR still reports a mergeable state" \
  'TestMergedPRReportsUnknown' \
  'prs.go::s{if pr\.merged \|\| pr\.state == "closed" \{}{if false \{}'

mutate "FetchLinkedPR leaks a computed mergeable_state" \
  'TestPRCreateReadyMergeIsObservable' \
  'prs.go::s{MergeableState:      "", // omitted by the list endpoint — see doc comment}{MergeableState:      "clean",}'

mutate "linked PR resolution picks the oldest on a reused branch" \
  'TestFetchLinkedPRPrefersNewestOnReusedBranch' \
  'board.go::s{for i := len\(nums\) - 1; i >= 0; i-- \{}{for i := 0; i < len(nums); i++ \{}'

mutate "issue and PR numbers come from independent sequences" \
  'TestIssueAndPRShareNumberSpace' \
  'model.go::s{^\tnextNumber int$}{\tnextNumber int\n\tnextPRNumber int}m' \
  'seed.go::s{^\t\tnextNumber:        1,$}{\t\tnextNumber:        1,\n\t\tnextPRNumber:      1,}m' \
  'prs.go::s{^\tnum := r\.allocNumber\(\)$}{\tnum := r.nextPRNumber\n\tr.nextPRNumber++}m'

mutate "an explicitly seeded number is not reserved against auto-assignment" \
  'TestAutoAssignedNumberSkipsSeededHoles' \
  'model.go::s{^\tif num >= r\.nextNumber \{\n\t\tr\.nextNumber = num \+ 1\n\t\}$}{\t_ = num}m'

mutate "seeding does not enforce the shared number space" \
  'TestSeedRejectsNumberHeldByOtherKind' \
  'seed.go::s{\} else if r\.numberTaken\(num\) \{}{\} else if false \{}g'

mutate "a PR card is accepted and silently dropped" \
  'TestSeedProjectItemRejectsPRCard' \
  'seed.go::s{\tif isPR \{}{\tif false \{}'

mutate "the runtime board API accepts a PR card the seeding API refuses" \
  'TestAddProjectV2ItemByIdRejectsPRCard' \
  'board.go::s{return "", fmt\.Errorf\("simgh: PR cards on a project board are not modelled \(%s#%d\); see FIDELITY\.md", ownerRepo, number\)}{ }'

mutate "a nonexistent blocker is accepted at runtime" \
  'TestAddBlockedByIssueRejectsUnknownBlocker' \
  'deps.go::s{if _, ok := br\.issues\[blockerNum\]; !ok \{}{if _, ok := br.issues[blockerNum]; !ok && false \{}'

mutate "a nonexistent blocker is accepted at seed time" \
  'TestSeedBlockedByRejectsUnknownBlocker' \
  'seed.go::s{if _, ok := br\.issues\[blockerNumber\]; !ok \{}{if _, ok := br.issues[blockerNumber]; !ok && false \{}'

# --- precedence ordering ------------------------------------------------
# The single-condition tests above prove each branch is reachable; only these
# constrain the order they are tried in. Each mutation demotes one state below
# the next, which a correctly-ordered suite must notice.

mutate "draft no longer outranks dirty" \
  'TestPrecedenceDraftOverDirty' \
  'prs.go::s{if draft \{\n\t\treturn stateDraft\n\t\}\n\tif facts\.conflict \{\n\t\treturn stateDirty\n\t\}}{if facts.conflict \{\n\t\treturn stateDirty\n\t\}\n\tif draft \{\n\t\treturn stateDraft\n\t\}}'

mutate "dirty no longer outranks behind" \
  'TestPrecedenceDirtyOverBehind' \
  'prs.go::s{if facts\.conflict \{\n\t\treturn stateDirty\n\t\}}{if false \{\n\t\treturn stateDirty\n\t\}}'

mutate "behind no longer outranks blocked" \
  'TestPrecedenceBehindOverBlocked' \
  'prs.go::s{if facts\.behind > 0 && r\.requireUpToDate\[base\] \{}{if false \{}'

mutate "an explicitly seeded check run ID is not reserved against auto-assignment" \
  'TestSeedCheckRunReservesExplicitIDs' \
  'seed.go::s|\t\} else if run\.ID >= s\.nextCheckRunID \{|\t\} else if false \{|'

mutate "a duplicate check run ID is accepted" \
  'TestSeedCheckRunRejectsDuplicateID' \
  'seed.go::s|\t\} else if s\.seededCheckRunIDs\[run\.ID\] \{|\t\} else if false \&\& s.seededCheckRunIDs[run.ID] \{|'

# Neutralised by short-circuiting the whole validator rather than editing its
# two conditions in place: the guard's own source contains backticks and
# backslashes that are awkward to match reliably, and a perl expression that
# silently fails to apply proves nothing (which is why SKIP now fails the sweep).
mutate "owner/repo path traversal is accepted" \
  'TestSeedRepoRejectsPathTraversal' \
  'util.go::s|func validatePathSegment\(kind, seg, ownerRepo string\) error \{|func validatePathSegment(kind, seg, ownerRepo string) error \{\n\treturn nil|'

# Short-circuited for the same reason as validatePathSegment above: the guard's
# own source contains backticks and backslashes that are awkward to match.
mutate "a seeded file path may escape the worktree" \
  'TestSeedCommitRejectsEscapingFilePath' \
  'util.go::s|func validateRepoRelPath\(name string\) error \{|func validateRepoRelPath(name string) error \{\n\treturn nil|'

# Both of SeedRepo's duplicate guards are neutralised together: single-threaded,
# either one alone still refuses the reseed, so a mutation of just one could not
# fail this test and would prove nothing.
mutate "a duplicate SeedRepo is accepted" \
  'TestSeedRepoRefusesDuplicateSeed' \
  'seed.go::s|if _, exists := s\.repos\[ownerRepo\]; exists \{|if false \{|g'

mutate_race "SeedRepo's creation lock is removed (concurrent callers race in git and clobber)" \
  'TestConcurrentSeedRepoDoesNotClobber' \
  'sim.go::s|\tseedRepoMu sync\.Mutex|\tseedRepoMu noopSeedMutex|' \
  'sim.go::s|^type realClock struct\{\}$|type noopSeedMutex struct{}\n\nfunc (noopSeedMutex) Lock()   {}\nfunc (noopSeedMutex) Unlock() {}\n\ntype realClock struct{}|m'

# The two halves of the re-add drift are neutralised separately, because each
# path had been missing a different one and a single mutation could not show
# that both are now covered.
mutate "re-adding a card leaves it archived" \
  'TestReAddingArchivedCardRevivesItOnBothPaths' \
  'board.go::s|\texisting\.archived = false|\t_ = existing.archived|'

mutate "re-adding a card does not advance the project updated-at" \
  'TestReAddingArchivedCardRevivesItOnBothPaths' \
  'board.go::s|\texisting\.updatedAt = now\n\tp\.updatedAt = now|\t_ = now|'

mutate "timestamps come from wall time, not the injected clock" \
  'TestLabelAddRemoveIsObservable' \
  'sim.go::s{func \(s \*Sim\) now\(\) time\.Time \{ return s\.clock\.Now\(\) \}}{func (s *Sim) now() time.Time { return time.Now() }}'

mutate_race "model mutex is removed (expects a data race)" \
  'TestConcurrentModelAccess|TestConcurrentReviewMutations' \
  'sim.go::s{^\tmu sync\.Mutex$}{\tmu noopMutex}m' \
  'sim.go::s{^type realClock struct\{\}$}{type noopMutex struct{}\n\nfunc (noopMutex) Lock()   {}\nfunc (noopMutex) Unlock() {}\n\nvar _ sync.Mutex\n\ntype realClock struct{}}m'

mutate "per-repo git serialisation is removed" \
  'TestConcurrentMergesOntoOneBase' \
  'model.go::s|^\tgitMu sync\.Mutex$|\tgitMu noopGitMutex|m' \
  'model.go::s|\z|\ntype noopGitMutex struct{}\n\nfunc (noopGitMutex) Lock() {}\nfunc (noopGitMutex) Unlock() {}\n\nvar _ sync.Mutex\n|'


echo
status=0
if [ ${#FAILED_TO_CATCH[@]} -ne 0 ]; then
  echo "Mutations NOT caught by the suite:"
  printf '  - %s\n' "${FAILED_TO_CATCH[@]}"
  status=1
fi
if [ ${#SKIPPED[@]} -ne 0 ]; then
  echo "Mutations that never ran (they prove nothing — fix the expression):"
  printf '  - %s\n' "${SKIPPED[@]}"
  status=1
fi
if [ "$status" -eq 0 ]; then
  echo "All mutations were caught. No vacuous tests found."
fi
exit "$status"
