package simgh

import (
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// seedPRForScheduling builds a repo with a PR whose head and base both exist,
// so every git-derived answer about it is real, and returns the head SHA.
func seedPRForScheduling(t *testing.T) (*Sim, *fakeClock, string) {
	t.Helper()
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"}).
		SeedIssue("acme/widgets", IssueSeed{Number: 7, Title: "Add a thing", Status: "Implement"}).
		SeedCommit("acme/widgets", "fabrik/issue-7", map[string]string{"a.txt": "a"}, "work").
		SeedPR("acme/widgets", PRSeed{Number: 8, Title: "PR", Head: "fabrik/issue-7", IssueNumber: 7})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return s, clk, mustHeadSHA(t, s, "acme/widgets", "fabrik/issue-7")
}

// TestScriptedCheckRunsAreKeyedBySHA is AC1: two SHAs carry two different
// verdicts within one scenario.
//
// Per-SHA keying is what the merge train's bisection path consumes — it calls
// FetchCheckRuns(owner, repo, sha) against each trial-branch head — so a
// scenario declares a poison set and lets the real algorithm discover it.
func TestScriptedCheckRunsAreKeyedBySHA(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCheckRun("acme/widgets", "sha-good", gh.CheckRun{Name: "build", Conclusion: "success"}).
		SeedCheckRun("acme/widgets", "sha-bad", gh.CheckRun{Name: "build", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, tc := range []struct{ sha, want string }{
		{"sha-good", "success"},
		{"sha-bad", "failure"},
	} {
		runs, err := s.FetchCheckRuns("acme", "widgets", tc.sha)
		if err != nil {
			t.Fatalf("FetchCheckRuns(%s): %v", tc.sha, err)
		}
		if len(runs) != 1 {
			t.Fatalf("FetchCheckRuns(%s) returned %d runs, want 1", tc.sha, len(runs))
		}
		if runs[0].Conclusion != tc.want {
			t.Errorf("FetchCheckRuns(%s) conclusion = %q, want %q", tc.sha, runs[0].Conclusion, tc.want)
		}
	}

	// An untouched SHA reports zero runs, not an error — GitHub's own answer
	// for a commit no CI has reached.
	runs, err := s.FetchCheckRuns("acme", "widgets", "sha-unknown")
	if err != nil {
		t.Fatalf("FetchCheckRuns(unknown): %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("an unseeded SHA reported %d runs, want 0", len(runs))
	}
}

// TestScheduledCheckRunTransitionIsClockDriven is AC2.
//
// Three claims: the surface reports pending before the scheduled instant,
// failure at and after it, and — the part read-count sequencing could not
// deliver — the answer is *stable across repeated reads at a single clock
// instant*. One poll is not one read: settleAwaitingCIScan's
// RefreshCheckRunsLive and checkCIGate each read the same SHA within one poll,
// so a sequence that advanced per read would not correspond to poll boundaries
// at all.
func TestScheduledCheckRunTransitionIsClockDriven(t *testing.T) {
	s, clk, sha := seedPRForScheduling(t)

	// The same check run, by ID, transitioning from in_progress to failure.
	s.SeedCheckRun("acme/widgets", sha, gh.CheckRun{ID: 900, Name: "build", Status: "in_progress"}).
		SeedCheckRunsAfter("acme/widgets", sha, 30*time.Minute,
			gh.CheckRun{ID: 900, Name: "build", Status: "completed", Conclusion: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	verdict := func() string {
		t.Helper()
		runs, err := s.FetchCheckRuns("acme", "widgets", sha)
		if err != nil {
			t.Fatalf("FetchCheckRuns: %v", err)
		}
		if len(runs) != 1 {
			t.Fatalf("got %d runs, want 1 — the step should supersede by ID, not append", len(runs))
		}
		return checkVerdict(runs[0])
	}

	// Repeated reads before the instant: still pending, however many.
	for i := 0; i < 5; i++ {
		if got := verdict(); got != "pending" {
			t.Fatalf("read %d before the scheduled instant = %q, want pending", i+1, got)
		}
	}

	clk.Advance(29 * time.Minute)
	if got := verdict(); got != "pending" {
		t.Fatalf("one minute short of the scheduled instant = %q, want pending", got)
	}

	clk.Advance(time.Minute)
	// Repeated reads at and after the instant: consistently failure. A step
	// that applied per read rather than per instant would flap here.
	for i := 0; i < 5; i++ {
		if got := verdict(); got != "failure" {
			t.Fatalf("read %d at the scheduled instant = %q, want failure", i+1, got)
		}
	}
	clk.Advance(24 * time.Hour)
	if got := verdict(); got != "failure" {
		t.Fatalf("long after the scheduled instant = %q, want failure", got)
	}
}

// A new ID appends rather than superseding — the rerun shape, which
// production's latestCheckRunsByName reduces by keeping the highest ID.
func TestScheduledCheckRunWithANewIDModelsARerun(t *testing.T) {
	s, clk, sha := seedPRForScheduling(t)
	s.SeedCheckRun("acme/widgets", sha, gh.CheckRun{ID: 900, Name: "build", Conclusion: "failure"}).
		SeedCheckRunsAfter("acme/widgets", sha, time.Hour,
			gh.CheckRun{ID: 901, Name: "build", Conclusion: "success"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	clk.Advance(time.Hour)
	runs, err := s.FetchCheckRuns("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2 — a new ID is a rerun, not a transition", len(runs))
	}
	// The reduction production performs keeps the newest run, so the PR is
	// clean rather than blocked.
	state, err := s.FetchPRMergeableState("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRMergeableState: %v", err)
	}
	if state != "clean" {
		t.Errorf("mergeable state = %q after the passing rerun, want clean", state)
	}
}

// TestScheduledCommitStatusIsClockDriven covers the classic Statuses API half
// of the CI surface, kept genuinely separate from check runs because
// production distinguishes them (ADR-933).
func TestScheduledCommitStatusIsClockDriven(t *testing.T) {
	s, clk, sha := seedPRForScheduling(t)
	s.SeedCommitStatus("acme/widgets", sha, gh.CommitStatus{Context: "legacy-ci", State: "pending"}).
		SeedCommitStatusesAfter("acme/widgets", sha, 15*time.Minute,
			gh.CommitStatus{Context: "legacy-ci", State: "failure"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	latest := func() string {
		t.Helper()
		sts, err := s.FetchCombinedStatus("acme", "widgets", sha)
		if err != nil {
			t.Fatalf("FetchCombinedStatus: %v", err)
		}
		if len(sts) == 0 {
			t.Fatal("no statuses")
		}
		return statusVerdict(sts[len(sts)-1])
	}

	if got := latest(); got != "pending" {
		t.Fatalf("before the scheduled instant = %q, want pending", got)
	}
	clk.Advance(15 * time.Minute)
	if got := latest(); got != "failure" {
		t.Fatalf("at the scheduled instant = %q, want failure", got)
	}
}

// TestEveryReviewStateIsSettableAndReadBack is AC3: each state the review gate
// distinguishes round-trips.
//
// The requested-but-unresponded case is the load-bearing one — it is what
// keeps fabrik:awaiting-review applied — and it lives on a different surface
// (FetchPRReviewRequests) from the four verdicts, so it is asserted there.
func TestEveryReviewStateIsSettableAndReadBack(t *testing.T) {
	s, _, _ := seedPRForScheduling(t)
	for _, state := range []string{"APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED"} {
		s.SeedReview("acme/widgets", 8, gh.PRReview{Author: "reviewer-" + state, State: state})
	}
	s.SeedReviewRequest("acme/widgets", 8, gh.ReviewRequest{Login: "unresponded-human"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	reviews, err := s.FetchPRReviews("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	got := make(map[string]string, len(reviews))
	for _, rev := range reviews {
		got[rev.Author] = rev.State
	}
	for _, state := range []string{"APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED"} {
		if got["reviewer-"+state] != state {
			t.Errorf("review state %s read back as %q", state, got["reviewer-"+state])
		}
	}

	reqs, err := s.FetchPRReviewRequests("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Login != "unresponded-human" {
		t.Errorf("FetchPRReviewRequests = %+v, want the one outstanding human", reqs)
	}
}

// TestScheduledReviewArrivesOnTheClock covers "no review, then a review at T"
// — the shape the review gate's bot re-prompt ladder and its timeout depend
// on. The derived reviewDecision must move with it, since it shares
// latestReviewsByAuthor with FetchPRReviews and the two cannot be allowed to
// disagree.
func TestScheduledReviewArrivesOnTheClock(t *testing.T) {
	s, clk, _ := seedPRForScheduling(t)
	s.SeedRequiredApprovals("acme/widgets", "main", 1).
		SeedReviewsAfter("acme/widgets", 8, 40*time.Minute,
			gh.PRReview{Author: "human", State: "APPROVED"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	reviews, err := s.FetchPRReviews("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("got %d reviews before the scheduled instant, want 0", len(reviews))
	}
	decision, err := s.FetchPRReviewDecision("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviewDecision: %v", err)
	}
	if decision != "REVIEW_REQUIRED" {
		t.Errorf("decision before the review = %q, want REVIEW_REQUIRED", decision)
	}

	clk.Advance(40 * time.Minute)

	reviews, err = s.FetchPRReviews("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].State != "APPROVED" {
		t.Fatalf("got %+v after the scheduled instant, want one APPROVED review", reviews)
	}
	// SubmittedAt is the step's instant, not whenever the read happened to
	// drain it.
	if want := clk.Now(); !reviews[0].SubmittedAt.Equal(want) {
		t.Errorf("SubmittedAt = %v, want the scheduled instant %v", reviews[0].SubmittedAt, want)
	}
	decision, err = s.FetchPRReviewDecision("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviewDecision: %v", err)
	}
	if decision != "APPROVED" {
		t.Errorf("decision after the review = %q, want APPROVED", decision)
	}
}

// A landed step must not mask later engine mutations to the same surface.
//
// AddReviewRequest is the bot re-prompt ladder's own mutation (ADR-1283): had
// schedules been implemented as read filters, the engine would make the call,
// the model would accept it, and the next read would deny it ever happened.
func TestEngineMutationsRemainObservableAfterAStepLands(t *testing.T) {
	s, clk, _ := seedPRForScheduling(t)
	s.SeedReviewRequestsAfter("acme/widgets", 8, 10*time.Minute,
		gh.ReviewRequest{Login: "scheduled-human"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	clk.Advance(10 * time.Minute)
	if _, err := s.FetchPRReviewRequests("acme", "widgets", 8); err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}

	if err := s.AddReviewRequest("acme", "widgets", 8, []string{"handarbeit-pruefer"}); err != nil {
		t.Fatalf("AddReviewRequest: %v", err)
	}
	reqs, err := s.FetchPRReviewRequests("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want both the scheduled one and the engine's: %+v", len(reqs), reqs)
	}

	// And a withdrawal must stick.
	if err := s.DeleteReviewRequest("acme", "widgets", 8, []string{"scheduled-human"}); err != nil {
		t.Fatalf("DeleteReviewRequest: %v", err)
	}
	reqs, err = s.FetchPRReviewRequests("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Login != "handarbeit-pruefer" {
		t.Errorf("after withdrawing the scheduled reviewer: %+v, want only the engine's", reqs)
	}
}

// A step that is already due when a mutator runs must land *before* it, not
// after. Without the drain inside AddReviewRequest/DeleteReviewRequest, a
// withdrawal would be silently undone by a step whose time had already come.
func TestADueStepCannotUndoAnEngineWithdrawal(t *testing.T) {
	s, clk, _ := seedPRForScheduling(t)
	s.SeedReviewRequestsAfter("acme/widgets", 8, 10*time.Minute,
		gh.ReviewRequest{Login: "scheduled-human"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Advance past the step's instant but never read the surface, so the step
	// is due and undrained when the withdrawal arrives.
	clk.Advance(10 * time.Minute)
	if err := s.DeleteReviewRequest("acme", "widgets", 8, []string{"scheduled-human"}); err != nil {
		t.Fatalf("DeleteReviewRequest: %v", err)
	}

	reqs, err := s.FetchPRReviewRequests("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("FetchPRReviewRequests: %v", err)
	}
	if len(reqs) != 0 {
		t.Errorf("got %+v, want none — the due step landed after the withdrawal instead of before it", reqs)
	}
}

// TestScheduleVisibleThroughEveryReadPath is the guard against the bug this
// design is most exposed to: a missed drain site making one read path disagree
// with another about the same model. reviews.go already warns about that class
// one layer up ("two reads of one model reporting two verdicts is a bug the
// sim would be introducing"); a schedule multiplies the number of places it
// can happen.
func TestScheduleVisibleThroughEveryReadPath(t *testing.T) {
	t.Run("CI paths", func(t *testing.T) {
		s, clk, sha := seedPRForScheduling(t)
		s.SeedRequiredContexts("acme/widgets", "main", []string{"build"}).
			SeedCheckRun("acme/widgets", sha, gh.CheckRun{ID: 900, Name: "build", Conclusion: "success"}).
			SeedCheckRunsAfter("acme/widgets", sha, time.Hour,
				gh.CheckRun{ID: 900, Name: "build", Status: "completed", Conclusion: "failure"})
		if err := s.Err(); err != nil {
			t.Fatalf("seeding: %v", err)
		}

		clk.Advance(time.Hour)

		// FetchCheckRuns — the direct read.
		runs, err := s.FetchCheckRuns("acme", "widgets", sha)
		if err != nil {
			t.Fatalf("FetchCheckRuns: %v", err)
		}
		if len(runs) != 1 || checkVerdict(runs[0]) != "failure" {
			t.Errorf("FetchCheckRuns = %+v, want one failing run", runs)
		}

		// deriveMergeableState via contextsForSHA — the derived read.
		state, err := s.FetchPRMergeableState("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("FetchPRMergeableState: %v", err)
		}
		if state != "blocked" {
			t.Errorf("mergeable state = %q, want blocked — the derivation did not see the step", state)
		}

		// FetchPRDetails reaches the same derivation through a different entry.
		details, err := s.FetchPRDetails("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("FetchPRDetails: %v", err)
		}
		if details.MergeableState != "blocked" {
			t.Errorf("FetchPRDetails MergeableState = %q, want blocked", details.MergeableState)
		}
	})

	t.Run("review paths", func(t *testing.T) {
		s, clk, _ := seedPRForScheduling(t)
		s.SeedRequiredApprovals("acme/widgets", "main", 1).
			SeedReviewsAfter("acme/widgets", 8, time.Hour, gh.PRReview{Author: "human", State: "CHANGES_REQUESTED"}).
			SeedReviewRequestsAfter("acme/widgets", 8, time.Hour, gh.ReviewRequest{Login: "second-human"})
		if err := s.Err(); err != nil {
			t.Fatalf("seeding: %v", err)
		}

		clk.Advance(time.Hour)

		reviews, err := s.FetchPRReviews("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("FetchPRReviews: %v", err)
		}
		if len(reviews) != 1 {
			t.Errorf("FetchPRReviews returned %d reviews, want 1", len(reviews))
		}

		decision, err := s.FetchPRReviewDecision("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("FetchPRReviewDecision: %v", err)
		}
		if decision != "CHANGES_REQUESTED" {
			t.Errorf("FetchPRReviewDecision = %q, want CHANGES_REQUESTED", decision)
		}

		reqs, err := s.FetchPRReviewRequests("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("FetchPRReviewRequests: %v", err)
		}
		if len(reqs) != 1 {
			t.Errorf("FetchPRReviewRequests returned %d, want 1", len(reqs))
		}

		// The board projection — the path the engine actually consumes most.
		item := firstItemFull(t, s)
		if len(item.LinkedPRReviews) != 1 {
			t.Errorf("board projection LinkedPRReviews = %+v, want the scheduled review", item.LinkedPRReviews)
		}
		if len(item.LinkedPRReviewRequests) != 1 {
			t.Errorf("board projection LinkedPRReviewRequests = %+v, want the scheduled request", item.LinkedPRReviewRequests)
		}

		// The probe path reads only updatedAt, but a due step bumps exactly
		// that — and EffectiveUpdatedAt is how the engine decides an item is
		// worth deep-fetching.
		probes, _, err := s.ProbeProjectBoard("acme", "widgets", 2, "organization")
		if err != nil {
			t.Fatalf("ProbeProjectBoard: %v", err)
		}
		if len(probes) != 1 {
			t.Fatalf("probe returned %d items, want 1", len(probes))
		}
		if probes[0].EffectiveUpdatedAt.Before(clk.Now()) {
			t.Errorf("probe EffectiveUpdatedAt = %v, want it bumped to the step's instant %v",
				probes[0].EffectiveUpdatedAt, clk.Now())
		}
	})
}

// Steps scheduled for the same instant apply in seeding order — the only
// ordering a scenario author can reason about — and a batch of steps at
// different instants applies in time order regardless of seeding order.
func TestStepsApplyInTimeThenSeedingOrder(t *testing.T) {
	s, clk, sha := seedPRForScheduling(t)
	// Seeded out of time order deliberately.
	s.SeedCheckRunsAfter("acme/widgets", sha, 2*time.Hour, gh.CheckRun{ID: 3, Name: "c", Conclusion: "success"}).
		SeedCheckRunsAfter("acme/widgets", sha, time.Hour, gh.CheckRun{ID: 1, Name: "a", Conclusion: "success"}).
		SeedCheckRunsAfter("acme/widgets", sha, time.Hour, gh.CheckRun{ID: 2, Name: "b", Conclusion: "success"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	clk.Advance(2 * time.Hour)
	runs, err := s.FetchCheckRuns("acme", "widgets", sha)
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	var order []string
	for _, run := range runs {
		order = append(order, run.Name)
	}
	want := []string{"a", "b", "c"}
	if len(order) != len(want) {
		t.Fatalf("got %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("steps applied in order %v, want %v", order, want)
		}
	}
}

// An empty step could never be observed, so a scenario built on one would pass
// without its sequence ever having existed.
func TestAnEmptyScheduledStepIsASeedingError(t *testing.T) {
	cases := []struct {
		name string
		seed func(*Sim)
	}{
		{"SeedCheckRunsAt", func(s *Sim) { s.SeedCheckRunsAt("acme/widgets", "sha", time.Now()) }},
		{"SeedCommitStatusesAt", func(s *Sim) { s.SeedCommitStatusesAt("acme/widgets", "sha", time.Now()) }},
		{"SeedReviewsAt", func(s *Sim) { s.SeedReviewsAt("acme/widgets", 8, time.Now()) }},
		{"SeedReviewRequestsAt", func(s *Sim) { s.SeedReviewRequestsAt("acme/widgets", 8, time.Now()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := seedPRForScheduling(t)
			tc.seed(s)
			if err := s.Err(); err == nil {
				t.Fatalf("%s with no values recorded no seeding error", tc.name)
			}
		})
	}
}

// SeedRateLimits is the controllability RateLimitStats needs, since fault
// injection cannot fail a method with no error return.
func TestSeedRateLimits(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRateLimits(5000, 100, 5000, 42)
	if err := s.Err(); err != nil {
		t.Fatalf("SeedRateLimits: %v", err)
	}
	rest, graphql := s.RateLimitStats()
	if rest.Remaining != 100 || rest.Limit != 5000 || rest.Used != 4900 {
		t.Errorf("rest = %+v, want limit 5000 remaining 100 used 4900", rest)
	}
	if graphql.Remaining != 42 || graphql.Used != 4958 {
		t.Errorf("graphql = %+v, want remaining 42 used 4958", graphql)
	}

	// Remaining above the limit is not a budget GitHub could report.
	s2, _ := newSim(t)
	s2.SeedRateLimits(100, 200, 100, 100)
	if err := s2.Err(); err == nil {
		t.Error("SeedRateLimits accepted remaining > limit")
	}
}

// mergeableRecomputeReads is the one surviving read-count sequence, kept
// because it models a genuine GitHub behaviour rather than a test's notion of
// elapsed time. Its reproducibility caveat — only one call site may read the
// surface — is recorded in FIDELITY.md; this pins the mechanism itself.
func TestReadCountSequencingSurvivesForTheRecomputeWindow(t *testing.T) {
	s, _, _ := seedPRForScheduling(t)
	s.SeedMergeableRecomputePending("acme/widgets", 8, 2)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for i := 0; i < 2; i++ {
		mergeable, state, err := s.FetchPRMergeableFields("acme", "widgets", 8)
		if err != nil {
			t.Fatalf("read %d: %v", i+1, err)
		}
		if mergeable != nil || state != "unknown" {
			t.Fatalf("read %d = (%v, %q), want (nil, unknown) inside the recompute window", i+1, mergeable, state)
		}
	}
	mergeable, state, err := s.FetchPRMergeableFields("acme", "widgets", 8)
	if err != nil {
		t.Fatalf("read 3: %v", err)
	}
	if mergeable == nil || !*mergeable || state != "clean" {
		t.Errorf("read 3 = (%v, %q), want the real git-derived answer", mergeable, state)
	}
}
