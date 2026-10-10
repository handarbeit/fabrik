package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// overlapFiles is a per-PR changed-file table backing mockGitHubClient.FetchPRFiles,
// keyed by the member's *issue* number for readability (PR number = 100 + issue in
// admissionMembers). It also counts reads per PR.
type overlapFiles struct {
	mu    sync.Mutex
	files map[int][]string // PR number -> files
	errs  map[int]error
	reads map[int]int
}

func newOverlapEngine(t *testing.T, files map[int][]string, ignore ...string) (*Engine, *overlapFiles, *mockGitHubClient) {
	t.Helper()
	of := &overlapFiles{files: map[int][]string{}, errs: map[int]error{}, reads: map[int]int{}}
	for issue, fs := range files {
		of.files[100+issue] = fs
	}
	client := &mockGitHubClient{}
	client.fetchPRFilesFn = func(owner, repo string, pr int) ([]string, error) {
		of.mu.Lock()
		defer of.mu.Unlock()
		of.reads[pr]++
		if err := of.errs[pr]; err != nil {
			return nil, err
		}
		return of.files[pr], nil
	}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.cfg.MergeTrainOverlapIgnore = ignore
	return eng, of, client
}

const overlapTestKey = "o/r"

func admitOverlap(eng *Engine, members []trainMember) (out []int, logs string) {
	logs = captureStdout(func() {
		out = numbersOf(eng.admitByOverlap(overlapTestKey, "o", "r", members))
	})
	return out, logs
}

func TestAdmitByOverlap_OverlappingNeverTogether_DisjointJoinsFirst(t *testing.T) {
	eng, _, _ := newOverlapEngine(t, map[int][]string{
		1: {"mcp/src/index.ts", "a.go"},
		2: {"mcp/src/index.ts", "b.go"},
		3: {"c.go"},
	})
	got, logs := admitOverlap(eng, admissionMembers(1, 2, 3))
	if !sameInts(got, []int{1, 3}) {
		t.Fatalf("admitted %v, want [1 3] (#2 overlaps #1; disjoint #3 joins)", got)
	}
	if want := "deferred #2: overlaps #1 on mcp/src/index.ts"; !strings.Contains(logs, want) {
		t.Errorf("log missing %q:\n%s", want, logs)
	}
	if n := strings.Count(logs, "deferred #"); n != 1 {
		t.Errorf("want exactly one deferral line, got %d:\n%s", n, logs)
	}
}

func TestAdmitByOverlap_LogNamesSmallestSharedPath(t *testing.T) {
	eng, _, _ := newOverlapEngine(t, map[int][]string{
		1: {"z.go", "m.go", "a.go"},
		2: {"z.go", "m.go"},
	})
	_, logs := admitOverlap(eng, admissionMembers(1, 2))
	if !strings.Contains(logs, "deferred #2: overlaps #1 on m.go") {
		t.Errorf("want the lexicographically smallest shared path (m.go):\n%s", logs)
	}
}

func TestAdmitByOverlap_OverlapIsAgainstAdmittedMembersOnly(t *testing.T) {
	// #2 is deferred, so #3 (which overlaps only #2) must still be admitted.
	eng, _, _ := newOverlapEngine(t, map[int][]string{
		1: {"a.go"},
		2: {"a.go", "b.go"},
		3: {"b.go"},
	})
	got, _ := admitOverlap(eng, admissionMembers(1, 2, 3))
	if !sameInts(got, []int{1, 3}) {
		t.Fatalf("admitted %v, want [1 3]", got)
	}
}

func TestAdmitByOverlap_FirstMemberAlwaysAdmitted_FullOverlapIsSingleton(t *testing.T) {
	eng, _, _ := newOverlapEngine(t, map[int][]string{
		1: {"hot.go"}, 2: {"hot.go"}, 3: {"hot.go"},
	})
	got, _ := admitOverlap(eng, admissionMembers(1, 2, 3))
	if !sameInts(got, []int{1}) {
		t.Fatalf("admitted %v, want the singleton [1]", got)
	}
}

func TestAdmitByOverlap_UnreadableFilesFailOpen(t *testing.T) {
	eng, of, _ := newOverlapEngine(t, map[int][]string{
		1: {"a.go"},
		2: {"a.go"},
		3: {"a.go"},
	})
	of.errs[102] = errors.New("boom")
	got, logs := admitOverlap(eng, admissionMembers(1, 2, 3))
	// #2 is unreadable → admitted unchecked, and its files do not enter the union;
	// #3 still overlaps #1 and is deferred.
	if !sameInts(got, []int{1, 2}) {
		t.Fatalf("admitted %v, want [1 2]", got)
	}
	if !strings.Contains(logs, "admitting #2 unchecked") {
		t.Errorf("fail-open admission not logged:\n%s", logs)
	}

	// The failure must not be cached as an empty list: the next formation re-reads.
	of.mu.Lock()
	before := of.reads[102]
	of.mu.Unlock()
	admitOverlap(eng, admissionMembers(1, 2, 3))
	of.mu.Lock()
	after := of.reads[102]
	of.mu.Unlock()
	if after != before+1 {
		t.Errorf("failed read was cached: reads %d -> %d, want a retry", before, after)
	}
}

func TestAdmitByOverlap_NoFilesReturnedFailsOpen(t *testing.T) {
	eng, _, _ := newOverlapEngine(t, map[int][]string{1: {"a.go"}, 2: nil})
	got, _ := admitOverlap(eng, admissionMembers(1, 2))
	if !sameInts(got, []int{1, 2}) {
		t.Fatalf("admitted %v, want [1 2] (an empty/404 list is not evidence of disjointness being false)", got)
	}
}

func TestAdmitByOverlap_TruncatedListFailsOpenAndIsCached(t *testing.T) {
	big := make([]string, prFilesTruncationLimit)
	for i := range big {
		big[i] = fmt.Sprintf("gen/f%d.go", i)
	}
	big[0] = "a.go"
	eng, of, _ := newOverlapEngine(t, map[int][]string{1: {"a.go"}, 2: big})
	got, logs := admitOverlap(eng, admissionMembers(1, 2))
	if !sameInts(got, []int{1, 2}) {
		t.Fatalf("admitted %v, want [1 2] (a >=3000 list cannot be known complete)", got)
	}
	if !strings.Contains(logs, "cannot be known complete") {
		t.Errorf("truncation not logged:\n%s", logs)
	}
	admitOverlap(eng, admissionMembers(1, 2))
	if of.reads[102] != 1 {
		t.Errorf("oversized list re-read %d times, want 1 (cached per head SHA)", of.reads[102])
	}
}

func TestAdmitByOverlap_OneReadPerHeadSHA(t *testing.T) {
	eng, of, _ := newOverlapEngine(t, map[int][]string{1: {"a.go"}, 2: {"b.go"}})
	ms := admissionMembers(1, 2)
	for i := 0; i < 3; i++ {
		admitOverlap(eng, ms)
	}
	if of.reads[101] != 1 || of.reads[102] != 1 {
		t.Fatalf("reads = %v, want one per PR across three formations", of.reads)
	}

	// A new head SHA invalidates only that PR's entry.
	ms[0].headSHA = "sha1-new"
	admitOverlap(eng, ms)
	if of.reads[101] != 2 || of.reads[102] != 1 {
		t.Fatalf("after a head change reads = %v, want PR 101 re-read once and PR 102 untouched", of.reads)
	}
}

func TestAdmitByOverlap_IgnoreGlobs(t *testing.T) {
	files := map[int][]string{
		1: {"web/yarn.lock", "a.go"},
		2: {"web/yarn.lock", "b.go"},
	}
	t.Run("ignored path is not overlap", func(t *testing.T) {
		eng, _, _ := newOverlapEngine(t, files, "**/*.lock")
		got, _ := admitOverlap(eng, admissionMembers(1, 2))
		if !sameInts(got, []int{1, 2}) {
			t.Fatalf("admitted %v, want [1 2]", got)
		}
	})
	t.Run("default counts every path", func(t *testing.T) {
		eng, _, _ := newOverlapEngine(t, files)
		got, _ := admitOverlap(eng, admissionMembers(1, 2))
		if !sameInts(got, []int{1}) {
			t.Fatalf("admitted %v, want [1]", got)
		}
	})
	t.Run("un-anchored glob does not cross directories", func(t *testing.T) {
		eng, _, _ := newOverlapEngine(t, files, "*.lock")
		got, _ := admitOverlap(eng, admissionMembers(1, 2))
		if !sameInts(got, []int{1}) {
			t.Fatalf("admitted %v, want [1] (*.lock matches only top-level files)", got)
		}
	})
	t.Run("fully ignored member never overlaps anyone", func(t *testing.T) {
		eng, _, _ := newOverlapEngine(t, map[int][]string{
			1: {"a.go"}, 2: {"CHANGELOG.md"}, 3: {"CHANGELOG.md"}, 4: {"CHANGELOG.md", "a.go"},
		}, "CHANGELOG.md")
		got, _ := admitOverlap(eng, admissionMembers(1, 2, 3, 4))
		if !sameInts(got, []int{1, 2, 3}) {
			t.Fatalf("admitted %v, want [1 2 3] (#4 still overlaps #1 on a.go)", got)
		}
	})
}

func TestAdmitByOverlap_StarvationGuard(t *testing.T) {
	// #2 overlaps #1 and is deferred each formation until its count reaches the threshold.
	eng, _, _ := newOverlapEngine(t, map[int][]string{
		1: {"hot.go"}, 2: {"hot.go"}, 3: {"other.go"},
	})
	for i := 1; i <= overlapStarvationThreshold; i++ {
		got, _ := admitOverlap(eng, admissionMembers(1, 2, 3))
		if !sameInts(got, []int{1, 3}) {
			t.Fatalf("formation %d admitted %v, want [1 3]", i, got)
		}
	}
	got, logs := admitOverlap(eng, admissionMembers(1, 2, 3))
	// #2 is admitted first regardless of overlap and #1 now defers against it. Survivors
	// keep today's relative order.
	if !sameInts(got, []int{2, 3}) {
		t.Fatalf("starved formation admitted %v, want [2 3]", got)
	}
	if !strings.Contains(logs, "admitting #2 first") || !strings.Contains(logs, "deferred #1: overlaps #2 on hot.go") {
		t.Errorf("starvation admission not logged as expected:\n%s", logs)
	}

	// Once admitted, #2's count is reset: the next formation (#2 gone, #1 back) is normal.
	got, _ = admitOverlap(eng, admissionMembers(1, 3))
	if !sameInts(got, []int{1, 3}) {
		t.Fatalf("after reset admitted %v, want [1 3]", got)
	}
}

func TestAdmitByOverlap_SkipCountResetsWhenMemberLeavesBatch(t *testing.T) {
	eng, _, _ := newOverlapEngine(t, map[int][]string{1: {"hot.go"}, 2: {"hot.go"}})
	admitOverlap(eng, admissionMembers(1, 2))
	admitOverlap(eng, admissionMembers(1, 2))
	// #2 leaves the formation (left Queued / reordered out): its count must reset.
	admitOverlap(eng, admissionMembers(1))
	for i := 0; i < overlapStarvationThreshold-1; i++ {
		got, _ := admitOverlap(eng, admissionMembers(1, 2))
		if !sameInts(got, []int{1}) {
			t.Fatalf("round %d admitted %v, want [1] — #2's earlier skips must not carry over", i, got)
		}
	}
}

func TestAdmitByOverlap_SkipCountsArePerPartition(t *testing.T) {
	eng, _, _ := newOverlapEngine(t, map[int][]string{1: {"hot.go"}, 2: {"hot.go"}})
	eng.admitByOverlap("o/r", "o", "r", admissionMembers(1, 2))
	eng.admitByOverlap("o/r", "o", "r", admissionMembers(1, 2))
	// A formation of an unrelated partition must not clear this partition's counts.
	eng.admitByOverlap("o/r|release", "o", "r", admissionMembers(3))
	eng.admitByOverlap("o/r", "o", "r", admissionMembers(1, 2))
	got := numbersOf(eng.admitByOverlap("o/r", "o", "r", admissionMembers(1, 2)))
	if !sameInts(got, []int{2}) {
		t.Fatalf("admitted %v, want the starved [2] first (counts survived the other partition's formation)", got)
	}
}

func TestAdmitByOverlap_DisabledSeamAdmitsEverything(t *testing.T) {
	eng, of, _ := newOverlapEngine(t, map[int][]string{1: {"a.go"}, 2: {"a.go"}})
	eng.SetMergeTrainOverlapDisabledForTest(true)
	got, _ := admitOverlap(eng, admissionMembers(1, 2))
	if !sameInts(got, []int{1, 2}) || len(of.reads) != 0 {
		t.Fatalf("disabled filter admitted %v with reads %v, want all and no reads", got, of.reads)
	}
}

func TestAdmitByOverlap_NoSideEffectsOnDeferredMember(t *testing.T) {
	eng, _, client := newOverlapEngine(t, map[int][]string{1: {"a.go"}, 2: {"a.go"}})
	admitOverlap(eng, admissionMembers(1, 2))
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.addLabelCalls) != 0 || len(client.updateStatusCalls) != 0 {
		t.Errorf("deferral wrote to GitHub: labels=%v statuses=%v", client.addLabelCalls, client.updateStatusCalls)
	}
	if len(eng.mergeTrainEjectionCounts) != 0 {
		t.Errorf("deferral charged an ejection: %v", eng.mergeTrainEjectionCounts)
	}
}
