package simgh

import "testing"

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
