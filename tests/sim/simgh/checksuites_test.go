package simgh

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// TestCheckSuitesAreKeyedBySHA: a suite seeded on one SHA is invisible on
// another, and an untouched SHA reports zero suites rather than an error — the
// shape GitHub gives for a commit no workflow has reached.
func TestCheckSuitesAreKeyedBySHA(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCheckSuite("acme/widgets", "sha-a", gh.CheckSuite{AppSlug: "github-actions", LatestCheckRunsCount: 3})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	got, err := s.FetchCheckSuites("acme", "widgets", "sha-a")
	if err != nil || len(got) != 1 {
		t.Fatalf("FetchCheckSuites(sha-a) = %+v, %v; want one suite", got, err)
	}
	if got[0].ID == 0 || got[0].Status != "in_progress" || got[0].CreatedAt.IsZero() {
		t.Errorf("defaults not applied: %+v", got[0])
	}
	other, err := s.FetchCheckSuites("acme", "widgets", "sha-b")
	if err != nil || len(other) != 0 {
		t.Errorf("FetchCheckSuites(sha-b) = %+v, %v; want none", other, err)
	}
}

// TestScheduledCheckSuiteTransitionIsClockDriven: the suite is in_progress until
// its scheduled completion instant, and the answer is stable across repeated
// reads at one instant.
func TestScheduledCheckSuiteTransitionIsClockDriven(t *testing.T) {
	s, clk, sha := seedPRForScheduling(t)
	s.SeedCheckSuite("acme/widgets", sha, gh.CheckSuite{ID: 42, AppSlug: "github-actions", LatestCheckRunsCount: 5}).
		SeedCheckSuitesAfter("acme/widgets", sha, time.Hour,
			gh.CheckSuite{ID: 42, AppSlug: "github-actions", Status: "completed", Conclusion: "failure", LatestCheckRunsCount: 6})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	read := func() gh.CheckSuite {
		t.Helper()
		got, err := s.FetchCheckSuites("acme", "widgets", sha)
		if err != nil || len(got) != 1 {
			t.Fatalf("FetchCheckSuites = %+v, %v; want exactly one suite (transition must supersede in place)", got, err)
		}
		return got[0]
	}
	if got := read(); got.Status != "in_progress" {
		t.Fatalf("before the step: status = %q, want in_progress", got.Status)
	}
	clk.Advance(time.Hour)
	for i := 0; i < 3; i++ {
		got := read()
		if got.Status != "completed" || got.Conclusion != "failure" || got.LatestCheckRunsCount != 6 {
			t.Fatalf("read %d after the step = %+v, want completed/failure/6", i, got)
		}
	}
}

// TestScheduledCheckSuiteDefaultsCreatedAtToTheStepInstant: a suite that
// appears an hour from now was created then, not at seed time — otherwise the
// post-push age rule would see a suite older than it is.
func TestScheduledCheckSuiteDefaultsCreatedAtToTheStepInstant(t *testing.T) {
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets")
	at := clk.Now().Add(time.Hour)
	s.SeedCheckSuitesAt("acme/widgets", "sha", at, gh.CheckSuite{AppSlug: "github-actions"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	clk.Advance(2 * time.Hour)
	got, err := s.FetchCheckSuites("acme", "widgets", "sha")
	if err != nil || len(got) != 1 {
		t.Fatalf("FetchCheckSuites = %+v, %v", got, err)
	}
	if !got[0].CreatedAt.Equal(at) {
		t.Errorf("CreatedAt = %v, want the step instant %v", got[0].CreatedAt, at)
	}
}

// TestCheckSuitesDoNotAlterMergeableState pins the fidelity claim: while a job
// is unscheduled GitHub reports "clean" for a green prefix, so an outstanding
// suite must not change the derived state — the engine's suite-awareness is
// exactly the thing that has to close that gap, not the sim.
func TestCheckSuitesDoNotAlterMergeableState(t *testing.T) {
	s, _, sha := seedPRForScheduling(t)
	s.SeedCheckRun("acme/widgets", sha, gh.CheckRun{Name: "build", Conclusion: "success"})
	before, err := s.FetchPRMergeableState("acme", "widgets", 8)
	if err != nil {
		t.Fatal(err)
	}
	s.SeedCheckSuite("acme/widgets", sha, gh.CheckSuite{AppSlug: "github-actions", LatestCheckRunsCount: 1})
	after, err := s.FetchPRMergeableState("acme", "widgets", 8)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("mergeable_state changed from %q to %q when a suite was seeded", before, after)
	}
}

// TestCheckSuitesSurviveSnapshotRestore: both the seeded collection and the
// pending scheduled step round-trip, and the ID counter stays ahead.
func TestCheckSuitesSurviveSnapshotRestore(t *testing.T) {
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCheckSuite("acme/widgets", "sha", gh.CheckSuite{ID: 7, AppSlug: "cursor", Status: "queued"}).
		SeedCheckSuitesAfter("acme/widgets", "sha", time.Hour,
			gh.CheckSuite{ID: 7, AppSlug: "cursor", Status: "completed", Conclusion: "success"})
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
	got, err := restored.FetchCheckSuites("acme", "widgets", "sha")
	if err != nil || len(got) != 1 || got[0].Status != "queued" {
		t.Fatalf("restored suites = %+v, %v; want the queued seed", got, err)
	}
	clk.Advance(time.Hour)
	got, _ = restored.FetchCheckSuites("acme", "widgets", "sha")
	if len(got) != 1 || got[0].Status != "completed" {
		t.Fatalf("restored schedule did not fire: %+v", got)
	}
	restored.SeedCheckSuite("acme/widgets", "sha2", gh.CheckSuite{})
	next, _ := restored.FetchCheckSuites("acme", "widgets", "sha2")
	if len(next) != 1 || next[0].ID <= 7 {
		t.Errorf("auto-assigned ID after restore = %+v, want > 7", next)
	}
}

// TestFetchCheckSuitesDrainsTheSchedule uses a fresh Sim so no other read path
// can have applied the step first — the non-vacuity for the drain in
// FetchCheckSuites.
func TestFetchCheckSuitesDrainsTheSchedule(t *testing.T) {
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCheckSuitesAfter("acme/widgets", "sha", time.Minute, gh.CheckSuite{AppSlug: "github-actions"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	clk.Advance(time.Minute)
	got, err := s.FetchCheckSuites("acme", "widgets", "sha")
	if err != nil || len(got) != 1 {
		t.Fatalf("FetchCheckSuites = %+v, %v; want the scheduled suite", got, err)
	}
}
