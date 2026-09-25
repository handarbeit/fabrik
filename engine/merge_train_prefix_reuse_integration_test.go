package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// closesNumbersFromBody extracts every issue number named in a "Closes #N" line of a
// merge-train trial's draft-PR body (see assembleAndValidateInner), in order. Test-only:
// it lets a mock fetchPRMergeableFieldsFn/fetchPRDetailsFn decide red/green by trial
// membership even though the trial is a real git assembly, not the trainValidateFn seam.
func closesNumbersFromBody(body string) []int {
	var nums []int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Closes #"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
				nums = append(nums, n)
			}
		}
	}
	return nums
}

// prefixLookupCall records one assembleTrialBranch prefix lookup, in call order
// (trainPrefixLookupHookFn fires exactly once per assembleTrialBranch invocation, at
// its very start, before any git merge or Claude conflict resolution for that trial).
// claudeCallsAtStart/mergeAttemptsAtStart are the two independent counters' values at
// that instant, letting a caller compute how much of each happened strictly during a
// given call (see claudeDeltaForCall/mergeDeltaForCall).
type prefixLookupCall struct {
	numbers              []int
	matchedLen           int
	totalLen             int
	claudeCallsAtStart   int32
	mergeAttemptsAtStart int32
}

func numbersEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func findPrefixLookupCall(t *testing.T, calls []prefixLookupCall, numbers []int) prefixLookupCall {
	t.Helper()
	for _, c := range calls {
		if numbersEqual(c.numbers, numbers) {
			return c
		}
	}
	t.Fatalf("expected a prefix lookup call for members %v, found none among %d call(s): %+v", numbers, len(calls), calls)
	return prefixLookupCall{}
}

// runPrefixReuseScenario reproduces the #1835 evidence log at minimal scale: a 4-member
// batch where every merge past the first conflicts against the trial-so-far (real
// Claude conflict resolution, real git), the full batch goes red because #3 is a
// poisoner, bisection isolates and ejects #3, and the survivors are re-formed. Red/green
// is decided by which member issue numbers appear in the trial's own draft-PR body
// ("Closes #N" lines) — the real assembly path (assembleTrialBranch), not the
// trainValidateFn membership-keyed test seam, which bypasses assembleTrialBranch
// entirely and so cannot exercise prefix reuse at all.
//
// Returns every prefix-lookup call observed (in order), the final Claude
// (resolveConflictWithClaude) invocation count, and the final real-`git merge` attempt
// count, for the caller to assert against. The two counters are deliberately kept
// separate: git rerere's autoupdate replay (ADR-1834) can resolve a repeated identical
// conflict without ever invoking Claude, so "zero Claude invocations" alone cannot
// distinguish #1835's prefix reuse (the merge never runs at all) from rerere doing its
// own, unrelated, already-existing job (the merge runs, but its conflict resolves for
// free) — mergeAttempts is the literal, rerere-immune "zero merges" signal Acceptance 1
// asks for.
func runPrefixReuseScenario(t *testing.T, disablePrefixReuse bool) ([]prefixLookupCall, int32, int32) {
	t.Helper()
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "counter.txt", "from-1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "counter.txt", "from-2\n")
	sha3 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-3", "counter.txt", "from-3\n")
	sha4 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-4", "counter.txt", "from-4\n")
	memberSHA := map[int]string{1: sha1, 2: sha2, 3: sha3, 4: sha4}

	var mu sync.Mutex
	prMembers := map[int][]int{}
	nextPR := int32(100)

	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			sha, ok := memberSHA[issueNumber]
			if !ok {
				return nil, fmt.Errorf("not found")
			}
			return &gh.PRDetails{Number: issueNumber + 10, HeadSHA: sha, State: "open"}, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			nextPR++
			prNum := int(nextPR)
			prMembers[prNum] = closesNumbersFromBody(body)
			return prNum, nil
		},
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			mu.Lock()
			members := prMembers[prNumber]
			mu.Unlock()
			// Red iff #3 (the poisoner) is present.
			for _, n := range members {
				if n == 3 {
					f := false
					return &f, "dirty", nil
				}
			}
			tr := true
			return &tr, "clean", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}

	var claudeCalls int32
	claude := &mockClaudeInvoker{
		invokeForCommentsFn: func(stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			atomic.AddInt32(&claudeCalls, 1)
			if err := os.WriteFile(filepath.Join(workDir, "counter.txt"), []byte(fmt.Sprintf("resolved-%d\n", issue.Number)), 0644); err != nil {
				return "", false, TokenUsage{}, err
			}
			addCmd := exec.Command("git", "add", "-A")
			addCmd.Dir = workDir
			if out, err := addCmd.CombinedOutput(); err != nil {
				return string(out), false, TokenUsage{}, nil
			}
			commitCmd := exec.Command("git", "commit", "--no-edit", "-m", fmt.Sprintf("resolve #%d", issue.Number))
			commitCmd.Dir = workDir
			if out, err := commitCmd.CombinedOutput(); err != nil {
				return string(out), false, TokenUsage{}, nil
			}
			return "resolved", true, TokenUsage{}, nil
		},
	}

	eng := trainTestEngine(t, client, claude, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()
	eng.mergeTrainPrefixReuseDisabledForTest = disablePrefixReuse

	var mergeAttempts int32
	eng.trainMergeAttemptHookFn = func(memberNumber int) {
		atomic.AddInt32(&mergeAttempts, 1)
	}

	var lookupsMu sync.Mutex
	var lookups []prefixLookupCall
	eng.trainPrefixLookupHookFn = func(matchedLen, totalLen int, numbers []int) {
		lookupsMu.Lock()
		defer lookupsMu.Unlock()
		lookups = append(lookups, prefixLookupCall{
			numbers:              append([]int(nil), numbers...),
			matchedLen:           matchedLen,
			totalLen:             totalLen,
			claudeCallsAtStart:   atomic.LoadInt32(&claudeCalls),
			mergeAttemptsAtStart: atomic.LoadInt32(&mergeAttempts),
		})
	}

	batch := []gh.ProjectItem{makeTrainItem(1, "1"), makeTrainItem(2, "2"), makeTrainItem(3, "3"), makeTrainItem(4, "4")}
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	eng.store.EnterRepoWorker(mergeTrainKey("owner/repo", "main"))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch)

	return lookups, atomic.LoadInt32(&claudeCalls), atomic.LoadInt32(&mergeAttempts)
}

// claudeDeltaForCall returns how many Claude invocations happened strictly during the
// named call — the difference between the next recorded call's starting count and this
// call's own starting count (calls are made from a single goroutine, in strict sequence,
// and nothing between two assembleTrialBranch calls invokes Claude — see
// forgetPoisonerResolutions's doc comment). finalCount is used when call is the last one
// recorded.
func claudeDeltaForCall(calls []prefixLookupCall, call prefixLookupCall, finalCount int32) int32 {
	return deltaForCall(calls, call, finalCount, func(c prefixLookupCall) int32 { return c.claudeCallsAtStart })
}

// mergeDeltaForCall is claudeDeltaForCall's counterpart for real `git merge` attempts —
// see runPrefixReuseScenario's doc comment for why this, not the Claude counter, is the
// signal that directly proves Acceptance 1's literal "zero merges".
func mergeDeltaForCall(calls []prefixLookupCall, call prefixLookupCall, finalCount int32) int32 {
	return deltaForCall(calls, call, finalCount, func(c prefixLookupCall) int32 { return c.mergeAttemptsAtStart })
}

func deltaForCall(calls []prefixLookupCall, call prefixLookupCall, finalCount int32, at func(prefixLookupCall) int32) int32 {
	for i, c := range calls {
		if numbersEqual(c.numbers, call.numbers) {
			if i+1 < len(calls) {
				return at(calls[i+1]) - at(c)
			}
			return finalCount - at(c)
		}
	}
	return -1
}

// TestMergeTrainWorker_PrefixReuse_BisectFirstHalfIsFree is Acceptance 1: bisect's
// first half — by construction an exact prefix of the red trial's own chain — is
// produced with zero resolveConflictWithClaude invocations and zero merges.
func TestMergeTrainWorker_PrefixReuse_BisectFirstHalfIsFree(t *testing.T) {
	lookups, finalClaude, finalMerges := runPrefixReuseScenario(t, false)

	initial := findPrefixLookupCall(t, lookups, []int{1, 2, 3, 4})
	if initial.matchedLen != 0 {
		t.Errorf("expected the very first trial of a fresh worker invocation to start with matchedLen 0, got %d", initial.matchedLen)
	}

	firstHalf := findPrefixLookupCall(t, lookups, []int{1, 2})
	if firstHalf.matchedLen != firstHalf.totalLen {
		t.Errorf("expected bisect's first half {1,2} to be a full prefix match (matchedLen == totalLen == 2), got matchedLen=%d totalLen=%d", firstHalf.matchedLen, firstHalf.totalLen)
	}
	if delta := mergeDeltaForCall(lookups, firstHalf, finalMerges); delta != 0 {
		t.Errorf("expected bisect's first half {1,2} to perform zero `git merge` attempts (full prefix reuse), got %d", delta)
	}
	if delta := claudeDeltaForCall(lookups, firstHalf, finalClaude); delta != 0 {
		t.Errorf("expected bisect's first half {1,2} to invoke Claude zero times, got %d", delta)
	}
}

// TestMergeTrainWorker_PrefixReuse_EjectReformReusesPrefix is Acceptance 2: after
// ejecting the isolated poisoner (#3, at chain position 3 of {1,2,3,4}), the re-formed
// trial for the survivors {1,2,4} reuses the prefix through #2 and performs a merge
// only for #4 — the sole member after the eject point.
func TestMergeTrainWorker_PrefixReuse_EjectReformReusesPrefix(t *testing.T) {
	lookups, finalClaude, finalMerges := runPrefixReuseScenario(t, false)

	reform := findPrefixLookupCall(t, lookups, []int{1, 2, 4})
	if reform.matchedLen != 2 {
		t.Errorf("expected the re-formed trial {1,2,4} to reuse a prefix of length 2 (members 1,2 — everything before the ejected #3), got matchedLen=%d", reform.matchedLen)
	}
	if delta := mergeDeltaForCall(lookups, reform, finalMerges); delta != 1 {
		t.Errorf("expected exactly 1 `git merge` attempt during the re-formed trial (only #4, the sole member after the eject point), got %d", delta)
	}
	if delta := claudeDeltaForCall(lookups, reform, finalClaude); delta != 1 {
		t.Errorf("expected exactly 1 Claude conflict-resolution invocation during the re-formed trial (only #4 actually conflicts), got %d", delta)
	}
}

// TestMergeTrainWorker_PrefixReuse_DisabledForTest_IsNonVacuous is Acceptance 6: with
// reuse disabled (mergeTrainPrefixReuseDisabledForTest), the same scenario shows the
// previously-suppressed merges return — proving
// TestMergeTrainWorker_PrefixReuse_BisectFirstHalfIsFree and
// TestMergeTrainWorker_PrefixReuse_EjectReformReusesPrefix are not vacuously true of a
// scenario that never needed any merge work to begin with. Deliberately asserted via the
// merge-attempt counter, not the Claude-invocation counter: git rerere's autoupdate
// (ADR-1834, unrelated to and unaffected by this test's reuse flag) can replay member
// #2's identical conflict resolution for free even with prefix reuse disabled, so a
// Claude-invocation-count assertion here would be flaky-by-design against that
// legitimate, independent optimization — the merge itself still has to run either way,
// which is what mergeAttempts proves.
func TestMergeTrainWorker_PrefixReuse_DisabledForTest_IsNonVacuous(t *testing.T) {
	lookups, _, finalMerges := runPrefixReuseScenario(t, true)

	for _, c := range lookups {
		if c.matchedLen != 0 {
			t.Errorf("expected zero prefix reuse for members %v with reuse disabled, got matchedLen=%d", c.numbers, c.matchedLen)
		}
	}

	firstHalf := findPrefixLookupCall(t, lookups, []int{1, 2})
	if delta := mergeDeltaForCall(lookups, firstHalf, finalMerges); delta != 2 {
		t.Errorf("expected bisect's first half {1,2} to re-pay both `git merge` attempts (members 1 and 2) with reuse disabled, got %d — this would make TestMergeTrainWorker_PrefixReuse_BisectFirstHalfIsFree's zero-merges assertion vacuous", delta)
	}

	reform := findPrefixLookupCall(t, lookups, []int{1, 2, 4})
	if delta := mergeDeltaForCall(lookups, reform, finalMerges); delta != 3 {
		t.Errorf("expected the re-formed trial {1,2,4} to re-pay `git merge` attempts for all 3 survivors with reuse disabled, got %d — this would make TestMergeTrainWorker_PrefixReuse_EjectReformReusesPrefix's assertion vacuous", delta)
	}
}

// TestMergeTrainWorker_PrefixReuse_LandOneAtATimeRepinnedBase_ForksFromRepinnedBase is
// Requirement 4 through a real worker path (the #1835 review finding): landOneAtATime
// re-pins its local copy of trialParams.baseSHA to the current origin/<base> before each
// singleton, while sharing the worker's one *trainPrefixCache. A member that was chain
// position 1 on the ORIGINAL base must not hit that stale chain — each singleton trial
// must fork from the re-pinned base (so a prior singleton's land is visible to its
// validation), not from a commit built on the old one.
func TestMergeTrainWorker_PrefixReuse_LandOneAtATimeRepinnedBase_ForksFromRepinnedBase(t *testing.T) {
	skipIfNoGit(t)
	_, srcDir, _, wm := setupTrainRepo(t)

	sha1 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-1", "file1.txt", "content1\n")
	sha2 := pushBranchToBare(t, srcDir, wm.baseDir, "fabrik/issue-2", "file2.txt", "content2\n")
	memberSHA := map[int]string{1: sha1, 2: sha2}
	m1 := trainMember{item: makeTrainItem(1, "1"), prNum: 11, headSHA: sha1}
	m2 := trainMember{item: makeTrainItem(2, "2"), prNum: 12, headSHA: sha2}

	baseA := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "main"))
	mustGitDir(t, wm.baseDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")

	// The trial branch is deleted as soon as its (red) trial is disposed, so the
	// fork-point check runs inside createDraftPRFn, while the branch still exists.
	var mu sync.Mutex
	var repinnedBase string // set once main advances
	var trialHeads []string
	var forkErrs []string
	nextPR := int32(100)
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, issueNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: issueNumber + 10, HeadSHA: memberSHA[issueNumber], State: "open"}, nil
		},
		createDraftPRFn: func(owner, repo, title, head, base, body string, issueNumber int) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			trialHeads = append(trialHeads, head)
			if repinnedBase != "" {
				cmd := exec.Command("git", "merge-base", "--is-ancestor", repinnedBase, "refs/heads/"+head)
				cmd.Dir = wm.baseDir
				if out, err := cmd.CombinedOutput(); err != nil {
					forkErrs = append(forkErrs, fmt.Sprintf("%s does not contain re-pinned base %s: %s: %v", head, repinnedBase, out, err))
				}
			}
			nextPR++
			return int(nextPR), nil
		},
		// Red keeps each singleton on the cheap dispose path — this test is about where
		// the trial forked from, not about landing.
		fetchPRMergeableFieldsFn: func(owner, repo string, prNumber int) (*bool, string, error) {
			f := false
			return &f, "dirty", nil
		},
		fetchPRDetailsFn: func(owner, repo string, prNumber int) (*gh.PRDetails, error) {
			return &gh.PRDetails{Number: prNumber, MergeableState: "clean"}, nil
		},
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()

	var lookups []prefixLookupCall
	eng.trainPrefixLookupHookFn = func(matchedLen, totalLen int, numbers []int) {
		lookups = append(lookups, prefixLookupCall{numbers: append([]int(nil), numbers...), matchedLen: matchedLen, totalLen: totalLen})
	}

	trainKey := mergeTrainKey("owner/repo", "main")
	p := trialParams{
		owner: "owner", repo: "repo", baseBranch: "main", trainKey: trainKey, baseSHA: baseA, wm: wm,
		nextTrialName: trialNameGen("repin-test"),
		prefixCache:   newTrainPrefixCache(trainKey, wm.baseDir, false),
	}
	defer p.prefixCache.cleanup()

	// A prior trial on the original base records a chain whose position 1 is m1 —
	// exactly the shape an original red trial (or bisect's second half) leaves behind.
	if _, _, err := e2eAssemble(eng, p, []trainMember{m1, m2}, "repin-seed"); err != nil {
		t.Fatalf("seed assembly on the original base: %v", err)
	}
	eng.cleanupTrialArtifacts(p.repoKey(), wm, "repin-seed")

	// main advances: this is the "prior singleton just landed" / "main moved" state.
	writeFile(t, srcDir+"/advance.txt", "main moved\n")
	mustGit(t, srcDir, "add", "-A")
	mustGit(t, srcDir, "commit", "-m", "advance main")
	mustGit(t, srcDir, "push", wm.baseDir, "main:main")
	baseB := strings.TrimSpace(gitOutputDir(t, srcDir, "rev-parse", "main"))
	if baseA == baseB {
		t.Fatal("setup failed: main did not advance")
	}
	lookups = nil
	mu.Lock()
	repinnedBase = baseB
	trialHeads = nil
	mu.Unlock()

	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(trainKey, state)
	eng.store.EnterRepoWorker(trainKey)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	eng.landOneAtATime(ctx, state, p, []trainMember{m1, m2})

	mu.Lock()
	heads := append([]string(nil), trialHeads...)
	mu.Unlock()
	if len(heads) != 2 {
		t.Fatalf("expected 2 singleton trials (one per member), got %d: %v", len(heads), heads)
	}
	mu.Lock()
	errs := append([]string(nil), forkErrs...)
	mu.Unlock()
	for _, msg := range errs {
		t.Errorf("singleton trial forked from a stale prefix: %s", msg)
	}
	for _, c := range lookups {
		if c.matchedLen != 0 {
			t.Errorf("expected zero prefix reuse after the base was re-pinned, got matchedLen=%d for members %v", c.matchedLen, c.numbers)
		}
	}
}

// e2eAssemble runs assembleTrialBranch for a test that needs a recorded chain without a
// full worker.
func e2eAssemble(eng *Engine, p trialParams, members []trainMember, trialName string) ([]trainMember, string, error) {
	return eng.assembleTrialBranch(context.Background(), p, members, trialName)
}
