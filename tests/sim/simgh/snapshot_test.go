package simgh

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// seedRichSim builds a model touching every collection Snapshot has to carry:
// issues with labels and comments and reactions, a PR with reviews, requests,
// review threads, check runs, commit statuses, branch protection, a recompute
// window, a merge-queue entry, a release, and both an undrained and a soon-due
// schedule step.
func seedRichSim(t *testing.T) (*Sim, *fakeClock, string) {
	t.Helper()
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"}).
		SeedIssue("acme/widgets", IssueSeed{
			Number: 7, Title: "Add a thing", Body: "spec", Author: "human",
			Labels: []string{"fabrik:awaiting-ci"}, Assignees: []string{"bot"}, Status: "Implement",
		}).
		SeedComment("acme/widgets", 7, "human", "please hurry").
		SeedCommit("acme/widgets", "fabrik/issue-7", map[string]string{"a.txt": "a"}, "work").
		SeedPR("acme/widgets", PRSeed{Number: 8, Title: "PR", Body: "body", Head: "fabrik/issue-7", IssueNumber: 7}).
		SeedReview("acme/widgets", 8, gh.PRReview{Author: "human", State: "APPROVED"}).
		SeedReviewRequest("acme/widgets", 8, gh.ReviewRequest{Login: "second-human"}).
		SeedReviewThreadComment("acme/widgets", 8, "human", "nit", "a.txt", 1).
		SeedRequiredApprovals("acme/widgets", "main", 1).
		SeedRequireUpToDate("acme/widgets", "main", true).
		SeedRepoAccess("acme/widgets", gh.RepoAccess{CanPush: true}).
		SeedLatestRelease("acme/widgets", gh.LatestRelease{TagName: "v1.2.3"}).
		SeedMergeQueueEnabled("acme/widgets", true).
		SeedRateLimits(5000, 321, 5000, 654)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	sha := mustHeadSHA(t, s, "acme/widgets", "fabrik/issue-7")
	s.SeedRequiredContexts("acme/widgets", "main", []string{"build"}).
		SeedCheckRun("acme/widgets", sha, gh.CheckRun{ID: 900, Name: "build", Conclusion: "success"}).
		SeedCommitStatus("acme/widgets", sha, gh.CommitStatus{Context: "legacy", State: "success"}).
		// Two steps: one due within the test's reach, one far out so it is
		// still undrained when the snapshot is taken.
		SeedCheckRunsAfter("acme/widgets", sha, time.Hour,
			gh.CheckRun{ID: 900, Name: "build", Status: "completed", Conclusion: "failure"}).
		SeedReviewsAfter("acme/widgets", 8, 2*time.Hour, gh.PRReview{Author: "late", State: "CHANGES_REQUESTED"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return s, clk, sha
}

// readModel captures everything a restored Sim must reproduce. Comparing the
// whole projection rather than a handful of fields is what makes "every read
// returns what it returned before" an assertion rather than a claim.
type modelReading struct {
	board        *gh.ProjectBoard
	labels       []string
	issueBody    string
	prDetails    *gh.PRDetails
	checkRuns    []gh.CheckRun
	statuses     []gh.CommitStatus
	reviews      []gh.PRReview
	requests     []gh.ReviewRequest
	decision     string
	mergeState   string
	restLimit    gh.RateLimitStats
	release      *gh.LatestRelease
	labelApplied time.Time
}

func readModel(t *testing.T, s *Sim, sha string) modelReading {
	t.Helper()
	var m modelReading
	var err error
	if m.board, err = s.FetchProjectBoard("acme", "widgets", 2, "organization"); err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if m.labels, err = s.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if m.issueBody, err = s.GetIssueBody("acme", "widgets", 7); err != nil {
		t.Fatalf("GetIssueBody: %v", err)
	}
	if m.prDetails, err = s.FetchPRDetails("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRDetails: %v", err)
	}
	if m.checkRuns, err = s.FetchCheckRuns("acme", "widgets", sha); err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if m.statuses, err = s.FetchCombinedStatus("acme", "widgets", sha); err != nil {
		t.Fatalf("FetchCombinedStatus: %v", err)
	}
	if m.reviews, err = s.FetchPRReviews("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if m.requests, err = s.FetchPRReviewRequests("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if m.decision, err = s.FetchPRReviewDecision("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRReviewDecision: %v", err)
	}
	if m.mergeState, err = s.FetchPRMergeableState("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRMergeableState: %v", err)
	}
	m.restLimit, _ = s.RateLimitStats()
	if m.release, err = s.FetchLatestRelease("acme", "widgets"); err != nil {
		t.Fatalf("FetchLatestRelease: %v", err)
	}
	if m.labelApplied, err = s.FetchLabelAppliedAt("acme", "widgets", 7, "fabrik:awaiting-ci"); err != nil {
		t.Fatalf("FetchLabelAppliedAt: %v", err)
	}
	return m
}

// TestSnapshotRestoreRoundTripsTheModel is the first half of AC7: after a
// restore into a fresh baseDir, every read returns what it returned before.
func TestSnapshotRestoreRoundTripsTheModel(t *testing.T) {
	s, clk, sha := seedRichSim(t)
	before := readModel(t, s, sha)

	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// A fresh baseDir every time, exactly as a real restart scenario would —
	// which is also what would expose a stale absolute path in git's worktree
	// admin entries.
	restored, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after := readModel(t, restored, sha)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("restored model differs from the original\nbefore: %+v\nafter:  %+v", before, after)
	}
}

// Git-derived answers must be recomputed against the copied repository, not
// replayed from a cached value — this is what would actually fail if a stale
// worktree admin entry survived the copy, or if the objects did not.
func TestRestoredGitStateAnswersMergeabilityForReal(t *testing.T) {
	s, clk, _ := seedRichSim(t)
	// Make the base advance so the PR is genuinely behind, and require
	// up-to-date heads — an answer only git can give.
	s.SeedCommit("acme/widgets", "main", map[string]string{"b.txt": "b"}, "move base")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restored, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	behind, err := restored.FetchCommitsBehind("acme", "widgets", "main", "fabrik/issue-7")
	if err != nil {
		t.Fatalf("FetchCommitsBehind on the restored repo: %v", err)
	}
	if behind != 1 {
		t.Errorf("commits behind = %d, want 1 — recomputed from the copied repository", behind)
	}
	state, err := restored.FetchPRMergeableState("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRMergeableState: %v", err)
	}
	if state != "behind" {
		t.Errorf("mergeable state = %q, want behind", state)
	}

	if err := restored.MergePR("acme", "widgets", 8); err == nil {
		t.Error("MergePR succeeded on a PR that is behind with up-to-date required")
	}

	// The copied repository must also still be *writable*, not merely
	// readable: drop the up-to-date requirement and let the merge write a real
	// commit into the copied bare repo. A copy that lost its object database's
	// permissions, or carried a stale worktree lock, fails here and nowhere
	// else.
	restored.SeedRequireUpToDate("acme/widgets", "main", false)
	if err := restored.Err(); err != nil {
		t.Fatalf("SeedRequireUpToDate on the restored Sim: %v", err)
	}
	if err := restored.MergePR("acme", "widgets", 8); err != nil {
		t.Fatalf("MergePR on the restored repository: %v", err)
	}
	merged, err := restored.FetchPRMerged("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRMerged: %v", err)
	}
	if !merged {
		t.Error("the merge did not record against the restored repository")
	}
}

// TestRestoreResumesScheduledSequencesAtTheSamePosition is the second half of
// AC7: an undrained step still fires at its original instant, and a drained
// one stays drained.
func TestRestoreResumesScheduledSequencesAtTheSamePosition(t *testing.T) {
	s, clk, sha := seedRichSim(t)

	// Drain the CI step before snapshotting; leave the review step pending.
	clk.Advance(time.Hour)
	runs, err := s.FetchCheckRuns("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 1 || checkVerdict(runs[0]) != "failure" {
		t.Fatalf("pre-snapshot CI step did not land: %+v", runs)
	}

	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restoredClock := newFakeClock()
	restoredClock.t = clk.Now()
	restored, err := Restore(snap, t.TempDir(), WithClock(restoredClock))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The drained step stays drained: its effect persists and it does not
	// re-apply.
	runs, err = restored.FetchCheckRuns("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCheckRuns after restore: %v", err)
	}
	if len(runs) != 1 || checkVerdict(runs[0]) != "failure" {
		t.Errorf("drained CI step did not survive the restore: %+v", runs)
	}

	// The undrained one has not fired yet…
	reviews, err := restored.FetchPRReviews("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("got %d reviews before the pending step's instant, want 1", len(reviews))
	}

	// …and fires at its original instant, unchanged by the restart.
	restoredClock.Advance(time.Hour)
	reviews, err = restored.FetchPRReviews("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Errorf("got %d reviews after the pending step's instant, want 2 — the step did not survive the restore", len(reviews))
	}
}

// The recompute counter is read-count state, so it must resume mid-drain
// rather than resetting to its seeded value or to zero.
func TestRestoreResumesTheRecomputeCounter(t *testing.T) {
	s, clk, _ := seedRichSim(t)
	s.SeedMergeableRecomputePending("acme/widgets", 8, 3)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if _, _, err := s.FetchPRMergeableFields("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRMergeableFields: %v", err)
	}

	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restored, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Two reads left in the window, then the real answer.
	for i := 0; i < 2; i++ {
		mergeable, state, err := restored.FetchPRMergeableFields("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
		if mergeable != nil || state != "unknown" {
			t.Fatalf("read %d after restore = (%v, %q), want the window to still be draining", i+1, mergeable, state)
		}
	}
	mergeable, _, err := restored.FetchPRMergeableFields("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("read 3: %v", err)
	}
	if mergeable == nil {
		t.Error("the recompute window did not drain after restore — the counter reset instead of resuming")
	}
}

// The restored model must be an independent copy: mutating one must not be
// visible through the other, or a "restart" scenario would still be sharing
// state with the engine it claims to have discarded.
func TestRestoredModelIsIndependentOfTheOriginal(t *testing.T) {
	s, clk, _ := seedRichSim(t)
	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restored, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if err := restored.AddLabelToIssue("acme", "widgets", 7, "only-in-restored"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	if err := s.AddLabelToIssue("acme", "widgets", 7, "only-in-original"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	origLabels, err := s.FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	restLabels, err := restored.FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if contains(origLabels, "only-in-restored") {
		t.Error("a mutation on the restored Sim was visible on the original")
	}
	if contains(restLabels, "only-in-original") {
		t.Error("a mutation on the original Sim was visible on the restored one")
	}
}

// One snapshot must seed several restores — a scenario may want to branch off
// the same recovered state twice.
func TestASnapshotIsReusable(t *testing.T) {
	s, clk, _ := seedRichSim(t)
	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	first, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	if err := first.AddLabelToIssue("acme", "widgets", 7, "only-in-first"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	second, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	labels, err := second.FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if contains(labels, "only-in-first") {
		t.Error("the second restore saw a mutation made on the first")
	}
}

// TestInstrumentedSnapshotCarriesFaultsAndLog is the instrumentation half of
// R5/D5: the engine died and came back, and GitHub is still broken.
func TestInstrumentedSnapshotCarriesFaultsAndLog(t *testing.T) {
	s, clk, _ := seedRichSim(t)
	in := Instrument(s)

	in.Faults().FailAlways("AddLabelToIssue", ErrRateLimit())
	if err := in.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err == nil {
		t.Fatal("AddLabelToIssue succeeded despite FailAlways")
	}
	if _, err := in.FetchLabels("acme", "widgets", 7); err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	logBefore := in.Log().Len()

	snap, err := in.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Instrumented.Snapshot: %v", err)
	}
	restored, err := RestoreInstrumented(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("RestoreInstrumented: %v", err)
	}

	// The failure persists across the restart.
	if err := restored.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-done"); err == nil {
		t.Error("the fault did not survive the restore — GitHub quietly healed")
	}

	// The log survives, and an ordering assertion can span the restart.
	if got := restored.Log().Len(); got != logBefore+1 {
		t.Errorf("restored log has %d entries, want %d (the %d carried plus the post-restart call)",
			got, logBefore+1, logBefore)
	}
	precedes, err := restored.Log().Precedes(MethodIs("AddLabelToIssue"), MethodIs("FetchLabels"))
	if err != nil {
		t.Fatalf("Precedes across the restart: %v", err)
	}
	if !precedes {
		t.Error("ordering across the restart was lost")
	}

	// Call counters survive too, so FailOnCall's K still counts from the
	// original table's creation.
	if got := restored.Faults().CallCount("AddLabelToIssue"); got != 2 {
		t.Errorf("CallCount after restore = %d, want 2 (one before, one after)", got)
	}
}

// A snapshot taken from a bare Sim carries no instruments. Handing back an
// empty fault table would let a recovery scenario pass by testing a GitHub
// that had silently stopped failing.
func TestRestoreInstrumentedRefusesABareSnapshot(t *testing.T) {
	s, _, _ := seedRichSim(t)
	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := RestoreInstrumented(snap, t.TempDir()); err == nil {
		t.Fatal("RestoreInstrumented accepted a snapshot taken by Sim.Snapshot")
	}
}

// TestSnapshotFieldRegistryIsComplete is the drift detector for the deep copy.
//
// A field added to model.go and not handled by Snapshot/Restore vanishes
// across a restart, where it looks exactly like an engine defect — the model
// forgot something GitHub would have kept. Reflection over the real structs is
// the only way to notice; the registry forces a deliberate decision per field
// rather than an omission.
func TestSnapshotFieldRegistryIsComplete(t *testing.T) {
	for name, typ := range snapshotRegisteredTypes {
		t.Run(name, func(t *testing.T) {
			registry, ok := snapshotFieldRegistry[name]
			if !ok {
				t.Fatalf("type %s has no entry in snapshotFieldRegistry", name)
			}
			seen := make(map[string]bool, typ.NumField())
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i).Name
				seen[field] = true
				if _, ok := registry[field]; !ok {
					t.Errorf("%s.%s is not in snapshotFieldRegistry: decide whether Snapshot copies, "+
						"rebuilds or deliberately drops it, and record that here", name, field)
				}
			}
			for field := range registry {
				if !seen[field] {
					t.Errorf("snapshotFieldRegistry lists %s.%s, which no longer exists", name, field)
				}
			}
		})
	}
	for name := range snapshotFieldRegistry {
		if _, ok := snapshotRegisteredTypes[name]; !ok {
			t.Errorf("snapshotFieldRegistry has an entry for %q with no corresponding type", name)
		}
	}
}

// Every model type reachable from Sim must be registered, or a whole struct
// could be added and deep-copied wrongly with nothing to notice.
func TestEveryModelTypeIsRegistered(t *testing.T) {
	want := []string{"Sim", "repoState", "issueRecord", "prRecord", "commentRecord", "projectState", "itemState"}
	for _, name := range want {
		if _, ok := snapshotRegisteredTypes[name]; !ok {
			t.Errorf("model type %s is not registered for the completeness check", name)
		}
	}
}

// Both directions must be idempotent. Git writes its loose objects read-only
// (0444), so a naive copy fails the moment either destination already holds a
// previous copy — and the failure is a bare "permission denied" on an object
// hash, which reads as a git problem rather than a copy problem.
func TestSnapshotAndRestoreAreIdempotentOverAnExistingDirectory(t *testing.T) {
	s, clk, sha := seedRichSim(t)

	staging := t.TempDir()
	if _, err := s.Snapshot(staging); err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	snap, err := s.Snapshot(staging)
	if err != nil {
		t.Fatalf("second Snapshot into the same staging dir: %v", err)
	}

	target := t.TempDir()
	if _, err := Restore(snap, target, WithClock(clk)); err != nil {
		t.Fatalf("first Restore: %v", err)
	}
	restored, err := Restore(snap, target, WithClock(clk))
	if err != nil {
		t.Fatalf("second Restore into the same baseDir: %v", err)
	}

	// And the twice-copied repository still answers a git-derived question.
	if _, err := restored.FetchCheckRuns("acme", "widgets", sha); err != nil {
		t.Fatalf("FetchCheckRuns after the second restore: %v", err)
	}
	if _, err := restored.FetchCommitsBehind("acme", "widgets", "main", "fabrik/issue-7"); err != nil {
		t.Fatalf("FetchCommitsBehind after the second restore: %v", err)
	}
}

func TestSnapshotRejectsAnEmptyStagingDir(t *testing.T) {
	s, _, _ := seedRichSim(t)
	if _, err := s.Snapshot(""); err == nil {
		t.Fatal("Snapshot accepted an empty staging directory")
	}
}

// copyDir must refuse anything that is not a regular file or a directory
// rather than skipping it: a silent skip corrupts the restore in a way that
// surfaces much later as an inexplicable git failure.
func TestCopyDirRefusesNonRegularFiles(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink(filepath.Join(src, "nothing"), filepath.Join(src, "link")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	err := copyDir(src, filepath.Join(t.TempDir(), "dst"))
	if err == nil {
		t.Fatal("copyDir silently accepted a symlink")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("error %q does not explain what it refused", err)
	}
}
