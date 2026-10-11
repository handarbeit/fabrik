package engine

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// ─── Parity oracle for the merge-train re-form / bisection / one-at-a-time logic ───
//
// refTrainModel is a deliberately naive, engine-independent re-statement of the
// halving order (ADR-059 D4, bors-ng order), the per-episode cost cap and the
// one-at-a-time fallback, written as the original recursive algorithm over plain
// integers. The tests below drive the real worker through the trainValidateFn seam
// and require the sequence of validated member sets, the ejected members and the
// number of landings to equal the model's exactly. It exists to pin the behaviour
// across the #2051 rewrite of bisect into an explicit state machine (R4).

type refVerdict func(present []int) TrainCIResult

type refOutcome struct {
	calls   [][]int
	ejected []int
	landed  int // integration/singleton landings (one merge each)
}

func refHas(set []int, n int) bool {
	for _, v := range set {
		if v == n {
			return true
		}
	}
	return false
}

// refRun mirrors runMergeTrainWorker for a batch of >= 2 members under the seam.
func refRun(batch []int, verdict refVerdict, costCap int) refOutcome {
	var out refOutcome
	validate := func(set []int) TrainCIResult {
		cp := append([]int(nil), set...)
		out.calls = append(out.calls, cp)
		return verdict(cp)
	}
	var bisect func(red []int, used *int) (poisoner int, fellBack bool)
	bisect = func(red []int, used *int) (int, bool) {
		if len(red) == 1 {
			return red[0], false
		}
		mid := len(red) / 2
		for _, half := range [][]int{red[:mid], red[mid:]} {
			if *used >= costCap {
				return 0, true
			}
			res := validate(half)
			*used++
			if res == TrainCIRed && len(half) > 0 {
				return bisect(half, used)
			}
		}
		return 0, true
	}

	current := append([]int(nil), batch...)
	for {
		if len(current) == 0 {
			return out
		}
		res := validate(current)
		switch res {
		case TrainCIGreen:
			out.landed++
			return out
		case TrainCIPending, TrainCIInfra:
			return out
		}
		if len(current) == 1 {
			out.ejected = append(out.ejected, current[0])
			return out
		}
		used := 1
		poisoner, fellBack := bisect(current, &used)
		if fellBack {
			for _, m := range current {
				r := validate([]int{m})
				switch r {
				case TrainCIGreen:
					out.landed++
				case TrainCIRed:
					out.ejected = append(out.ejected, m)
				}
			}
			return out
		}
		out.ejected = append(out.ejected, poisoner)
		var rest []int
		for _, m := range current {
			if m != poisoner {
				rest = append(rest, m)
			}
		}
		current = rest
	}
}

type scriptedValidator struct {
	mu      sync.Mutex
	calls   [][]int
	verdict refVerdict
}

func (sv *scriptedValidator) fn(_ context.Context, members []trainMember) (TrainCIResult, *trainCIDiagnostic) {
	nums := make([]int, 0, len(members))
	for _, m := range members {
		nums = append(nums, m.item.Number)
	}
	sv.mu.Lock()
	sv.calls = append(sv.calls, nums)
	sv.mu.Unlock()
	res := sv.verdict(nums)
	if res == TrainCIRed {
		return res, &trainCIDiagnostic{PRNum: 900, TrialSHA: "seam-trial-sha", Note: "synthetic seam failure"}
	}
	return res, nil
}

func runParityScenario(t *testing.T, n int, verdict refVerdict, costCap int) (refOutcome, refOutcome) {
	t.Helper()
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	sv := &scriptedValidator{verdict: verdict}
	eng.trainValidateFn = sv.fn
	if costCap > 0 {
		eng.cfg.MaxBisectValidations = costCap
	}

	batch := makeSeamBatch(n)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_test"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch)

	var got refOutcome
	sv.mu.Lock()
	got.calls = append(got.calls, sv.calls...)
	sv.mu.Unlock()
	for i := 1; i <= n; i++ {
		if parityDisposed(client, i) {
			got.ejected = append(got.ejected, i)
		}
	}
	client.mu.Lock()
	got.landed = len(client.mergePRCalls)
	client.mu.Unlock()

	nums := make([]int, n)
	for i := range nums {
		nums[i] = i + 1
	}
	want := refRun(nums, verdict, eng.effectiveBisectCap())
	sort.Ints(got.ejected)
	sort.Ints(want.ejected)
	return got, want
}

// parityDisposed reports whether member n was ejected by the train: either the
// multi-member ejection comment or the red-singleton "validation failed" disposition.
func parityDisposed(client *mockGitHubClient, n int) bool {
	if ejectionCommentCount(client, n) > 0 {
		return true
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addCommentCalls {
		if c.issueNumber == n && strings.Contains(c.body, "validation failed") {
			return true
		}
	}
	return false
}

func assertParity(t *testing.T, got, want refOutcome) {
	t.Helper()
	if !reflect.DeepEqual(got.calls, want.calls) {
		t.Errorf("validated member sets differ\n got: %v\nwant: %v", got.calls, want.calls)
	}
	if fmt.Sprint(got.ejected) != fmt.Sprint(want.ejected) {
		t.Errorf("ejected members differ: got %v want %v", got.ejected, want.ejected)
	}
	if got.landed != want.landed {
		t.Errorf("landings differ: got %d want %d", got.landed, want.landed)
	}
}

func TestTrainParity_SinglePoisonerEveryIndex(t *testing.T) {
	skipIfNoGit(t)
	for n := 2; n <= 8; n++ {
		for poison := 1; poison <= n; poison++ {
			n, poison := n, poison
			t.Run(fmt.Sprintf("n%d_poison%d", n, poison), func(t *testing.T) {
				verdict := func(set []int) TrainCIResult {
					if refHas(set, poison) {
						return TrainCIRed
					}
					return TrainCIGreen
				}
				got, want := runParityScenario(t, n, verdict, 0)
				assertParity(t, got, want)
			})
		}
	}
}

// Red only when two specific members co-reside: both halves green, so bisection
// degrades to the one-at-a-time fallback (a non-isolable interaction).
func TestTrainParity_InteractionFallsBack(t *testing.T) {
	skipIfNoGit(t)
	for _, tc := range []struct{ n, a, b int }{{4, 1, 4}, {5, 2, 5}, {6, 1, 6}, {3, 1, 3}} {
		tc := tc
		t.Run(fmt.Sprintf("n%d_%d_%d", tc.n, tc.a, tc.b), func(t *testing.T) {
			verdict := func(set []int) TrainCIResult {
				if refHas(set, tc.a) && refHas(set, tc.b) {
					return TrainCIRed
				}
				return TrainCIGreen
			}
			got, want := runParityScenario(t, tc.n, verdict, 0)
			assertParity(t, got, want)
			if want.landed == 0 {
				t.Fatalf("scenario should land singletons in the fallback, model says %+v", want)
			}
		})
	}
}

// A tiny cost cap forces the one-at-a-time fallback before the poisoner is isolated.
func TestTrainParity_CostCapExhaustion(t *testing.T) {
	skipIfNoGit(t)
	for capN := 1; capN <= 3; capN++ {
		for _, poison := range []int{1, 3, 6} {
			capN, poison := capN, poison
			t.Run(fmt.Sprintf("cap%d_poison%d", capN, poison), func(t *testing.T) {
				verdict := func(set []int) TrainCIResult {
					if refHas(set, poison) {
						return TrainCIRed
					}
					return TrainCIGreen
				}
				got, want := runParityScenario(t, 6, verdict, capN)
				assertParity(t, got, want)
			})
		}
	}
}

// A pending sub-trial is treated as "not red" — it falls through to the next half and,
// failing that, the fallback — exactly as the recursive algorithm did.
func TestTrainParity_PendingSubTrial(t *testing.T) {
	skipIfNoGit(t)
	verdict := func(set []int) TrainCIResult {
		switch {
		case len(set) == 4: // initial batch red
			return TrainCIRed
		case len(set) == 2 && refHas(set, 1): // first half pending
			return TrainCIPending
		case refHas(set, 4):
			return TrainCIRed
		}
		return TrainCIGreen
	}
	got, want := runParityScenario(t, 4, verdict, 0)
	assertParity(t, got, want)
}

// Two independent poisoners: the first is ejected, the survivors are re-formed and
// re-validated, and the second is found on the next episode.
func TestTrainParity_TwoPoisoners(t *testing.T) {
	skipIfNoGit(t)
	verdict := func(set []int) TrainCIResult {
		if refHas(set, 2) || refHas(set, 7) {
			return TrainCIRed
		}
		return TrainCIGreen
	}
	got, want := runParityScenario(t, 8, verdict, 0)
	assertParity(t, got, want)
}

var _ = gh.CheckRun{}
