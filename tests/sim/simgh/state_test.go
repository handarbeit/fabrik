package simgh

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// TestSatisfiesGitHubClient pins Sim to the engine's interface at runtime as
// well as at compile time, so the assertion is visible in test output rather
// than only as a build failure.
func TestSatisfiesGitHubClient(t *testing.T) {
	var client engine.GitHubClient = New(t.TempDir())
	if client == nil {
		t.Fatal("Sim does not satisfy engine.GitHubClient")
	}
}

// The tests below are the statefulness proofs: for each mutation, a read
// afterwards must observe it. This is the property engine/mocks_test.go's
// per-call hooks cannot express, and the whole reason this layer exists.

func TestLabelAddRemoveIsObservable(t *testing.T) {
	s, clk := seedBasicBoard(t)

	if err := s.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-ci"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}

	labels, err := s.FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels: %v", err)
	}
	if !contains(labels, "fabrik:awaiting-ci") {
		t.Fatalf("after add, labels = %v, want to contain fabrik:awaiting-ci", labels)
	}

	// The board projection must see it too — a mutation is observable by
	// *every* subsequent read, not just the one that mirrors it.
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if !contains(board.Items[0].Labels, "fabrik:awaiting-ci") {
		t.Fatalf("board item labels = %v, want to contain fabrik:awaiting-ci", board.Items[0].Labels)
	}

	// Applied-at is stamped from the injected clock, and re-adding an
	// already-present label must not reset it: three engine mechanisms anchor
	// timeouts on this instant.
	appliedAt, err := s.FetchLabelAppliedAt("acme", "widgets", 7, "fabrik:awaiting-ci")
	if err != nil {
		t.Fatalf("FetchLabelAppliedAt: %v", err)
	}
	if !appliedAt.Equal(clk.Now()) {
		t.Fatalf("appliedAt = %v, want clock time %v", appliedAt, clk.Now())
	}
	clk.Advance(time.Hour)
	if err := s.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-ci"); err != nil {
		t.Fatalf("re-AddLabelToIssue: %v", err)
	}
	again, err := s.FetchLabelAppliedAt("acme", "widgets", 7, "fabrik:awaiting-ci")
	if err != nil {
		t.Fatalf("FetchLabelAppliedAt after re-add: %v", err)
	}
	if !again.Equal(appliedAt) {
		t.Fatalf("re-adding a present label moved appliedAt from %v to %v", appliedAt, again)
	}

	if err := s.RemoveLabelFromIssue("acme", "widgets", 7, "fabrik:awaiting-ci"); err != nil {
		t.Fatalf("RemoveLabelFromIssue: %v", err)
	}
	labels, err = s.FetchLabels("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLabels after remove: %v", err)
	}
	if contains(labels, "fabrik:awaiting-ci") {
		t.Fatalf("after remove, labels = %v, want fabrik:awaiting-ci gone", labels)
	}
}

func TestCommentAddAndReactionAreObservable(t *testing.T) {
	s, _ := seedBasicBoard(t)

	id, err := s.AddComment("acme", "widgets", 7, "stage output")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if id == 0 {
		t.Fatal("AddComment returned database ID 0")
	}

	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	comments := board.Items[0].Comments
	if len(comments) != 1 || comments[0].Body != "stage output" {
		t.Fatalf("comments = %+v, want one comment with body %q", comments, "stage output")
	}
	if comments[0].HasReaction("ROCKET") {
		t.Fatal("fresh comment already has a ROCKET reaction")
	}

	// The rocket reaction is the engine's durable "already processed" state,
	// so it must survive into every later read.
	if err := s.AddCommentReaction("acme", "widgets", id, "ROCKET"); err != nil {
		t.Fatalf("AddCommentReaction: %v", err)
	}
	board, err = s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard after reaction: %v", err)
	}
	if !board.Items[0].Comments[0].HasReaction("ROCKET") {
		t.Fatalf("reaction not observable: %+v", board.Items[0].Comments[0].Reactions)
	}

	if err := s.UpdateComment("acme", "widgets", id, "edited output"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
	board, err = s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard after edit: %v", err)
	}
	if got := board.Items[0].Comments[0].Body; got != "edited output" {
		t.Fatalf("comment body = %q, want %q", got, "edited output")
	}
}

func TestIssueBodyUpdateIsObservable(t *testing.T) {
	s, _ := seedBasicBoard(t)

	body, err := s.GetIssueBody("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("GetIssueBody: %v", err)
	}
	if body != "spec body" {
		t.Fatalf("seeded body = %q, want %q", body, "spec body")
	}

	if err := s.UpdateIssueBody("acme", "widgets", 7, "rewritten spec"); err != nil {
		t.Fatalf("UpdateIssueBody: %v", err)
	}

	body, err = s.GetIssueBody("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("GetIssueBody after update: %v", err)
	}
	if body != "rewritten spec" {
		t.Fatalf("body = %q, want %q", body, "rewritten spec")
	}

	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if board.Items[0].Body != "rewritten spec" {
		t.Fatalf("board item body = %q, want %q", board.Items[0].Body, "rewritten spec")
	}
}

func TestProjectStatusUpdateIsObservable(t *testing.T) {
	s, _ := seedBasicBoard(t)

	item := firstItem(t, s)
	if item.status != "Implement" {
		t.Fatalf("seeded status = %q, want Implement", item.status)
	}

	field, err := s.FetchStatusField(item.projectID)
	if err != nil {
		t.Fatalf("FetchStatusField: %v", err)
	}
	if len(field.OrderedOptionNames) != 4 || field.OrderedOptionNames[0] != "Backlog" {
		t.Fatalf("ordered options = %v, want Backlog first of four", field.OrderedOptionNames)
	}

	if err := s.UpdateProjectItemStatus(item.projectID, item.itemID, field.FieldID, field.Options["Review"]); err != nil {
		t.Fatalf("UpdateProjectItemStatus: %v", err)
	}

	if got := firstItem(t, s).status; got != "Review" {
		t.Fatalf("board status = %q, want Review", got)
	}
	status, err := s.FetchProjectItemStatus(item.itemID)
	if err != nil {
		t.Fatalf("FetchProjectItemStatus: %v", err)
	}
	if status != "Review" {
		t.Fatalf("FetchProjectItemStatus = %q, want Review", status)
	}
	batch, err := s.FetchProjectItemStatusBatch(item.projectID)
	if err != nil {
		t.Fatalf("FetchProjectItemStatusBatch: %v", err)
	}
	if batch[item.itemID] != "Review" {
		t.Fatalf("batch[%s] = %q, want Review", item.itemID, batch[item.itemID])
	}

	// A foreign option ID must be refused rather than silently accepted —
	// otherwise a test could "move" a card to a column that does not exist.
	if err := s.UpdateProjectItemStatus(item.projectID, item.itemID, field.FieldID, "opt:bogus"); err == nil {
		t.Fatal("UpdateProjectItemStatus accepted an unknown option ID")
	}
}

func TestPRCreateReadyMergeIsObservable(t *testing.T) {
	s, _ := seedBasicBoard(t)
	const repo = "acme/widgets"

	s.SeedCommit(repo, "main", map[string]string{"README.md": "hello\n"}, "seed main").
		SeedCommit(repo, "fabrik/issue-7", map[string]string{"feature.go": "package feature\n"}, "feature work")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding branches: %v", err)
	}

	num, err := s.CreateDraftPR("acme", "widgets", "Add a thing", "fabrik/issue-7", "main", "Closes #7", 7)
	if err != nil {
		t.Fatalf("CreateDraftPR: %v", err)
	}

	// Linkage is discovered by head branch, exactly as production does it.
	found, err := s.FindPRForIssue("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FindPRForIssue: %v", err)
	}
	if found != num {
		t.Fatalf("FindPRForIssue = %d, want %d", found, num)
	}

	pr, err := s.FetchLinkedPR("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLinkedPR: %v", err)
	}
	if pr == nil || !pr.Draft {
		t.Fatalf("linked PR = %+v, want a draft", pr)
	}
	if pr.HeadSHA != mustHeadSHA(t, s, repo, "fabrik/issue-7") {
		t.Fatalf("linked PR head SHA = %q, want the branch tip", pr.HeadSHA)
	}
	// The list endpoint production reaches this through omits mergeable_state;
	// reporting a computed value here would let a test pass on a signal
	// production never receives.
	if pr.MergeableState != "" {
		t.Fatalf("FetchLinkedPR MergeableState = %q, want empty (list endpoint omits it)", pr.MergeableState)
	}

	if err := s.MarkPRReady("acme", "widgets", num); err != nil {
		t.Fatalf("MarkPRReady: %v", err)
	}
	pr, err = s.FetchLinkedPR("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchLinkedPR after ready: %v", err)
	}
	if pr.Draft {
		t.Fatal("PR still reports draft after MarkPRReady")
	}

	merged, err := s.FetchPRMerged("acme", "widgets", num)
	if err != nil {
		t.Fatalf("FetchPRMerged: %v", err)
	}
	if merged {
		t.Fatal("PR reports merged before MergePR")
	}

	if err := s.MergePR("acme", "widgets", num); err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	merged, err = s.FetchPRMerged("acme", "widgets", num)
	if err != nil {
		t.Fatalf("FetchPRMerged after merge: %v", err)
	}
	if !merged {
		t.Fatal("PR does not report merged after MergePR")
	}

	// GitHub auto-closes an issue a merged PR closes, when the merge lands on
	// the default branch. The engine depends on that behaviour.
	issue, err := s.FetchIssue("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.State != "closed" {
		t.Fatalf("issue state = %q, want closed via the merged PR's Closes #7", issue.State)
	}
}

func TestCloseIssueIsObservable(t *testing.T) {
	s, _ := seedBasicBoard(t)

	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if board.Items[0].IsClosed {
		t.Fatal("seeded issue already reports closed")
	}

	if err := s.CloseIssue("acme", "widgets", 7); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	board, err = s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard after close: %v", err)
	}
	if !board.Items[0].IsClosed {
		t.Fatal("board item does not report closed after CloseIssue")
	}
	issue, err := s.FetchIssue("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.State != "closed" {
		t.Fatalf("issue state = %q, want closed", issue.State)
	}
}

// TestBlockedByResolvesBlockerStateLive proves dependencies are resolved on
// read rather than frozen at seed time — closing a blocker must unblock the
// dependent issue on the next read, which is what the engine's dependency gate
// polls for.
func TestBlockedByResolvesBlockerStateLive(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedIssue("acme/widgets", IssueSeed{Number: 8, Title: "blocker", Status: "Implement"}).
		SeedBlockedBy("acme/widgets", 7, "acme/widgets", 8)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	item, err := s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if len(item.BlockedBy) != 1 || item.BlockedBy[0].State != "OPEN" {
		t.Fatalf("blockedBy = %+v, want one OPEN dependency", item.BlockedBy)
	}

	if err := s.CloseIssue("acme", "widgets", 8); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}

	item, err = s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem after close: %v", err)
	}
	if item.BlockedBy[0].State != "CLOSED" {
		t.Fatalf("blocker state = %q, want CLOSED after the blocker was closed", item.BlockedBy[0].State)
	}
}

// TestReviewThreadResolutionIsObservable covers the review-thread half of the
// comment surface: an unresolved thread surfaces on the board item, and
// resolving it both removes it and bumps the resolved count the engine's
// progress detection reads.
func TestReviewThreadResolutionIsObservable(t *testing.T) {
	s, _ := seedBasicBoard(t)
	const repo = "acme/widgets"

	s.SeedCommit(repo, "fabrik/issue-7", map[string]string{"a.go": "package a\n"}, "work").
		SeedPR(repo, PRSeed{Number: 42, Head: "fabrik/issue-7", Title: "Add a thing"}).
		SeedReviewThreadComment(repo, 42, "reviewer", "please fix", "a.go", 1)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	item, err := s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if len(item.LinkedPRReviewThreadComments) != 1 {
		t.Fatalf("thread comments = %d, want 1", len(item.LinkedPRReviewThreadComments))
	}
	threadID := item.LinkedPRReviewThreadComments[0].ReviewThreadID
	if item.LinkedPRResolvedThreadCount != 0 {
		t.Fatalf("resolved count = %d, want 0", item.LinkedPRResolvedThreadCount)
	}

	if err := s.ResolveReviewThread(threadID); err != nil {
		t.Fatalf("ResolveReviewThread: %v", err)
	}

	item, err = s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem after resolve: %v", err)
	}
	if len(item.LinkedPRReviewThreadComments) != 0 {
		t.Fatalf("resolved thread still surfaces as unresolved: %+v", item.LinkedPRReviewThreadComments)
	}
	if item.LinkedPRResolvedThreadCount != 1 {
		t.Fatalf("resolved count = %d, want 1", item.LinkedPRResolvedThreadCount)
	}
}

// TestReviewDecisionEmptyWithoutBranchProtection covers the case the engine's
// authoritative review gate falls back on: GraphQL's reviewDecision is null
// unless branch protection actually requires reviews.
func TestReviewDecisionEmptyWithoutBranchProtection(t *testing.T) {
	s, _ := seedBasicBoard(t)
	const repo = "acme/widgets"

	s.SeedCommit(repo, "fabrik/issue-7", map[string]string{"a.go": "package a\n"}, "work").
		SeedPR(repo, PRSeed{Number: 42, Head: "fabrik/issue-7"}).
		SeedReview(repo, 42, gh.PRReview{Author: "reviewer", State: "CHANGES_REQUESTED", Body: "no"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	decision, err := s.FetchPRReviewDecision("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewDecision: %v", err)
	}
	if decision != "" {
		t.Fatalf("decision = %q, want empty with no branch protection configured", decision)
	}

	// With a requirement configured, the same reviews now produce a decision.
	s.SeedRequiredApprovals(repo, "main", 1)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding approvals: %v", err)
	}
	decision, err = s.FetchPRReviewDecision("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewDecision with protection: %v", err)
	}
	if decision != "CHANGES_REQUESTED" {
		t.Fatalf("decision = %q, want CHANGES_REQUESTED", decision)
	}
}

// TestAssigneesAreObservableFromBothPaths pins that an issue's assignees survive
// to ProjectItem.Assignees whether the issue was seeded or created at runtime.
//
// Both halves matter. CreateIssue grew its assignees parameter when production
// did (engine/spawn.go assigns every spawned child to the configured user), and
// the field is only useful if it reads back — a stored-but-never-projected
// assignee would let a spawn scenario assert on nil and pass for the wrong
// reason. Seeding is covered alongside it because a seed API that cannot express
// what the runtime API accepts is the seed/runtime asymmetry this package has
// had to correct repeatedly.
func TestAssigneesAreObservableFromBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		// place returns the issue number it put on the board.
		place func(t *testing.T, s *Sim) int
	}{
		{
			name: "seeded",
			place: func(t *testing.T, s *Sim) int {
				t.Helper()
				s.SeedIssue(repoName, IssueSeed{
					Number:    11,
					Title:     "seeded",
					Assignees: []string{"alice"},
					Status:    "Implement",
				})
				if err := s.Err(); err != nil {
					t.Fatalf("SeedIssue: %v", err)
				}
				return 11
			},
		},
		{
			name: "created at runtime",
			place: func(t *testing.T, s *Sim) int {
				t.Helper()
				num, nodeID, err := s.CreateIssue("acme", "widgets", "runtime", "body", []string{"alice"})
				if err != nil {
					t.Fatalf("CreateIssue: %v", err)
				}
				board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
				if err != nil {
					t.Fatalf("FetchProjectBoard: %v", err)
				}
				if _, err := s.AddProjectV2ItemById(board.ProjectID, nodeID); err != nil {
					t.Fatalf("AddProjectV2ItemById: %v", err)
				}
				return num
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newSim(t)
			s.SeedRepo(repoName).
				SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"})
			if err := s.Err(); err != nil {
				t.Fatalf("seeding: %v", err)
			}

			num := tc.place(t, s)

			board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
			if err != nil {
				t.Fatalf("FetchProjectBoard: %v", err)
			}
			var found *gh.ProjectItem
			for i := range board.Items {
				if board.Items[i].Number == num {
					found = &board.Items[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("issue #%d is not on the board", num)
			}
			// Assert after the deep fetch, which is the phase that populates
			// assignees in production. The sim currently also fills them on the
			// shallow board read (see FIDELITY.md, "Shallow board reads return
			// deep-phase fields"); going through FetchItemDetails keeps this
			// test correct either way, so narrowing the shallow path later
			// cannot silently turn it vacuous.
			if err := s.FetchItemDetails(found); err != nil {
				t.Fatalf("FetchItemDetails: %v", err)
			}
			if len(found.Assignees) != 1 || found.Assignees[0] != "alice" {
				t.Fatalf("Assignees = %v, want [alice]", found.Assignees)
			}
		})
	}
}

// TestUpdateCommentBumpsParentIssueUpdatedAt pins that editing a comment marks
// the parent issue as changed, the way GitHub's updated_at does.
//
// The engine takes this path for real: it rewrites an existing stage comment
// rather than posting a new one (engine/comments.go, engine/dependencies.go).
// An unbumped timestamp would make that edit invisible to any read that watches
// updatedAt to decide something changed.
func TestUpdateCommentBumpsParentIssueUpdatedAt(t *testing.T) {
	s, clk := seedBasicBoard(t)

	commentID, err := s.AddComment("acme", "widgets", 7, "original")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	before := firstItemFull(t, s).UpdatedAt

	// Advance first, or a bumped timestamp is indistinguishable from a stale one.
	clk.Advance(time.Hour)

	if err := s.UpdateComment("acme", "widgets", commentID, "edited"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}

	item := firstItemFull(t, s)
	if !item.UpdatedAt.After(before) {
		t.Fatalf("issue UpdatedAt = %v, want later than %v after a comment edit", item.UpdatedAt, before)
	}
	// The edit itself must still be observable — a bump alone proves nothing.
	if len(item.Comments) != 1 || item.Comments[0].Body != "edited" {
		t.Fatalf("comment body = %+v, want the edited text", item.Comments)
	}
}

// TestFetchItemDetailsPrefersItemIDAcrossProjects pins that the item ID decides
// which card is backfilled when one issue sits on two boards.
//
// This is a supported arrangement, not a pathological one: Fabrik's own guidance
// puts a report on a public triage board and the engine work on a private board
// at the same time. Item IDs embed the project, content node IDs do not — so
// matching either ID interchangeably let Go's randomised map order pick a board,
// and FetchItemDetails could backfill the other board's Status.
func TestFetchItemDetailsPrefersItemIDAcrossProjects(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo(repoName).
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"}).
		SeedProject("acme", 3, "Triage", []string{"Backlog", "Implement", "Review", "Done"}).
		SeedIssue(repoName, IssueSeed{Number: 7, Title: "on two boards", Status: "Implement"}).
		SeedProjectItem("acme", 3, repoName, 7, false, "Review")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Ask for the Triage board's card specifically, by its item ID.
	triage, err := s.FetchProjectBoard("acme", "widgets", 3, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(triage.Items) != 1 {
		t.Fatalf("triage board has %d items, want 1", len(triage.Items))
	}
	wantItemID := triage.Items[0].ItemID

	// Repeat: the bug this pins is a randomised map order, so a single pass can
	// pick the right board by luck.
	for i := 0; i < 20; i++ {
		probe := &gh.ProjectItem{ID: triage.Items[0].ID, ItemID: wantItemID}
		if err := s.FetchItemDetails(probe); err != nil {
			t.Fatalf("FetchItemDetails: %v", err)
		}
		if probe.ItemID != wantItemID {
			t.Fatalf("run %d: ItemID = %q, want the Triage card %q", i, probe.ItemID, wantItemID)
		}
		if probe.Status != "Review" {
			t.Fatalf("run %d: Status = %q, want Review (the Triage column), not the Engineering board's", i, probe.Status)
		}
	}
}

// TestUpdateCommentBumpsParentPRUpdatedAt is the PR half of the comment-edit
// timestamp rule. It is pinned separately because the two branches are
// separate code paths, and a single test could not show that both work.
//
// The PR's timestamp is observable: buildProjectItem folds a linked PR's
// updatedAt into the board item's UpdatedAt, and ProbeProjectBoard surfaces it
// as LinkedPRUpdatedAt.
func TestUpdateCommentBumpsParentPRUpdatedAt(t *testing.T) {
	s, clk := seedBasicBoard(t)
	s.SeedCommit(repoName, "main", map[string]string{"README.md": "hello\n"}, "seed main").
		SeedCommit(repoName, headBranch, map[string]string{"feature.go": "package feature\n"}, "work").
		SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "a PR"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// #42 is a PR, so this comment lands on the PR, not the issue.
	commentID, err := s.AddComment("acme", "widgets", 42, "original")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	before := firstItemFull(t, s).UpdatedAt

	// Advance first, or a bumped timestamp is indistinguishable from a stale one.
	clk.Advance(time.Hour)

	if err := s.UpdateComment("acme", "widgets", commentID, "edited"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}

	if got := firstItemFull(t, s).UpdatedAt; !got.After(before) {
		t.Fatalf("item UpdatedAt = %v, want later than %v after editing a PR comment", got, before)
	}
}
