package sim

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// batchCapMembers is the live scenario's own member count
// (tests/e2e/mergetrain_batchcap_test.go): 7 Queued against the cap of 5 that
// mergeTrainEnv already sets, so the train lands 5 then 2.
const batchCapMembers = 7

// batchCapPlacement is the order the member cards are put on the board, as
// indexes into the members slice (index 0 = lowest issue number). It is
// deliberately scrambled: simgh lists the board in insertion order, and
// StatusEnteredAt is never stamped on the pass-through adapter, so the only
// thing separating "the first five by the train's ordering" from "the first
// five the board happens to list" is the (StatusEnteredAt, Number) sort in
// groupQueuedByRepoAndBase. Board order's first five are m7,m3,m6,m1,m5 (by
// index 6,2,5,0,4) — a set that differs from the five lowest numbers — so the
// ordering assertion can fail.
var batchCapPlacement = []int{6, 2, 5, 0, 4, 1, 3}

// TestMergeTrainQueuedDeeperThanBatchCap is the sim twin of the live scenario
// of the same name (tests/e2e/mergetrain_batchcap_test.go, #1850 / ADR-1833):
// with more members Queued than max_batch_size allows, the train takes a
// deterministic first batch — the first N by (StatusEnteredAt, issue number) —
// lands it, then lands the remainder in a second batch, with no member skipped,
// duplicated, ejected or left in Queued.
//
// All seven members are seeded before the first poll, so they are all visible
// as Queued in the same PollOnce (no straddle race — the live scenario's
// poll-boundary race, #1978, does not exist here). The scenario then runs
// exactly two RunPolls with assertions between: batch 2 can only form on the
// poll after batch 1's worker has landed and released its in-flight marker.
//
// The board lists the members in a scrambled order (batchCapPlacement), so the
// first five by board order differ from the first five by issue number; only
// the engine's sort yields the lowest five. See the SortDisabled variant, which
// shows that same fixture does produce the board-order five once the sort is
// off.
//
// Mapping to the live assertions (the sim has no log-scraping primitive, so
// state is read directly):
//   - A1 (first batch = the first N, first trial has N survivors): the first
//     merged landing PR's Closes set, and the first trial's recorded member set.
//   - A2 (batch membership stable, no abandoned trial): exactly two trial PRs
//     ever opened, none closed unmerged, none left open.
//   - A3 (5 then 2, all land): two merged landing PRs, the second being the
//     remainder; exactly two MergePR calls, batch 1 landing before poll 2 starts.
//   - exactly-once: Closes sets are disjoint and cover all members; every member
//     is Done, closed, with exactly one "Landed via" comment (posted on its member PR).
//
// The "batch capped … 7 Queued item(s) exceed max_batch_size=5" log line has no
// sim analogue; it is covered by proxy — a first trial of exactly five and a
// second of exactly two can only come from the cap.
//
// Known limit (ADR-1833): the sim uses the pass-through GitHubAdapter and simgh
// iterates a slice, so the pre-#1833 board-cache map-iteration churn cannot be
// reproduced here. This twin guards the engine's ordering and capping logic;
// the churn stays covered by engine/merge_train_queue_churn_test.go.
func TestMergeTrainQueuedDeeperThanBatchCap(t *testing.T) {
	t.Parallel()
	runBatchCapScenario(t, false)
}

// TestMergeTrainQueuedDeeperThanBatchCap_SortDisabledPicksBoardOrder is the
// ordering twin's own non-vacuity proof, kept permanently: the identical
// scrambled-board fixture with groupQueuedByRepoAndBase's sort disabled
// (SetMergeTrainQueueSortDisabledForTest, ADR-1833) must land the board-order
// first five instead of the lowest five. If the fixture ever degenerated to
// board order == number order, this test would fail, flagging that the
// ordering assertion in the main twin had become vacuous.
func TestMergeTrainQueuedDeeperThanBatchCap_SortDisabledPicksBoardOrder(t *testing.T) {
	t.Parallel()
	runBatchCapScenario(t, true)
}

func runBatchCapScenario(t *testing.T, sortDisabled bool) {
	t.Helper()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{})
	if sortDisabled {
		env.Engine.SetMergeTrainQueueSortDisabledForTest(true)
	}

	// Seed every member off-board, then place them in the scrambled order.
	members := make([]int, batchCapMembers)
	memberPRs := map[int]int{}
	for i := range members {
		n, pr := QueueMemberUnplaced(t, env, fmt.Sprintf("batchcap-m%d", i+1),
			map[string]string{fmt.Sprintf("batchcap-m%d.txt", i+1): fmt.Sprintf("member %d\n", i+1)})
		members[i] = n
		memberPRs[n] = pr
	}
	var boardOrder []int
	for _, idx := range batchCapPlacement {
		PlaceQueued(t, env, members[idx])
		boardOrder = append(boardOrder, members[idx])
	}

	const capN = 5
	var wantFirst, wantSecond []int
	if sortDisabled {
		// Board order: the first capN placed, then the remainder.
		wantFirst, wantSecond = boardOrder[:capN], boardOrder[capN:]
	} else {
		// The train's ordering: lowest issue numbers first.
		wantFirst, wantSecond = members[:capN], members[capN:]
	}
	sorted := func(s []int) []int {
		c := append([]int(nil), s...)
		sort.Ints(c)
		return c
	}
	wantFirst, wantSecond = sorted(wantFirst), sorted(wantSecond)

	var mu sync.Mutex
	var trialSets [][]int
	startTrialVerdictSeeder(t, env, func(m []int) []gh.CheckRun {
		mu.Lock()
		trialSets = append(trialSets, sorted(m))
		mu.Unlock()
		return allGreenVerdict(m)
	})

	// --- Poll 1: batch 1 (capped to N) lands; the remainder stays Queued. ---
	RunPoll(t, env)

	landings := batchCapLandings(t, env)
	if len(landings) != 1 {
		t.Fatalf("after poll 1: want exactly 1 merged landing PR (the capped first batch), got %d: %+v", len(landings), landings)
	}
	if !equalInts(landings[0].closes, wantFirst) {
		t.Fatalf("batch 1 closes %v, want exactly %v (sortDisabled=%v); all members: %v, board order: %v",
			landings[0].closes, wantFirst, sortDisabled, members, boardOrder)
	}
	for _, n := range wantFirst {
		item := projectItem(t, env, n)
		if item.Status != "Done" || !item.IsClosed {
			t.Errorf("after poll 1: batch-1 member #%d Status=%q closed=%v, want Done/closed", n, item.Status, item.IsClosed)
		}
	}
	for _, n := range wantSecond {
		item := projectItem(t, env, n)
		if item.Status != "Queued" || item.IsClosed {
			t.Errorf("after poll 1: remainder member #%d Status=%q closed=%v, want still Queued and open (batch 2 forms on the next poll)", n, item.Status, item.IsClosed)
		}
		if hasLabel(item.Labels, "fabrik:paused") {
			t.Errorf("after poll 1: remainder member #%d was paused", n)
		}
	}

	// --- Poll 2: the remainder (now under the cap) forms batch 2. ---
	RunPoll(t, env)

	landings = batchCapLandings(t, env)
	if len(landings) != 2 {
		t.Fatalf("after poll 2: want exactly 2 merged landing PRs (5 then 2), got %d: %+v", len(landings), landings)
	}
	if !equalInts(landings[0].closes, wantFirst) {
		t.Errorf("batch 1 closes %v, want %v", landings[0].closes, wantFirst)
	}
	if !equalInts(landings[1].closes, wantSecond) {
		t.Errorf("batch 2 closes %v, want exactly the remainder %v", landings[1].closes, wantSecond)
	}

	// Exactly once: the two Closes sets are disjoint and cover every member.
	landedBy := map[int]int{}
	for _, l := range landings {
		for _, n := range l.closes {
			landedBy[n]++
		}
	}
	for _, n := range members {
		if landedBy[n] != 1 {
			t.Errorf("member #%d is closed by %d landing PRs, want exactly 1", n, landedBy[n])
		}
		item := projectItem(t, env, n)
		if item.Status != "Done" || !item.IsClosed {
			t.Errorf("member #%d Status=%q closed=%v, want Done/closed", n, item.Status, item.IsClosed)
		}
		if hasLabel(item.Labels, "fabrik:paused") {
			t.Errorf("member #%d is paused", n)
		}
		if got := landedCommentCount(t, env, n, memberPRs[n]); got != 1 {
			t.Errorf("member #%d has %d \"Landed via\" comments, want exactly 1", n, got)
		}
		if hasCommentContaining(t, env, n, "ejected") {
			t.Errorf("member #%d carries an ejection comment", n)
		}
	}

	// Exactly two merges, one per batch (batch order is pinned by the
	// per-poll landing assertions above).
	if got := len(env.Sim.Log().ByMethod("MergePR")); got != 2 {
		t.Errorf("MergePR called %d times, want exactly 2 (one per batch)", got)
	}

	// Trials: exactly one per batch, of exactly N then the remainder.
	mu.Lock()
	gotTrials := append([][]int(nil), trialSets...)
	mu.Unlock()
	if len(gotTrials) != 2 {
		t.Fatalf("seeder observed %d trial PRs, want exactly 2 (no abandoned or re-formed trial): %v", len(gotTrials), gotTrials)
	}
	// The seeder's observation order is a goroutine race on a 2ms poll, so
	// compare as a set of sets rather than a sequence.
	if !(equalInts(gotTrials[0], wantFirst) && equalInts(gotTrials[1], wantSecond)) &&
		!(equalInts(gotTrials[1], wantFirst) && equalInts(gotTrials[0], wantSecond)) {
		t.Errorf("trial member sets = %v, want %v and %v", gotTrials, wantFirst, wantSecond)
	}

	// No unmerged-closed or open trial PR, and no stale trial branch.
	prs, err := env.Sim.ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	for _, pr := range prs {
		if strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) && (pr.State == "open" || !pr.Merged) {
			t.Errorf("stale/abandoned train PR #%d (%s): state=%s merged=%v", pr.Number, pr.HeadRefName, pr.State, pr.Merged)
		}
	}
	dir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	refs := exec.Command("git", "for-each-ref", "--format=%(refname)", "refs/heads/"+mergeTrainBranchPrefix)
	refs.Dir = dir
	if out, err := refs.Output(); err != nil {
		t.Errorf("listing trial branches: %v", err)
	} else if strings.TrimSpace(string(out)) != "" {
		t.Errorf("stale merge-train branches remain:\n%s", out)
	}
}

// batchCapLanding is one merged merge-train PR and the sorted member issue
// numbers its body closes.
type batchCapLanding struct {
	pr     int
	closes []int
}

// batchCapLandings returns the merged merge-train landing PRs in PR-number
// (creation) order, which is landing order for sequential batches.
func batchCapLandings(t *testing.T, env *Env) []batchCapLanding {
	t.Helper()
	prs, err := env.Sim.ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	var out []batchCapLanding
	for _, pr := range prs {
		if strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) && pr.Merged {
			c := parseClosesNumbers(pr.Body)
			sort.Ints(c)
			out = append(out, batchCapLanding{pr: pr.Number, closes: c})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pr < out[j].pr })
	return out
}

func equalInts(a, b []int) bool {
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
