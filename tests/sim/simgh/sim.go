// Package simgh implements a stateful, git-backed simulation of the GitHub
// surface that Fabrik's engine talks to.
//
// It exists to fill the gap between engine/mocks_test.go — a per-call function
// hook stub where nothing carries across poll cycles — and tests/e2e, which
// drives a real daemon against real GitHub at real cost. simgh is a third
// layer: a model faithful enough that a multi-poll transition sequence can be
// asserted in an ordinary `go test` run.
//
// Two properties define it:
//
//   - It is stateful. A mutation is observable by every subsequent read, the
//     way GitHub behaves. Read projections (gh.ProjectItem, gh.PRDetails,
//     gh.BoardProbeItem) are computed fresh from the model on every call and
//     never cached, so this is structural rather than something each test has
//     to re-assert.
//
//   - It is backed by real git. Anything git can genuinely answer —
//     mergeability, merge commits, commits-behind — is computed by running the
//     real git binary against a real bare repository, not declared by the test.
//     A fake that lets a test assert mergeability cannot catch a mergeability
//     bug.
//
// What it is structurally blind to is documented in ../README.md; every place
// it knowingly diverges from real GitHub is documented in FIDELITY.md. Read
// FIDELITY.md before trusting a sim-backed test to cover a subtle behaviour.
//
// # Locking
//
// Two tiers, with a strict ordering invariant:
//
//   - Sim.mu guards all in-memory model state across every repo and project.
//   - repoState.gitMu guards git subprocess invocations against that repo's
//     bare directory. Git's own on-disk state (config, index, worktree list)
//     is not concurrency-safe, so a Go-level mutex over the model alone is not
//     enough.
//
// INVARIANT: never acquire mu while holding gitMu, and never hold mu across a
// git subprocess call. The pattern used throughout is: take mu, copy out the
// inputs, release mu; take gitMu, run git, release gitMu; take mu again to
// write the result back. A violation of this ordering surfaces only as an
// intermittent deadlock or -race failure, so it is enforced by review.
//
// # Adding a method
//
// Sim pins itself to engine.GitHubClient with a compile-time assertion, so any
// future widening of that interface breaks this package's build immediately.
// That is deliberate — it is how the layer detects drift. When it happens, add
// the new method to the grouped file matching its concern (prs.go, ci.go,
// reviews.go, board.go, labels.go, comments.go, issues.go, deps.go, misc.go),
// give it a real if simplified implementation, and record any divergence in
// FIDELITY.md. Do not add a stub that returns an error — an unimplemented
// method that compiles is exactly the silent-green failure mode this layer
// must not introduce.
package simgh

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/engine"
)

// Sim implements engine.GitHubClient against an in-memory model backed by real
// local git repositories.
var _ engine.GitHubClient = (*Sim)(nil)

// Clock supplies the current time. Every time-bearing value the model
// produces — label applied-at, comment timestamps, project updated-at, rate
// limit windows — is read from it, so a test can drive the engine's
// time-anchored gates (the review-gate timeout, the CI settle scan, the
// Done-archive scan, all of which key off FetchLabelAppliedAt) without
// wall-clock waiting.
type Clock interface {
	Now() time.Time
}

// realClock is the default Clock, reading wall time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Sim is a simulated GitHub instance. The zero value is not usable; construct
// one with New.
type Sim struct {
	// baseDir is the directory holding every backing bare repository. Callers
	// normally pass t.TempDir().
	baseDir string

	clock Clock

	// mu guards every field below it. See the package doc's locking invariant.
	mu sync.Mutex

	repos    map[string]*repoState    // keyed "owner/repo"
	projects map[string]*projectState // keyed "owner:projectNum"

	// defaultProject is the first project seeded, used by seeding helpers so a
	// single-project scenario does not have to name it repeatedly.
	defaultProject *projectState

	nextCommentDatabaseID int
	nextCheckRunID        int64

	// seededCheckRunIDs is every check run ID handed out or explicitly seeded,
	// so SeedCheckRun can refuse a duplicate. GitHub's check run IDs are unique
	// across the instance, and production relies on their ordering to pick the
	// latest rerun of a check name.
	seededCheckRunIDs map[int64]bool

	// restRate and graphqlRate back RateLimitStats. Modelled as static
	// budgets that never deplete; see FIDELITY.md.
	restRate    rateBudget
	graphqlRate rateBudget

	// seedErr holds the first error produced by a chained Seed* call. Checked
	// with Err.
	seedErr error
}

// rateBudget is the sim's static stand-in for a GitHub rate limit window.
type rateBudget struct {
	limit     int
	remaining int
	used      int
	resetIn   time.Duration
}

// Option configures a Sim at construction.
type Option func(*Sim)

// WithClock substitutes the clock every time-bearing value is read from.
func WithClock(c Clock) Option {
	return func(s *Sim) { s.clock = c }
}

// WithRateLimits overrides the static rate-limit budgets reported by
// RateLimitStats.
func WithRateLimits(restLimit, restRemaining, graphqlLimit, graphqlRemaining int) Option {
	return func(s *Sim) {
		s.restRate = rateBudget{limit: restLimit, remaining: restRemaining, used: restLimit - restRemaining, resetIn: time.Hour}
		s.graphqlRate = rateBudget{limit: graphqlLimit, remaining: graphqlRemaining, used: graphqlLimit - graphqlRemaining, resetIn: time.Hour}
	}
}

// New constructs a Sim whose backing git repositories live under baseDir.
// baseDir must exist and be writable; tests normally pass t.TempDir().
func New(baseDir string, opts ...Option) *Sim {
	s := &Sim{
		baseDir:               baseDir,
		clock:                 realClock{},
		repos:                 make(map[string]*repoState),
		projects:              make(map[string]*projectState),
		nextCommentDatabaseID: 1000,
		nextCheckRunID:        5000,
		seededCheckRunIDs:     make(map[int64]bool),
		restRate:              rateBudget{limit: 5000, remaining: 5000, resetIn: time.Hour},
		graphqlRate:           rateBudget{limit: 5000, remaining: 5000, resetIn: time.Hour},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// now reads the injected clock. Never call time.Now directly elsewhere in this
// package — determinism for the engine's time-anchored gates depends on every
// timestamp flowing through here.
func (s *Sim) now() time.Time { return s.clock.Now() }

// Err reports the first error recorded by a chained Seed* call, or nil. Seeding
// uses the sticky-error builder idiom so a scenario reads as a short chain
// rather than a ladder of error checks; call Err once at the end of setup.
func (s *Sim) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seedErr
}

// fail records a seeding error if none has been recorded yet. Caller must hold mu.
func (s *Sim) fail(format string, args ...any) {
	if s.seedErr == nil {
		s.seedErr = fmt.Errorf(format, args...)
	}
}

// repoKey normalises an owner and repo into the model's repo key.
func repoKey(owner, repo string) string { return owner + "/" + repo }

// projectKey normalises a project's owner-scoped identity. Projects v2 are
// owned by a user or organisation, not by a repository, so project state is
// keyed by (owner, number) — a repo parameter on a fetch is a query input, not
// part of the project's identity.
func projectKey(owner string, num int) string { return fmt.Sprintf("%s:%d", owner, num) }

// lookupRepo returns the repo state for owner/repo. Caller must hold mu.
func (s *Sim) lookupRepo(owner, repo string) (*repoState, error) {
	r, ok := s.repos[repoKey(owner, repo)]
	if !ok {
		return nil, fmt.Errorf("simgh: repo %s not seeded", repoKey(owner, repo))
	}
	return r, nil
}

// repoDir is the on-disk path of a repo's backing bare repository.
func (s *Sim) repoDir(owner, repo string) string {
	return filepath.Join(s.baseDir, owner+"-"+repo+".git")
}

// ensureBaseDir creates baseDir if it does not exist.
func (s *Sim) ensureBaseDir() error {
	if err := os.MkdirAll(s.baseDir, 0o755); err != nil {
		return fmt.Errorf("simgh: creating base dir %s: %w", s.baseDir, err)
	}
	return nil
}
