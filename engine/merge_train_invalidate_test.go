package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// invalidateWorld is a real-git fixture for the post-landing scan: srcDir plays GitHub's
// origin (branches stay on it, so wm.FetchOrigin populates refs/remotes/origin/*), and
// wm.baseDir is the engine's bare clone.
type invalidateWorld struct {
	t      *testing.T
	eng    *Engine
	client *mockGitHubClient
	src    string
	wm     *WorktreeManager
	group  queuedRepoGroup
}

func sharedLines(replace map[int]string) string {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		if v, ok := replace[i]; ok {
			fmt.Fprintf(&b, "%s\n", v)
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	return b.String()
}

func newInvalidateWorld(t *testing.T) *invalidateWorld {
	t.Helper()
	skipIfNoGit(t)
	_, src, _, wm := setupTrainRepo(t)
	writeFile(t, filepath.Join(src, "shared.txt"), sharedLines(nil))
	mustGit(t, src, "add", "-A")
	mustGit(t, src, "commit", "-m", "shared file")

	client := &mockGitHubClient{}
	eng := trainTestEngine(t, client, &mockClaudeInvoker{}, wm)
	yes := true
	for _, s := range eng.cfg.Stages {
		if s.Name == "Implement" {
			s.WaitForCI = &yes
		}
	}
	eng.mu.Lock()
	eng.worktreeManagers["owner/repo"] = wm
	eng.mu.Unlock()
	return &invalidateWorld{t: t, eng: eng, client: client, src: src, wm: wm,
		group: queuedRepoGroup{repoKey: "owner/repo", base: "main", trainKey: mergeTrainKey("owner/repo", "main")}}
}

// member creates branch fabrik/issue-N on the origin from main, writing the given files.
func (w *invalidateWorld) member(n int, files map[string]string) gh.ProjectItem {
	w.t.Helper()
	mustGit(w.t, w.src, "checkout", "main")
	mustGit(w.t, w.src, "checkout", "-b", fmt.Sprintf("fabrik/issue-%d", n))
	for name, content := range files {
		writeFile(w.t, filepath.Join(w.src, name), content)
	}
	mustGit(w.t, w.src, "add", "-A")
	mustGit(w.t, w.src, "commit", "-m", fmt.Sprintf("member %d", n))
	mustGit(w.t, w.src, "checkout", "main")
	return gh.ProjectItem{Number: n, Title: fmt.Sprintf("Issue %d", n), ItemID: fmt.Sprintf("item-%d", n), Repo: "owner/repo", Status: "Queued"}
}

// land advances main on the origin, as a landing's merge would.
func (w *invalidateWorld) land(n int, files map[string]string) {
	w.t.Helper()
	mustGit(w.t, w.src, "checkout", "main")
	for name, content := range files {
		writeFile(w.t, filepath.Join(w.src, name), content)
	}
	mustGit(w.t, w.src, "add", "-A")
	mustGit(w.t, w.src, "commit", "-m", fmt.Sprintf("landed %d", n))
	w.eng.noteTrainLanded(w.group.trainKey, trainMember{item: gh.ProjectItem{Number: n}, prNum: 100 + n})
}

func (w *invalidateWorld) scan(candidates ...gh.ProjectItem) (remaining []int, logs string) {
	w.t.Helper()
	landed := w.eng.takeTrainLanded(w.group.trainKey)
	logs = captureStdout(func() {
		for _, it := range w.eng.invalidateConflictingQueued(w.group, "PVT_1", landed, candidates) {
			remaining = append(remaining, it.Number)
		}
	})
	return remaining, logs
}

func (w *invalidateWorld) statusMovesTo(optionID string) []string {
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	var items []string
	for _, c := range w.client.updateStatusCalls {
		if c.optionID == optionID {
			items = append(items, c.itemID)
		}
	}
	return items
}

func TestMergeTreeConflicts_CleanAndConflicting(t *testing.T) {
	w := newInvalidateWorld(t)
	w.member(2, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member two"})})
	w.member(3, map[string]string{"shared.txt": sharedLines(map[int]string{28: "member three"})})
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})
	if out, err := w.wm.FetchOrigin(); err != nil {
		t.Fatalf("fetch: %s", out)
	}

	paths, conflict, err := mergeTreeConflicts(w.wm.baseDir, "refs/remotes/origin/main", "refs/remotes/origin/fabrik/issue-2")
	if err != nil || !conflict || len(paths) != 1 || paths[0] != "shared.txt" {
		t.Fatalf("issue-2: paths=%v conflict=%v err=%v, want shared.txt conflict", paths, conflict, err)
	}
	// Overlapping on the same file but a different hunk merges cleanly.
	paths, conflict, err = mergeTreeConflicts(w.wm.baseDir, "refs/remotes/origin/main", "refs/remotes/origin/fabrik/issue-3")
	if err != nil || conflict || len(paths) != 0 {
		t.Fatalf("issue-3: paths=%v conflict=%v err=%v, want a clean merge", paths, conflict, err)
	}
	// An unknown ref is an error, never a conflict.
	if _, conflict, err = mergeTreeConflicts(w.wm.baseDir, "refs/remotes/origin/main", "refs/remotes/origin/fabrik/issue-404"); err == nil || conflict {
		t.Fatalf("missing ref: conflict=%v err=%v, want an error", conflict, err)
	}
}

func TestInvalidateConflictingQueued_ReroutesOnlyRealConflicts(t *testing.T) {
	w := newInvalidateWorld(t)
	conflicting := w.member(2, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member two"})})
	overlapClean := w.member(3, map[string]string{"shared.txt": sharedLines(map[int]string{28: "member three"})})
	disjoint := w.member(4, map[string]string{"other.txt": "x\n"})
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})

	remaining, logs := w.scan(conflicting, overlapClean, disjoint)
	if !sameInts(remaining, []int{3, 4}) {
		t.Fatalf("remaining %v, want [3 4]: only the real conflict leaves Queued", remaining)
	}
	if want := "invalidated #2: conflicts with landed #1 (merge-tree)"; !strings.Contains(logs, want) {
		t.Errorf("log missing %q:\n%s", want, logs)
	}
	if n := strings.Count(logs, "invalidated #"); n != 1 {
		t.Errorf("want one invalidation line, got %d:\n%s", n, logs)
	}
	if got := w.statusMovesTo("opt-implement"); len(got) != 1 || got[0] != "item-2" {
		t.Errorf("reroute moves = %v, want only item-2 to the preceding stage", got)
	}

	// FR-009: not an ejection, not a pause. rebase-needed is the only label written.
	if len(w.eng.mergeTrainEjectionCounts) != 0 {
		t.Errorf("reroute charged an ejection: %v", w.eng.mergeTrainEjectionCounts)
	}
	if got := pauseLabelsFor(w.client, 2); len(got) != 0 {
		t.Errorf("rerouted member was paused: %v", got)
	}
	w.client.mu.Lock()
	var labels []string
	for _, c := range w.client.addLabelCalls {
		labels = append(labels, fmt.Sprintf("#%d:%s", c.issueNumber, c.labelName))
	}
	nComments := len(w.client.addCommentCalls)
	w.client.mu.Unlock()
	if !sameStrings(labels, []string{"#2:fabrik:rebase-needed"}) {
		t.Errorf("labels written = %v, want only #2:fabrik:rebase-needed", labels)
	}
	if nComments != 1 {
		t.Errorf("comments = %d, want exactly one", nComments)
	}
}

func sameStrings(a, b []string) bool {
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

func TestInvalidateConflictingQueued_NeverTouchesLandedMemberOrFailsClosed(t *testing.T) {
	w := newInvalidateWorld(t)
	landedItem := w.member(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member one"})})
	missing := gh.ProjectItem{Number: 9, ItemID: "item-9", Repo: "owner/repo", Status: "Queued"} // no branch on origin
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})

	remaining, logs := w.scan(landedItem, missing)
	if !sameInts(remaining, []int{1, 9}) {
		t.Fatalf("remaining %v, want both left alone (the landed member itself, and a member whose ref cannot be resolved)", remaining)
	}
	if !strings.Contains(logs, "cannot resolve refs/remotes/origin/fabrik/issue-9") {
		t.Errorf("unresolvable member not logged:\n%s", logs)
	}
	if got := w.statusMovesTo("opt-implement"); len(got) != 0 {
		t.Errorf("unexpected reroutes: %v", got)
	}
}

func TestInvalidateConflictingQueued_CommentDedupedPerHead(t *testing.T) {
	w := newInvalidateWorld(t)
	conflicting := w.member(2, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member two"})})
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})
	w.scan(conflicting)
	// The same head bounces back to Queued and a second landing triggers another scan.
	w.eng.noteTrainLanded(w.group.trainKey, trainMember{item: gh.ProjectItem{Number: 1}, prNum: 101})
	w.scan(conflicting)
	w.client.mu.Lock()
	defer w.client.mu.Unlock()
	if len(w.client.addCommentCalls) != 1 {
		t.Errorf("comments = %d, want 1 (deduped per member+head)", len(w.client.addCommentCalls))
	}
}

func TestInvalidateConflictingQueued_SkippedWithoutWaitForCITarget(t *testing.T) {
	w := newInvalidateWorld(t)
	for _, s := range w.eng.cfg.Stages {
		s.WaitForCI = nil
	}
	conflicting := w.member(2, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member two"})})
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})
	remaining, _ := w.scan(conflicting)
	if !sameInts(remaining, []int{2}) || len(w.statusMovesTo("opt-implement")) != 0 {
		t.Fatalf("remaining %v — without wait_for_ci on the target nothing would re-detect the member, so it must not move", remaining)
	}
}

func TestLandedAttribution(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.overlap.files = map[overlapFileKey]overlapFileEntry{
		{repoKey: "o/r", pr: 101}: {headSHA: "a", files: []string{"a.go", "shared.go"}},
		{repoKey: "o/r", pr: 102}: {headSHA: "b", files: []string{"b.go"}},
	}
	landed := []landedMember{{number: 1, prNum: 101}, {number: 2, prNum: 102}}
	if got := eng.landedAttribution("o", "r", landed, []string{"shared.go"}); !sameInts(got, []int{1}) {
		t.Errorf("attribution = %v, want [1] (only #1's files intersect)", got)
	}
	if got := eng.landedAttribution("o", "r", landed, []string{"unrelated.go"}); !sameInts(got, []int{1, 2}) {
		t.Errorf("fallback attribution = %v, want every landed member", got)
	}
}

func TestRouteQueuedGroup_ScanWaitsForInFlightWorker(t *testing.T) {
	w := newInvalidateWorld(t)
	conflicting := w.member(2, map[string]string{"shared.txt": sharedLines(map[int]string{2: "member two"})})
	w.land(1, map[string]string{"shared.txt": sharedLines(map[int]string{2: "landed"})})

	// A worker owns the partition: the poll goroutine must not touch its members, and
	// the landed signal must survive for after the worker exits.
	key := w.group.trainKey
	w.eng.mergeTrainInFlight.Store(key, &mergeTrainWorkerState{batchNumbers: map[int]bool{2: true}})
	g := w.group
	g.items = []gh.ProjectItem{conflicting}
	w.eng.routeQueuedGroup(t.Context(), g, "PVT_1")
	if got := w.statusMovesTo("opt-implement"); len(got) != 0 {
		t.Fatalf("scan ran while a worker was in flight: %v", got)
	}
	if !w.eng.hasTrainLanded(key) {
		t.Fatal("landed signal was consumed while a worker was in flight")
	}
}

func TestNoteTrainLanded_DisabledSeamRecordsNothing(t *testing.T) {
	eng := trainTestEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	eng.SetMergeTrainInvalidationDisabledForTest(true)
	eng.noteTrainLanded("k", trainMember{item: gh.ProjectItem{Number: 1}})
	if eng.hasTrainLanded("k") {
		t.Fatal("disabled seam still recorded a landing")
	}
}
