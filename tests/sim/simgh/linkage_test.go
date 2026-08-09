package simgh

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// The model's identity rules — which PR a branch resolves to, and how issue and
// PR numbers are allocated — are places where a plausible-looking simplification
// diverges from GitHub silently. Both cases below produce a green test against a
// wrong model, so each is pinned here.

// TestFetchLinkedPRPrefersNewestOnReusedBranch pins newest-wins resolution.
//
// Fabrik reuses the branch fabrik/issue-<N>, so a PR closed without merging
// leaves a stale record on the branch its successor reuses. Production queries
// pulls?head=owner:branch&state=all&per_page=1, which defaults to
// created-descending and therefore returns the newest. Resolving to the oldest
// would hand the engine a closed PR it would never have seen against real
// GitHub — and engine/prcreate.go decides whether to create a PR on exactly
// this answer.
func TestFetchLinkedPRPrefersNewestOnReusedBranch(t *testing.T) {
	s, _ := seedBasicBoard(t)
	branch := issueBranch(7)

	s.SeedCommit("acme/widgets", branch, map[string]string{"a.txt": "a"}, "work").
		SeedPR("acme/widgets", PRSeed{Number: 10, Title: "abandoned", Head: branch, State: "closed"}).
		SeedPR("acme/widgets", PRSeed{Number: 11, Title: "live", Head: branch})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	pr, err := s.FetchLinkedPR("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil {
		t.Fatal("FetchLinkedPR returned nil, want the live PR")
	}
	if pr.Number != 11 {
		t.Errorf("FetchLinkedPR = #%d, want #11 (the newest PR on the reused branch)", pr.Number)
	}
	if pr.State != "open" {
		t.Errorf("linked PR state = %q, want %q", pr.State, "open")
	}

	num, err := s.FindPRForIssue("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FindPRForIssue: %v", err)
	}
	if num != 11 {
		t.Errorf("FindPRForIssue = #%d, want #11", num)
	}

	// The board projection must agree. If these two reads disagreed, the same
	// model would report two different linked PRs depending on which call the
	// engine happened to make — a bug the sim itself would be introducing.
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("board has %d items, want 1", len(board.Items))
	}
	if got := board.Items[0].LinkedPRNumber; got != 11 {
		t.Errorf("board LinkedPRNumber = #%d, want #11 (must agree with FetchLinkedPR)", got)
	}
}

// TestIssueAndPRShareNumberSpace pins GitHub's single per-repo number sequence.
//
// GitHub numbers issues and pull requests from one counter, so #7 is either an
// issue or a PR and never both. Two independent sequences would let issue #1 and
// PR #1 coexist — and because GitHub's issue-comment endpoint is likewise shared
// between issues and PRs, AddComment would then resolve a PR comment onto the
// same-numbered issue. The engine posts stage output that way (engine/pr.go),
// so the output would vanish from the PR while a test still saw "a comment was
// posted" and passed.
func TestIssueAndPRShareNumberSpace(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCommit("acme/widgets", "feature", map[string]string{"a.txt": "a"}, "work")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	issueNum, _, err := s.CreateIssue("acme", "widgets", "an issue", "body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	prNum, err := s.CreateDraftPR("acme", "widgets", "a PR", "feature", "main", "body", issueNum)
	if err != nil {
		t.Fatalf("CreateDraftPR: %v", err)
	}
	if prNum == issueNum {
		t.Fatalf("PR and issue were both numbered #%d; GitHub allocates issues and PRs from one shared sequence", prNum)
	}

	// A comment addressed to the PR number must reach the PR, not an issue.
	if _, err := s.AddComment("acme", "widgets", prNum, "stage output"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	iss, err := s.FetchIssue("acme", "widgets", issueNum)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if iss.Comments != 0 {
		t.Errorf("issue #%d has %d comments, want 0 — the comment was addressed to PR #%d", issueNum, iss.Comments, prNum)
	}
}

// TestSeedRejectsNumberHeldByOtherKind pins that the shared sequence is enforced
// against explicitly-seeded numbers too, not just auto-assigned ones.
func TestSeedRejectsNumberHeldByOtherKind(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCommit("acme/widgets", "feature", map[string]string{"a.txt": "a"}, "work").
		SeedIssue("acme/widgets", IssueSeed{Number: 5, Title: "an issue"}).
		SeedPR("acme/widgets", PRSeed{Number: 5, Title: "a PR", Head: "feature"})

	if err := s.Err(); err == nil {
		t.Fatal("seeding PR #5 alongside issue #5 succeeded; want a failure, since GitHub cannot number both 5")
	}
}

// TestSeedProjectItemRejectsPRCard pins that an unmodelled PR card fails loudly
// at seed time.
//
// Board projections resolve a card's content as an issue, so a PR card would be
// omitted from every board read with no error — a scenario would assert against
// a board the model never built and pass for the wrong reason.
func TestSeedProjectItemRejectsPRCard(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Done"}).
		SeedCommit("acme/widgets", "feature", map[string]string{"a.txt": "a"}, "work").
		SeedPR("acme/widgets", PRSeed{Number: 4, Title: "a PR", Head: "feature"}).
		SeedProjectItem("acme", 2, "acme/widgets", 4, true, "Backlog")

	if err := s.Err(); err == nil {
		t.Fatal("seeding a PR card succeeded; want a failure, since board projections cannot represent one")
	}
}

// TestAutoAssignedNumberSkipsSeededHoles pins that auto-assignment steps over a
// number already held by either kind, rather than colliding with it.
func TestAutoAssignedNumberSkipsSeededHoles(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCommit("acme/widgets", "feature", map[string]string{"a.txt": "a"}, "work").
		SeedPR("acme/widgets", PRSeed{Number: 1, Title: "a PR", Head: "feature"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	num, _, err := s.CreateIssue("acme", "widgets", "an issue", "body")
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if num == 1 {
		t.Error("CreateIssue reused #1, which PR #1 already holds")
	}
}

// TestAddProjectV2ItemByIdRejectsPRCard pins that the runtime board API refuses
// a PR card for the same reason the seeding API does.
//
// The two must agree about what the model can represent. Board projections
// resolve a card's content as an issue, so a PR card recorded here would be
// dropped by every subsequent board read — a loud refusal at seed time and a
// silent no-op at runtime is exactly the asymmetry that makes a fake untrustworthy.
func TestAddProjectV2ItemByIdRejectsPRCard(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Done"}).
		SeedCommit("acme/widgets", "feature", map[string]string{"a.txt": "a"}, "work").
		SeedPR("acme/widgets", PRSeed{Number: 4, Title: "a PR", Head: "feature"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	_, projectID, err := s.ProbeProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}

	if _, err := s.AddProjectV2ItemById(projectID, "pr:acme/widgets#4"); err == nil {
		t.Fatal("AddProjectV2ItemById accepted a PR content node ID; want a refusal, since no board read can project one")
	}

	// The refusal must also leave no half-added card behind.
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 0 {
		t.Fatalf("board has %d items after a refused add, want 0", len(board.Items))
	}
}

// TestAddBlockedByIssueRejectsUnknownBlocker pins that a blocker node ID which
// resolves to nothing is refused rather than recorded.
//
// A dangling blocker is worse than a plain no-op: resolveDependenciesLocked
// reports an unresolvable blocker as OPEN, so the dependent issue would read as
// permanently blocked with no diagnostic anywhere.
func TestAddBlockedByIssueRejectsUnknownBlocker(t *testing.T) {
	s, _ := seedBasicBoard(t)

	if err := s.AddBlockedByIssue("issue:acme/widgets#7", "issue:acme/widgets#999"); err == nil {
		t.Fatal("AddBlockedByIssue accepted a blocker that does not exist; want a refusal")
	}
	if err := s.AddBlockedByIssue("issue:acme/widgets#7", "issue:nosuch/repo#1"); err == nil {
		t.Fatal("AddBlockedByIssue accepted a blocker in an unseeded repo; want a refusal")
	}

	item, err := s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if len(item.BlockedBy) != 0 {
		t.Fatalf("issue reads as blocked by %+v after refused links, want no dependency", item.BlockedBy)
	}
}

// TestSeedBlockedByRejectsUnknownBlocker pins the same guard on the seeding
// path, so the two APIs cannot disagree about what a valid blocker is.
func TestSeedBlockedByRejectsUnknownBlocker(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedBlockedBy("acme/widgets", 7, "acme/widgets", 999)

	if err := s.Err(); err == nil {
		t.Fatal("SeedBlockedBy accepted a blocker that does not exist; want a failure")
	}
}

// TestSeedCheckRunReservesExplicitIDs pins that an explicitly-seeded check run
// ID is reserved against later auto-assignment, the way reserveNumber reserves
// an explicitly-seeded issue/PR number.
//
// The collision this prevents is not cosmetic. Production's
// latestCheckRunsByName (github/checkruns.go) reduces same-named runs by
// keeping the highest ID, treating it as the most recent rerun; on a tie it
// keeps whichever it saw first. Two runs sharing an ID therefore let a
// "failed, then the rerun passed" scenario be classified off the failed run.
func TestSeedCheckRunReservesExplicitIDs(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets")

	// 5000 is the auto-assign counter's starting value, so an explicit seed of
	// it collides with the very next ID: 0 seed unless it is reserved.
	s.SeedCheckRun("acme/widgets", "sha1", gh.CheckRun{ID: 5000, Name: "build", Conclusion: "failure"})
	s.SeedCheckRun("acme/widgets", "sha1", gh.CheckRun{Name: "build", Conclusion: "success"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	runs, err := s.FetchCheckRuns("acme", "widgets", "sha1")
	if err != nil {
		t.Fatalf("FetchCheckRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d check runs, want 2", len(runs))
	}
	if runs[0].ID == runs[1].ID {
		t.Fatalf("both check runs share ID %d; an explicit ID was not reserved against auto-assignment", runs[0].ID)
	}

	// The consequence the reservation protects: production must resolve the
	// rerun (the higher ID) as the current verdict for the name.
	if _, _, failed := gh.ClassifyCheckRuns(runs); len(failed) != 0 {
		t.Fatalf("ClassifyCheckRuns resolved the stale failed run as current (%+v); the ID tiebreak was defeated by a collision", failed)
	}
}

// TestSeedCheckRunRejectsDuplicateID pins the loud refusal for the case the
// caller writes one ID twice by hand — unrepresentable, since GitHub's check
// run IDs are unique across the instance.
func TestSeedCheckRunRejectsDuplicateID(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedCheckRun("acme/widgets", "sha1", gh.CheckRun{ID: 77, Name: "build", Conclusion: "success"}).
		SeedCheckRun("acme/widgets", "sha2", gh.CheckRun{ID: 77, Name: "vet", Conclusion: "success"})

	if err := s.Err(); err == nil {
		t.Fatal("SeedCheckRun accepted a duplicate check run ID; want a failure")
	}
}

// TestSeedRepoRejectsPathTraversal pins that owner/repo cannot escape baseDir.
// repoDir joins the segments straight into a directory name, so without this
// guard "../evil/pwned" creates the bare repo as a sibling of the t.TempDir()
// the package promises to keep everything inside.
func TestSeedRepoRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{"../evil/pwned", "acme/../../pwned", "..//pwned"} {
		s, _ := newSim(t)
		s.SeedRepo(name)
		if err := s.Err(); err == nil {
			t.Fatalf("SeedRepo(%q) was accepted; want a refusal", name)
		}
	}

	// An ordinary name still works — the guard must not reject anything valid.
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets")
	if err := s.Err(); err != nil {
		t.Fatalf("SeedRepo rejected a valid owner/repo: %v", err)
	}
}

// TestSeedRepoRefusesDuplicateAfterInit pins that the second SeedRepo for one
// key fails rather than clobbering the live repoState. Clobbering would swap
// out the repo's gitMu while callers still hold the old one, leaving two
// mutexes guarding one bare directory.
func TestSeedRepoRefusesDuplicateSeed(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme/widgets")
	if err := s.Err(); err != nil {
		t.Fatalf("first SeedRepo: %v", err)
	}
	first, err := s.repoByKey("acme/widgets")
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}

	s.SeedRepo("acme/widgets")
	if err := s.Err(); err == nil {
		t.Fatal("second SeedRepo for the same key was accepted; want a failure")
	}
	second, err := s.repoByKey("acme/widgets")
	if err != nil {
		t.Fatalf("repoByKey after refused reseed: %v", err)
	}
	if first != second {
		t.Fatal("the refused reseed replaced the live repoState; its gitMu no longer guards the bare directory callers are using")
	}
}
