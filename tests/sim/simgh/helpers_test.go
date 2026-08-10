package simgh

import (
	"errors"
	"os/exec"
	"reflect"
	"sync"
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
//
// It is mutex-guarded because Clock's contract requires it (see sim.go): Now is
// called from every worker goroutine — on every timestamped model read, on
// every schedule drain, and on every call the instrumentation layer intercepts
// — while Advance writes. An unguarded time.Time field would be a data race
// the moment a scenario advanced the clock with anything in flight, which is
// how a harness driving Engine.Run() necessarily uses it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

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

// newInstrumented builds a Sim over the usual basic board and wraps it in the
// instrumentation layer.
func newInstrumented(t *testing.T) (*Instrumented, *fakeClock) {
	t.Helper()
	s, clk := seedBasicBoard(t)
	return Instrument(s), clk
}

// callInterfaceMethod invokes the named engine.GitHubClient method on in with
// zero-valued arguments, discarding whatever it returns.
//
// The reflection-driven completeness tests care only that the call was
// intercepted, not that it succeeded — most of these fail inside the model
// because nothing relevant is seeded, and that is fine: a failed call is still
// an intercepted one, and the log records attempts rather than mutations.
// Pointer parameters get a fresh zero value rather than nil, so a method that
// rejects nil outright still reaches its wrapper the same way.
func callInterfaceMethod(t *testing.T, in *Instrumented, name string) {
	t.Helper()
	m := reflect.ValueOf(in).MethodByName(name)
	if !m.IsValid() {
		t.Fatalf("*Instrumented has no method %q — every engine.GitHubClient method needs a wrapper", name)
	}
	mt := m.Type()
	args := make([]reflect.Value, mt.NumIn())
	for i := range args {
		at := mt.In(i)
		if at.Kind() == reflect.Pointer {
			args[i] = reflect.New(at.Elem())
			continue
		}
		args[i] = reflect.Zero(at)
	}
	m.Call(args)
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

// firstItemFull is firstItem's counterpart for tests that need the whole
// projected item rather than just its identifiers.
func firstItemFull(t *testing.T, s *Sim) *gh.ProjectItem {
	t.Helper()
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("board has %d items, want 1", len(board.Items))
	}
	return &board.Items[0]
}
