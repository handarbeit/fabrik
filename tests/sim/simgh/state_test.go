package simgh

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestSeedProjectItemRefusesUnresolvableTarget pins that a board card must
// point at something that exists.
//
// buildProjectItem resolves a card's content through the repo's issues and
// returns nil when it finds nothing, so a card seeded for a mistyped or
// not-yet-seeded number was recorded and then silently omitted from every
// board read — the scenario believes it built a board it did not. Every
// sibling API in the package (SeedPR, SeedBlockedBy, AddBlockedByIssue,
// AddProjectV2ItemById) already fails loudly on an unresolvable target; this
// one was the gap.
func TestSeedProjectItemRefusesUnresolvableTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ownerRepo string
		number    int
		wantErr   string
	}{
		{"missing issue", "acme/widgets", 999, "no issue acme/widgets#999"},
		{"unseeded repo", "acme/other", 7, "repo acme/other not seeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := seedBasicBoard(t)
			s.SeedProjectItem("acme", 2, tc.ownerRepo, tc.number, false, "Review")

			err := s.Err()
			if err == nil {
				t.Fatal("SeedProjectItem reported success for an unresolvable target; " +
					"the card would be silently absent from every board read")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
			}

			// The refusal must not leave a half-built board behind: the only
			// card is still the one seedBasicBoard placed.
			board, boardErr := s.FetchProjectBoard("acme", "widgets", 2, "")
			if boardErr != nil {
				t.Fatalf("FetchProjectBoard: %v", boardErr)
			}
			if len(board.Items) != 1 {
				t.Fatalf("board has %d items, want 1; the refused card was recorded anyway", len(board.Items))
			}
		})
	}
}

// TestSeedRequiredApprovalsRefusesNonPositive pins that a zero approval
// requirement is refused rather than modelled.
//
// With required = 0 the approvals-satisfied branch of FetchPRReviewDecision is
// reached vacuously, so a PR with no reviews at all reports APPROVED — not a
// reading real GitHub produces. "No review requirement" is already
// representable as the absence of the call, and that absence returns ""
// (GraphQL's null), the case whose engine-side fallback ADR-1250 makes
// load-bearing. The two must not collapse into one silently.
func TestSeedRequiredApprovalsRefusesNonPositive(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	s.SeedRequiredApprovals(repoName, "main", 0)
	err := s.Err()
	if err == nil {
		t.Fatal("SeedRequiredApprovals accepted 0; a PR with no reviews would report APPROVED")
	}
	if !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("error = %v, want a 'must be positive' refusal", err)
	}

	// And the refusal must not have recorded the requirement anyway: an
	// unseeded branch reports "" so the engine's own fallback stays exercised.
	decision, decErr := s.FetchPRReviewDecision("acme", "widgets", 42)
	if decErr != nil {
		t.Fatalf("FetchPRReviewDecision: %v", decErr)
	}
	if decision != "" {
		t.Fatalf("reviewDecision = %q, want \"\"; the refused requirement was recorded anyway", decision)
	}
}

// TestProbeReadsCardStateLive pins that the probe reports the card's state at
// projection time, not at snapshot time — the same property buildProjectItem
// has, in the sibling that feeds the engine's idle poll.
//
// A stale answer matters more here than on a full board read: the probe is
// what the poll consults to decide whether anything changed, so a stale column
// is a poll that does not notice work it should.
func TestProbeReadsCardStateLive(t *testing.T) {
	t.Run("a status move after the snapshot is reported", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		projectID, itemID := mustLookupItem(t, s)

		s.mu.Lock()
		p := s.projects[projectKey("acme", 2)]
		ref := p.liveItemRefs()[0]
		s.mu.Unlock()

		field, err := s.FetchStatusField(projectID)
		if err != nil {
			t.Fatalf("FetchStatusField: %v", err)
		}
		if err := s.UpdateProjectItemStatus(projectID, itemID, field.FieldID, field.Options["Review"]); err != nil {
			t.Fatalf("UpdateProjectItemStatus: %v", err)
		}

		probe, err := s.buildProbeItem(p, ref)
		if err != nil {
			t.Fatalf("buildProbeItem: %v", err)
		}
		if probe == nil {
			t.Fatal("buildProbeItem dropped a live card")
		}
		if probe.Status != "Review" {
			t.Fatalf("probe Status = %q, want %q; the probe used the stale snapshot", probe.Status, "Review")
		}
	})

	t.Run("an archived card is absent", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		projectID, itemID := mustLookupItem(t, s)

		s.mu.Lock()
		p := s.projects[projectKey("acme", 2)]
		ref := p.liveItemRefs()[0]
		s.mu.Unlock()

		if err := s.ArchiveProjectItem(projectID, itemID); err != nil {
			t.Fatalf("ArchiveProjectItem: %v", err)
		}

		probe, err := s.buildProbeItem(p, ref)
		if err != nil {
			t.Fatalf("buildProbeItem: %v", err)
		}
		if probe != nil {
			t.Fatalf("probe still reports an archived card: %+v", probe)
		}
	})
}

// TestFetchPRReviewsCollapsesToLatestPerAuthor pins production's reduction rule.
//
// github.Client.FetchPRReviews reduces the REST endpoint's full submission
// history to one entry per author, and the engine's review-gate call sites
// consume the result assuming that already happened. A raw list would leave a
// superseded CHANGES_REQUESTED visible forever — blocking a gate real GitHub
// would have cleared, and disagreeing with FetchPRReviewDecision on the same PR.
func TestFetchPRReviewsCollapsesToLatestPerAuthor(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"}).
		SeedReview(repoName, 42, gh.PRReview{Author: "carol", State: "CHANGES_REQUESTED", Body: "needs work"}).
		SeedReview(repoName, 42, gh.PRReview{Author: "dave", State: "APPROVED", Body: "lgtm"}).
		SeedReview(repoName, 42, gh.PRReview{Author: "carol", State: "APPROVED", Body: "fixed now"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	reviews, err := s.FetchPRReviews("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("got %d reviews, want 2 (one per author); the history was not collapsed", len(reviews))
	}
	// First-submission order is preserved, so carol stays first.
	if reviews[0].Author != "carol" || reviews[0].State != "APPROVED" {
		t.Fatalf("reviews[0] = %s/%s, want carol/APPROVED; the stale verdict outlived the later one",
			reviews[0].Author, reviews[0].State)
	}
	if reviews[1].Author != "dave" || reviews[1].State != "APPROVED" {
		t.Fatalf("reviews[1] = %s/%s, want dave/APPROVED", reviews[1].Author, reviews[1].State)
	}

	// A COMMENTED follow-up must NOT supersede a formal verdict — GitHub treats
	// it as informational, not a state transition. Pinning this direction too,
	// or "latest wins" would be indistinguishable from "last write wins".
	s.SeedReview(repoName, 42, gh.PRReview{Author: "dave", State: "COMMENTED", Body: "one more thought"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding follow-up: %v", err)
	}
	reviews, err = s.FetchPRReviews("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews after comment: %v", err)
	}
	if reviews[1].State != "APPROVED" {
		t.Fatalf("dave's verdict = %q after a COMMENTED follow-up, want APPROVED", reviews[1].State)
	}

	// The board projection sources the same field from GraphQL's latestReviews,
	// so it must agree. Two reads of one model reporting two answers is a bug
	// the sim would be introducing.
	item, err := s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if len(item.LinkedPRReviews) != len(reviews) {
		t.Fatalf("board projection has %d reviews, FetchPRReviews has %d; the two reads disagree",
			len(item.LinkedPRReviews), len(reviews))
	}
}

// TestRepoDirsDoNotCollide pins that two distinct owner/repo pairs never share
// one backing repository.
//
// Joining owner and repo with a separator is not collision-free:
// "acme-widgets/foo" and "acme/widgets-foo" flatten to the same name. Since the
// already-seeded check is keyed on the full "owner/repo" string, both would be
// accepted and would produce two repoStates — each with its own gitMu — over
// one physical repo, voiding the per-repo git serialisation everything rests on.
func TestRepoDirsDoNotCollide(t *testing.T) {
	s, _ := newSim(t)
	s.SeedRepo("acme-widgets/foo").
		SeedRepo("acme/widgets-foo")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding two distinct repos: %v", err)
	}

	a, err := s.repoByKey("acme-widgets/foo")
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}
	b, err := s.repoByKey("acme/widgets-foo")
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}
	if a.bareDir == b.bareDir {
		t.Fatalf("both repos share the backing directory %s; their gitMus guard one repository", a.bareDir)
	}

	// And the sharing must be observable as such: a commit on one must not
	// appear in the other.
	s.SeedCommit("acme-widgets/foo", "only-in-a", map[string]string{"a.txt": "a"}, "a")
	if err := s.Err(); err != nil {
		t.Fatalf("SeedCommit: %v", err)
	}
	if _, err := s.HeadSHA("acme/widgets-foo", "only-in-a"); err == nil {
		t.Fatal("a branch seeded in one repo is visible in the other; they share a backing repository")
	}
}

// TestSeedPRRefusesImpossibleShapes pins that the seeding API will not build a
// PR state GitHub cannot produce. A scenario driving the engine from an
// impossible state can pass for a reason production never delivers.
func TestSeedPRRefusesImpossibleShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seed    PRSeed
		wantErr string
	}{
		{"merged and draft", PRSeed{Number: 42, Draft: true, Merged: true}, "both merged and draft"},
		{"merged and open", PRSeed{Number: 42, Merged: true, State: "open"}, "merged and open"},
		{"negative number", PRSeed{Number: -3}, "not valid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := seedBasicBoard(t)
			seedCleanDivergence(t, s)
			seed := tc.seed
			seed.Head = headBranch
			seed.Base = "main"
			s.SeedPR(repoName, seed)

			err := s.Err()
			if err == nil {
				t.Fatalf("SeedPR accepted %+v; GitHub cannot produce that shape", tc.seed)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}

	// Merged with State unspecified is the ordinary case and must be accepted,
	// resolving to closed — otherwise the refusals above would be satisfied by
	// a seed API that simply rejects Merged outright.
	t.Run("merged defaults to closed", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		seedCleanDivergence(t, s)
		s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Merged: true})
		if err := s.Err(); err != nil {
			t.Fatalf("SeedPR(Merged) rejected: %v", err)
		}
		prs, err := s.ListPRs("acme", "widgets")
		if err != nil {
			t.Fatalf("ListPRs: %v", err)
		}
		if len(prs) != 1 || prs[0].State != "closed" || !prs[0].Merged {
			t.Fatalf("PR = %+v, want merged and closed", prs)
		}
	})
}

// TestSelfBlockIsRefused pins that an issue cannot be recorded as blocking
// itself, on either the runtime or the seeding path. A self-block is
// unsatisfiable, so the engine's dependency gate would hold the issue forever
// with nothing to diagnose.
func TestSelfBlockIsRefused(t *testing.T) {
	t.Run("runtime", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		id := "issue:acme/widgets#7"
		if err := s.AddBlockedByIssue(id, id); err == nil {
			t.Fatal("AddBlockedByIssue accepted a self-block")
		} else if !strings.Contains(err.Error(), "cannot block itself") {
			t.Fatalf("error = %v, want a self-block refusal", err)
		}
	})

	t.Run("seeding", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		s.SeedBlockedBy("acme/widgets", 7, "acme/widgets", 7)
		err := s.Err()
		if err == nil {
			t.Fatal("SeedBlockedBy accepted a self-block")
		}
		if !strings.Contains(err.Error(), "cannot block itself") {
			t.Fatalf("error = %v, want a self-block refusal", err)
		}
	})
}

// TestSeedIssueRefusesNegativeNumber pins that an explicit number GitHub could
// never assign is refused. reserveNumber is a no-op below nextNumber, so a
// negative number would be accepted silently and embedded in node IDs.
func TestSeedIssueRefusesNegativeNumber(t *testing.T) {
	s, _ := seedBasicBoard(t)
	s.SeedIssue("acme/widgets", IssueSeed{Number: -3, Title: "impossible"})
	err := s.Err()
	if err == nil {
		t.Fatal("SeedIssue accepted a negative number")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("error = %v, want a 'not valid' refusal", err)
	}
}

// TestRemoveAbsentLabelReturnsErrNotFound pins that removing a label the issue
// does not carry is an error, not a no-op.
//
// Production issues a DELETE that GitHub answers 404 for an absent label, and
// roughly a dozen engine call sites branch on errors.Is(err, gh.ErrNotFound)
// from this exact call. Returning nil would make every one of those branches
// unreachable in a sim-backed test — a bug in the ErrNotFound-specific handling
// would pass green here and fail against real GitHub.
func TestRemoveAbsentLabelReturnsErrNotFound(t *testing.T) {
	s, _ := seedBasicBoard(t)

	err := s.RemoveLabelFromIssue("acme", "widgets", 7, "fabrik:never-applied")
	if err == nil {
		t.Fatal("removing an absent label reported success; the engine's ErrNotFound branches are unreachable")
	}
	if !errors.Is(err, gh.ErrNotFound) {
		t.Fatalf("error = %v, want it to wrap gh.ErrNotFound so errors.Is works as the engine expects", err)
	}

	// A real removal still succeeds — otherwise the assertion above would be
	// satisfied by a method that simply always fails.
	if err := s.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-ci"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	if err := s.RemoveLabelFromIssue("acme", "widgets", 7, "fabrik:awaiting-ci"); err != nil {
		t.Fatalf("removing a present label: %v", err)
	}
}

// TestDismissedReviewDoesNotCountTowardApproval pins that a dismissal
// supersedes the author's earlier verdict in the decision rollup.
//
// Filtering the raw history down to APPROVED/CHANGES_REQUESTED before
// collapsing skips a later DISMISSED instead of letting it supersede, leaving a
// dismissed approval counted. That reaches the authoritative review gate
// directly: reviewGateAuthorityVerdict trusts an APPROVED decision outright, so
// a scenario that dismisses an approval and expects the landing gate to hold
// would see it clear.
func TestDismissedReviewDoesNotCountTowardApproval(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"}).
		SeedRequiredApprovals(repoName, "main", 1).
		SeedReview(repoName, 42, gh.PRReview{Author: "carol", State: "APPROVED", Body: "lgtm"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	decision, err := s.FetchPRReviewDecision("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewDecision: %v", err)
	}
	if decision != "APPROVED" {
		t.Fatalf("decision = %q before dismissal, want APPROVED", decision)
	}

	s.SeedReview(repoName, 42, gh.PRReview{Author: "carol", State: "DISMISSED", Body: "stale"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding dismissal: %v", err)
	}

	decision, err = s.FetchPRReviewDecision("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviewDecision after dismissal: %v", err)
	}
	if decision != "REVIEW_REQUIRED" {
		t.Fatalf("decision = %q after the approval was dismissed, want REVIEW_REQUIRED; "+
			"a dismissed approval is still being counted", decision)
	}

	// The two reads must describe the same PR: FetchPRReviews already lets a
	// dismissal supersede, so a decision computed by a different reduction
	// would put the package in contradiction with itself.
	reviews, err := s.FetchPRReviews("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPRReviews: %v", err)
	}
	if len(reviews) != 1 || reviews[0].State != "DISMISSED" {
		t.Fatalf("FetchPRReviews = %+v, want carol's single DISMISSED entry", reviews)
	}
}

// TestSeedRefusesNonCanonicalState pins that a state string outside GitHub's
// own enum is refused rather than stored verbatim. Every downstream read
// compares it exactly, so "closed" on an issue (or "Open" on a PR) would never
// match and the record would be silently misclassified.
func TestSeedRefusesNonCanonicalState(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		s.SeedIssue("acme/widgets", IssueSeed{Number: 8, Title: "x", State: "closed"})
		err := s.Err()
		if err == nil {
			t.Fatal(`SeedIssue accepted State "closed"; GitHub's issue enum is OPEN/CLOSED`)
		}
		if !strings.Contains(err.Error(), "not valid") {
			t.Fatalf("error = %v, want a 'not valid' refusal", err)
		}
	})

	t.Run("pr", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		seedCleanDivergence(t, s)
		s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", State: "Open"})
		err := s.Err()
		if err == nil {
			t.Fatal(`SeedPR accepted State "Open"; GitHub's PR enum is open/closed`)
		}
		if !strings.Contains(err.Error(), "not valid") {
			t.Fatalf("error = %v, want a 'not valid' refusal", err)
		}
	})

	// The canonical values must still be accepted, or the refusals above would
	// be satisfied by an API that rejects every explicit state.
	t.Run("canonical values accepted", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		seedCleanDivergence(t, s)
		s.SeedIssue("acme/widgets", IssueSeed{Number: 8, Title: "x", State: "CLOSED"}).
			SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", State: "closed"})
		if err := s.Err(); err != nil {
			t.Fatalf("canonical states rejected: %v", err)
		}
	})
}

// TestPRHeadEqualBaseIsRefused pins that a PR whose head is its base is refused
// on both paths. GitHub answers "No commits between ...", and the shape is
// degenerate here too — the trial merge collapses to the nothing-to-merge path.
func TestPRHeadEqualBaseIsRefused(t *testing.T) {
	t.Run("runtime", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		seedCleanDivergence(t, s)
		if _, err := s.CreateDraftPR("acme", "widgets", "t", "main", "main", "body", 7); err == nil {
			t.Fatal("CreateDraftPR accepted head == base")
		} else if !strings.Contains(err.Error(), "head and base") {
			t.Fatalf("error = %v, want a head-equals-base refusal", err)
		}
	})

	t.Run("seeding", func(t *testing.T) {
		s, _ := seedBasicBoard(t)
		seedCleanDivergence(t, s)
		s.SeedPR(repoName, PRSeed{Number: 42, Head: "main", Base: "main"})
		err := s.Err()
		if err == nil {
			t.Fatal("SeedPR accepted head == base")
		}
		if !strings.Contains(err.Error(), "head and base") {
			t.Fatalf("error = %v, want a head-equals-base refusal", err)
		}
	})
}

// TestReviewRequestNoOpDoesNotBumpUpdatedAt pins that a request that changes
// nothing leaves the PR's updated-at alone, following AddLabelToIssue's
// convention: engine gates anchor on observable change, so a spurious bump is a
// change a scenario cannot distinguish from a real one.
func TestReviewRequestNoOpDoesNotBumpUpdatedAt(t *testing.T) {
	s, clk := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := s.AddReviewRequest("acme", "widgets", 42, []string{"carol"}); err != nil {
		t.Fatalf("AddReviewRequest: %v", err)
	}

	before := prUpdatedAt(t, s, 42)
	// Time must move, or an unbumped timestamp is indistinguishable from a
	// bumped one.
	clk.Advance(time.Hour)

	if err := s.AddReviewRequest("acme", "widgets", 42, []string{"carol"}); err != nil {
		t.Fatalf("re-AddReviewRequest: %v", err)
	}
	if got := prUpdatedAt(t, s, 42); !got.Equal(before) {
		t.Fatalf("re-requesting an outstanding reviewer bumped updated-at from %v to %v", before, got)
	}

	if err := s.DeleteReviewRequest("acme", "widgets", 42, []string{"dave"}); err != nil {
		t.Fatalf("DeleteReviewRequest: %v", err)
	}
	if got := prUpdatedAt(t, s, 42); !got.Equal(before) {
		t.Fatalf("withdrawing a reviewer who was not outstanding bumped updated-at from %v to %v", before, got)
	}

	// A real change must still bump, or the assertions above would be satisfied
	// by a method that never bumps at all.
	if err := s.DeleteReviewRequest("acme", "widgets", 42, []string{"carol"}); err != nil {
		t.Fatalf("DeleteReviewRequest(carol): %v", err)
	}
	if got := prUpdatedAt(t, s, 42); !got.After(before) {
		t.Fatalf("a real withdrawal did not bump updated-at (%v -> %v)", before, got)
	}
}

// prUpdatedAt reads a PR's updated-at from the model.
func prUpdatedAt(t *testing.T, s *Sim, prNumber int) time.Time {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pr, err := s.prLocked("acme", "widgets", prNumber)
	if err != nil {
		t.Fatalf("prLocked: %v", err)
	}
	return pr.updatedAt
}

// TestNoOpBoardMutationsDoNotBump pins the no-op timestamp convention on the
// two board mutations that were still bumping unconditionally.
//
// FetchProjectUpdatedAt gates whether the engine's idle poll looks at the board
// at all, so a bump for a mutation that changed nothing is a wake signal real
// GitHub does not produce — and one a scenario cannot tell from a real change.
// AddLabelToIssue, AddReviewRequest, AddProjectV2ItemById, and
// placeOnProjectLocked already follow this rule; these two were the holdouts.
func TestNoOpBoardMutationsDoNotBump(t *testing.T) {
	t.Run("re-archiving an archived card", func(t *testing.T) {
		s, clk := seedBasicBoard(t)
		projectID, itemID := mustLookupItem(t, s)
		if err := s.ArchiveProjectItem(projectID, itemID); err != nil {
			t.Fatalf("ArchiveProjectItem: %v", err)
		}

		before, err := s.FetchProjectUpdatedAt(projectID)
		if err != nil {
			t.Fatalf("FetchProjectUpdatedAt: %v", err)
		}
		// Time must move, or an unbumped timestamp is indistinguishable from a
		// bumped one.
		clk.Advance(time.Hour)

		if err := s.ArchiveProjectItem(projectID, itemID); err != nil {
			t.Fatalf("re-ArchiveProjectItem: %v", err)
		}
		after, err := s.FetchProjectUpdatedAt(projectID)
		if err != nil {
			t.Fatalf("FetchProjectUpdatedAt after re-archive: %v", err)
		}
		if !after.Equal(before) {
			t.Fatalf("re-archiving bumped the project updated-at from %v to %v", before, after)
		}
	})

	t.Run("moving a card to the column it already occupies", func(t *testing.T) {
		s, clk := seedBasicBoard(t)
		projectID, itemID := mustLookupItem(t, s)
		field, err := s.FetchStatusField(projectID)
		if err != nil {
			t.Fatalf("FetchStatusField: %v", err)
		}

		before, err := s.FetchProjectUpdatedAt(projectID)
		if err != nil {
			t.Fatalf("FetchProjectUpdatedAt: %v", err)
		}
		clk.Advance(time.Hour)

		// The card was seeded into Implement.
		if err := s.UpdateProjectItemStatus(projectID, itemID, field.FieldID, field.Options["Implement"]); err != nil {
			t.Fatalf("UpdateProjectItemStatus: %v", err)
		}
		after, err := s.FetchProjectUpdatedAt(projectID)
		if err != nil {
			t.Fatalf("FetchProjectUpdatedAt after same-column move: %v", err)
		}
		if !after.Equal(before) {
			t.Fatalf("a same-column move bumped the project updated-at from %v to %v", before, after)
		}

		// A real move must still bump, or the assertion above would be
		// satisfied by a method that never bumps at all.
		if err := s.UpdateProjectItemStatus(projectID, itemID, field.FieldID, field.Options["Review"]); err != nil {
			t.Fatalf("UpdateProjectItemStatus(Review): %v", err)
		}
		moved, err := s.FetchProjectUpdatedAt(projectID)
		if err != nil {
			t.Fatalf("FetchProjectUpdatedAt after a real move: %v", err)
		}
		if !moved.After(before) {
			t.Fatalf("a real column move did not bump updated-at (%v -> %v)", before, moved)
		}
	})
}

// TestGitIgnoresTheInvokingUsersGlobalConfig pins that this package's git
// subprocesses do not inherit ~/.gitconfig.
//
// This is the one correctness issue here that breaks the package for someone
// other than its author. commit.gpgsign=true is a common developer and CI
// setting, and without isolation every commit-writing operation in this package
// fails or hangs on a GPG prompt for reasons unrelated to the code under test.
// core.hooksPath and commit.template do the same in different ways.
//
// The test provokes it directly: a global config that would make any inheriting
// commit fail. It must not.
func TestGitIgnoresTheInvokingUsersGlobalConfig(t *testing.T) {
	skipIfNoGit(t)

	home := t.TempDir()
	cfg := "[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /nonexistent/gpg-that-does-not-exist\n"
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing global gitconfig: %v", err)
	}
	// Both are consulted for the global layer; set them together so the test
	// does not depend on which one the host happens to use.
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	s := New(t.TempDir())
	s.SeedRepo("acme/widgets").
		SeedCommit("acme/widgets", "main", map[string]string{"a.txt": "a\n"}, "a commit")
	if err := s.Err(); err != nil {
		t.Fatalf("seeding under a hostile global gitconfig: %v\n"+
			"git subprocesses are inheriting ~/.gitconfig; they must not", err)
	}
	if _, err := s.HeadSHA("acme/widgets", "main"); err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
}

// TestUpdatePRBaseRefusesHeadAsBase pins the same "No commits between ..."
// refusal createPR and SeedPR already carry, on the one path that could still
// arrive at that shape after the PR exists.
func TestUpdatePRBaseRefusesHeadAsBase(t *testing.T) {
	s, clk := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	err := s.UpdatePRBase("acme", "widgets", 42, headBranch)
	if err == nil {
		t.Fatal("UpdatePRBase retargeted a PR onto its own head branch")
	}
	if !strings.Contains(err.Error(), "its head branch") {
		t.Fatalf("error = %v, want a head-as-base refusal", err)
	}
	base, err := s.GetPRBase("acme", "widgets", 42)
	if err != nil {
		t.Fatalf("GetPRBase: %v", err)
	}
	if base != "main" {
		t.Fatalf("base = %q after a refused retarget, want main", base)
	}

	// Retargeting to the base it already has is a no-op and must not bump.
	before := prUpdatedAt(t, s, 42)
	clk.Advance(time.Hour)
	if err := s.UpdatePRBase("acme", "widgets", 42, "main"); err != nil {
		t.Fatalf("UpdatePRBase(main): %v", err)
	}
	if got := prUpdatedAt(t, s, 42); !got.Equal(before) {
		t.Fatalf("retargeting to the current base bumped updated-at from %v to %v", before, got)
	}

	// A real retarget must still work and bump.
	s.SeedBranch(repoName, "release", "main")
	if err := s.Err(); err != nil {
		t.Fatalf("SeedBranch: %v", err)
	}
	if err := s.UpdatePRBase("acme", "widgets", 42, "release"); err != nil {
		t.Fatalf("UpdatePRBase(release): %v", err)
	}
	if got := prUpdatedAt(t, s, 42); !got.After(before) {
		t.Fatalf("a real retarget did not bump updated-at (%v -> %v)", before, got)
	}
}

// TestSeedBlockedByDedupesAndStamps pins that the seeding path agrees with
// AddBlockedByIssue about what a repeated dependency link means: GitHub
// silently ignores a duplicate, and a link that is recorded bumps the issue.
func TestSeedBlockedByDedupesAndStamps(t *testing.T) {
	s, clk := seedBasicBoard(t)
	s.SeedIssue("acme/widgets", IssueSeed{Number: 8, Title: "blocker", Status: "Implement"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	before := issueUpdatedAt(t, s, 7)
	clk.Advance(time.Hour)

	s.SeedBlockedBy("acme/widgets", 7, "acme/widgets", 8)
	if err := s.Err(); err != nil {
		t.Fatalf("SeedBlockedBy: %v", err)
	}
	stamped := issueUpdatedAt(t, s, 7)
	if !stamped.After(before) {
		t.Fatalf("recording a dependency did not bump the issue's updated-at (%v -> %v)", before, stamped)
	}

	clk.Advance(time.Hour)
	s.SeedBlockedBy("acme/widgets", 7, "acme/widgets", 8)
	if err := s.Err(); err != nil {
		t.Fatalf("duplicate SeedBlockedBy reported an error; GitHub ignores duplicates: %v", err)
	}

	item, err := s.FetchProjectItem("acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchProjectItem: %v", err)
	}
	if len(item.BlockedBy) != 1 {
		t.Fatalf("blockedBy = %+v, want one entry; the duplicate was not deduped", item.BlockedBy)
	}
	if got := issueUpdatedAt(t, s, 7); !got.Equal(stamped) {
		t.Fatalf("a deduped duplicate bumped updated-at from %v to %v", stamped, got)
	}
}

// issueUpdatedAt reads an issue's updated-at from the model.
func issueUpdatedAt(t *testing.T, s *Sim, issueNumber int) time.Time {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, err := s.issueLocked("acme", "widgets", issueNumber)
	if err != nil {
		t.Fatalf("issueLocked: %v", err)
	}
	return iss.updatedAt
}
