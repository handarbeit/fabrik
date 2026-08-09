package simgh

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// isNotMergeable and isNotMergeableCI classify a MergePR refusal the way the
// engine does. Keeping them distinct matters: production must not route
// ErrNotMergeableCI into the rebase-reinvoke path, so a test that conflated
// them would not notice the model sending it there.
func isNotMergeable(err error) bool { return errors.Is(err, gh.ErrNotMergeable) }

func isNotMergeableCI(err error) bool { return errors.Is(err, gh.ErrNotMergeableCI) }

// skipIfNoGit follows the repo-wide convention (engine/worktree_test.go) of
// exercising the real git binary in tests rather than reimplementing git
// semantics, and skipping where it is unavailable.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// fakeClock is a manually-advanced Clock. Every time-bearing value the model
// produces reads from the injected clock, so a test can place label
// applications at exact instants — which is what the engine's timeout-anchored
// gates need.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// newSim builds a Sim with a fake clock over a temp base dir.
func newSim(t *testing.T) (*Sim, *fakeClock) {
	t.Helper()
	skipIfNoGit(t)
	clk := newFakeClock()
	return New(t.TempDir(), WithClock(clk)), clk
}

// seedBasicBoard builds the setup most tests need: one repo, one project with
// the usual columns, and one issue sitting in Implement. It exists to
// demonstrate R3 — a board in a given state in a few lines.
func seedBasicBoard(t *testing.T) (*Sim, *fakeClock) {
	t.Helper()
	s, clk := newSim(t)
	s.SeedRepo("acme/widgets").
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"}).
		SeedIssue("acme/widgets", IssueSeed{
			Number: 7,
			Title:  "Add a thing",
			Body:   "spec body",
			Author: "human",
			Status: "Implement",
		})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return s, clk
}

// mustHeadSHA resolves a branch tip or fails the test.
func mustHeadSHA(t *testing.T, s *Sim, ownerRepo, branch string) string {
	t.Helper()
	sha, err := s.HeadSHA(ownerRepo, branch)
	if err != nil {
		t.Fatalf("HeadSHA(%s, %s): %v", ownerRepo, branch, err)
	}
	return sha
}

// firstItem returns the single board item, failing if there is not exactly one.
func firstItem(t *testing.T, s *Sim) *itemProjection {
	t.Helper()
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("board has %d items, want 1", len(board.Items))
	}
	return &itemProjection{board.Items[0].ItemID, board.Items[0].Status, board.ProjectID}
}

type itemProjection struct {
	itemID    string
	status    string
	projectID string
}
