// Package sim is tests/sim's harness: it wires tests/sim/simgh (a stateful,
// git-backed simulation of GitHub) and tests/sim/simclaude (a scripted
// engine.ClaudeInvoker) into a real engine.Engine via engine.NewWithDeps, and
// provides deterministic poll advancement (poll.go) and an assertion
// vocabulary named after tests/e2e/harness.go (assertions.go) — see
// README.md's vocabulary-mapping table for the handful of places the two
// beds diverge, and ADR-1449 for why the two engine-side seams
// (Engine.PollOnce, Engine.SetClock) and the internal/claudeerr extraction
// exist.
package sim

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// skipIfNoGit follows the repo-wide convention (engine/worktree_test.go,
// tests/sim/simgh/helpers_test.go, tests/sim/simclaude/invoker_test.go) of
// exercising the real git binary rather than reimplementing git semantics.
func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// Env is one scenario's fully-wired sim bed: a real engine.Engine pointed at
// a real WorktreeManager (backed by a local clone of simgh's repo, standing
// in for the real GitHub remote — R6), a scripted Claude invoker, and a
// shared Clock driving both simgh's GitHub-anchored timestamps and the
// engine's own CooldownAt seam in lockstep (R3).
type Env struct {
	T      *testing.T
	Engine *engine.Engine
	Sim    *simgh.Instrumented
	Clock  *Clock
	Claude *simclaude.Invoker

	// Owner/Repo/OwnerRepo identify the single repo this Env manages —
	// mirrors tests/e2e's Env.RepoAlpha in spirit, singular here because a
	// smoke-scale scenario needs exactly one.
	Owner     string
	Repo      string
	OwnerRepo string

	ProjectOwner string
	ProjectNum   int

	// issueSeqMu/issueSeqNext back FileIssue's explicit issue-number
	// assignment (assertions.go) — deterministic rather than relying on
	// simgh's auto-assign, so a scenario always knows the number it just
	// filed without a round-trip to discover it.
	issueSeqMu   sync.Mutex
	issueSeqNext int
}

// EnvOptions configures NewEnv. Every field has a working default; a
// scenario only sets what it cares about.
type EnvOptions struct {
	// Owner/Repo identify the managed repo. Default "acme"/"widgets".
	Owner string
	Repo  string

	// ProjectOwner/ProjectNum identify the project board. Default Owner/1.
	ProjectOwner string
	ProjectNum   int

	// Stages is the pipeline the engine runs. Required — there is no
	// sensible default shape (see smoke_test.go's SmokeStages for the R7
	// scenario's own 7-stage pipeline).
	Stages []*stages.Stage

	// Columns are the project board's Status column names, leftmost first.
	// Defaults to one column per non-holding stage in Stages, in Order.
	Columns []string

	// Claude is the scripted invoker. Defaults to a fresh simclaude.New().
	Claude *simclaude.Invoker

	// MaxConcurrent is the engine's worker concurrency (AC8 requires a
	// smoke-scenario run with MaxConcurrent > 1 to be race-clean). Default 2.
	MaxConcurrent int

	// Yolo sets cfg.Yolo. Default true — every scenario in this package
	// exercises the auto-advancing path; a scenario wanting manual advance
	// sets this false explicitly.
	Yolo *bool

	// StartTime seeds the shared Clock. Default 2026-01-01 00:00:00 UTC — an
	// arbitrary fixed instant, deliberately not time.Now(), so a scenario
	// asserting durations never depends on wall-clock reality.
	StartTime time.Time

	// ConfigureCfg, when non-nil, is called with the constructed Config
	// before NewWithDeps — an escape hatch for fields not covered above
	// (e.g. ReviewWaitTimeout, CIWaitTimeout) without growing this struct
	// for every engine.Config field a future scenario might need.
	ConfigureCfg func(*engine.Config)
}

// stageColumns returns one column name per non-holding, non-unmanaged stage
// in stgs, in ascending Order — the default project-board shape for a
// pipeline that doesn't declare its own Columns.
func stageColumns(stgs []*stages.Stage) []string {
	sorted := append([]*stages.Stage(nil), stgs...)
	// Simple insertion sort by Order — pipelines here are a handful of
	// stages, and stages.FindStage elsewhere in this repo makes the same
	// "small N, don't import sort for it" choice.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].Order < sorted[j-1].Order; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	cols := make([]string, 0, len(sorted))
	for _, s := range sorted {
		cols = append(cols, s.Name)
	}
	return cols
}

// NewEnv constructs a fully-wired Env: seeds a repo and project in simgh,
// wraps it with Instrument (load-bearing for AC7's mutation-log diagnostics
// — see ADR-1449), reproduces production's git topology by cloning simgh's
// backing repo as a local "origin" (R6), and boots a real engine.Engine via
// NewWithDeps with the shared Clock wired to both simgh and the engine.
func NewEnv(t *testing.T, opts EnvOptions) *Env {
	t.Helper()
	skipIfNoGit(t)

	owner := opts.Owner
	if owner == "" {
		owner = "acme"
	}
	repo := opts.Repo
	if repo == "" {
		repo = "widgets"
	}
	ownerRepo := owner + "/" + repo

	projectOwner := opts.ProjectOwner
	if projectOwner == "" {
		projectOwner = owner
	}
	projectNum := opts.ProjectNum
	if projectNum == 0 {
		projectNum = 1
	}

	if len(opts.Stages) == 0 {
		t.Fatal("sim.NewEnv: EnvOptions.Stages is required")
	}
	columns := opts.Columns
	if len(columns) == 0 {
		columns = stageColumns(opts.Stages)
	}

	startTime := opts.StartTime
	if startTime.IsZero() {
		startTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	clk := NewClock(startTime)

	simModel := simgh.New(t.TempDir(), simgh.WithClock(clk)).
		SeedRepo(ownerRepo).
		SeedProject(projectOwner, projectNum, "Engineering", columns)
	if err := simModel.Err(); err != nil {
		t.Fatalf("sim.NewEnv: seeding: %v", err)
	}
	inst := simgh.Instrument(simModel)

	wm := buildWorktreeManager(t, simModel, ownerRepo)

	claude := opts.Claude
	if claude == nil {
		claude = simclaude.New()
	}

	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = 2
	}
	yolo := true
	if opts.Yolo != nil {
		yolo = *opts.Yolo
	}

	cfg := engine.Config{
		Owner:         owner,
		Repo:          repo,
		ProjectNum:    projectNum,
		OwnerType:     "User",
		User:          "fabrik-sim-bot",
		Token:         "sim-token",
		Yolo:          yolo,
		MaxConcurrent: maxConcurrent,
		MaxRetries:    3,
		PollSeconds:   1,
		Stages:        opts.Stages,
	}
	if opts.ConfigureCfg != nil {
		opts.ConfigureCfg(&cfg)
	}

	eng := engine.NewWithDeps(cfg, inst, claude, wm)
	eng.SetClock(clk)

	return &Env{
		T:            t,
		Engine:       eng,
		Sim:          inst,
		Clock:        clk,
		Claude:       claude,
		Owner:        owner,
		Repo:         repo,
		OwnerRepo:    ownerRepo,
		ProjectOwner: projectOwner,
		ProjectNum:   projectNum,
	}
}

// buildWorktreeManager reproduces production's git topology (R6): a real
// bare-clone-of-a-bare-clone acting as the engine's "origin", exactly as
// ensureBareClone (engine/worktree.go) bare-clones the real GitHub remote —
// a local filesystem path stands in for the network URL. This is not a
// stub: git creates a genuine origin remote pointing at simgh's backing
// repo, so pushes are observable by reading simModel's repo afterward
// (AC3), and DefaultBaseBranch resolves the same way it does in production
// (step 1: refs/remotes/origin/HEAD, kept fresh here exactly as
// ensureBareClone keeps it fresh).
func buildWorktreeManager(t *testing.T, simModel *simgh.Sim, ownerRepo string) *engine.WorktreeManager {
	t.Helper()

	simBareDir, err := simModel.RepoBareDir(ownerRepo)
	if err != nil {
		t.Fatalf("buildWorktreeManager: %v", err)
	}

	localBareDir := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "", "clone", "--bare", simBareDir, localBareDir)
	// Bare clones don't set a default fetch refspec — mirrors
	// ensureBareClone's post-clone repair exactly.
	runGit(t, localBareDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, localBareDir, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*")
	// Populates refs/remotes/origin/HEAD, which DefaultBaseBranch's step 1
	// reads — git clone --bare alone does not create this ref.
	runGit(t, localBareDir, "remote", "set-head", "origin", "--auto")

	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	owner, repo, _ := strings.Cut(ownerRepo, "/")
	return engine.NewWorktreeManagerForRepo(localBareDir, worktreeRoot, owner+"-"+repo)
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s (dir=%q): %v: %s", strings.Join(args, " "), dir, err, out)
	}
}
