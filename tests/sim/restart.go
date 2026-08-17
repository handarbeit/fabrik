package sim

import (
	"os/exec"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// This file is R3's restart harness: RestartEnv discards a scenario's
// engine.Engine and rebuilds one against exactly the state a real GitHub (and
// a real on-disk worktree tree) would have retained across a process
// restart — the genuine process-state boundary R3's kill-point scenarios
// need, as distinct from simply running another poll against the same
// process (which every other scenario in this package already does).
//
// Two correctness properties are load-bearing, not incidental:
//
//  1. The snapshot must be taken via Instrumented.Snapshot, not the bare
//     Sim.Snapshot — RestoreInstrumented refuses a snapshot that didn't carry
//     the fault schedule and the mutation log, precisely so a scenario
//     relying on a persistent fault to survive the restart cannot silently
//     see GitHub "heal" itself across the boundary.
//  2. The rebuilt Engine must reuse the *original* Env's WorktreeManager
//     (env.WM), never a freshly-cloned one. A fresh clone would produce an
//     empty worktree with nothing to recover, and a "restart recovery"
//     scenario built on top of it would pass by accident (nothing was lost
//     because nothing was there to begin with) rather than by proving
//     recovery. The WorktreeManager's own worktree root is just a directory
//     under t.TempDir() — it survives constructing a second engine.Engine in
//     the same test process without any filesystem-level restart simulation
//     needed.
//  3. The reused WorktreeManager's own "origin" remote must be re-pointed at
//     the *restored* Sim's backing bare repository, not left on the
//     pre-restart one. RestoreInstrumented's baseDir is always a fresh
//     t.TempDir() (Snapshot's own doc comment: a stale git-worktree admin
//     entry pointing at the old baseDir must never be copied forward), so
//     the restored Sim's git content genuinely lives at a new filesystem
//     path — but env.WM's own origin remote config, untouched by reuse,
//     still points at the pre-restart path. That old directory is not
//     deleted until the whole test ends (t.TempDir() cleanup), so a push
//     through it does not error — it silently lands in a repository nothing
//     reads from afterward, while any *branch that already existed at
//     snapshot time* was copied forward and still "resolves" the moment
//     something merely checks for its existence — repeatedly, deceptively
//     unremarkable, since simgh's ListPRs/CreateDraftPR check existence, not
//     staleness. This is invisible to a scenario whose stages reuse the same
//     branch name throughout (fabrik/issue-N, present pre-restart, so its
//     mere existence post-restart masks a misdirected push of new commits)
//     — restart_recovery_test.go's own four kill points never happen to push
//     a genuinely new branch after restart, which is exactly why this went
//     undetected until #1452's merge-train restart scenarios, whose every
//     trial branch is a brand-new name with no pre-restart existence to hide
//     behind. Fixed here once, for every RestartEnv caller, rather than
//     patched around in the caller that happened to notice.
//
// The shared Clock and simclaude.Invoker are also carried forward
// deliberately, not rebuilt: a real process crash does not rewind wall-clock
// time or forget which scripts a scenario registered, so a restart scenario
// that wants either of those to change does so explicitly, the same way any
// other scenario in this package would.
//
// See adrs/1451-sim-bed-restart-harness.md for the full rationale.

// RestartEnv discards env's engine.Engine and returns a new Env wired
// against a freshly-restored simgh model (via Instrumented.Snapshot/
// simgh.RestoreInstrumented) and a freshly-constructed engine.Engine — but
// reusing env's WorktreeManager, Clock, and Claude invoker unchanged. The
// returned Env's Sim, Engine, and issue-number sequence are independent of
// env's from this point on; env itself must not be polled again afterward
// (nothing enforces this — as with a real restart, the old process is simply
// gone).
func RestartEnv(t *testing.T, env *Env) *Env {
	t.Helper()

	stagingDir := t.TempDir()
	snap, err := env.Sim.Snapshot(stagingDir)
	if err != nil {
		t.Fatalf("RestartEnv: snapshot: %v", err)
	}

	baseDir := t.TempDir()
	restored, err := simgh.RestoreInstrumented(snap, baseDir, simgh.WithClock(env.Clock))
	if err != nil {
		t.Fatalf("RestartEnv: restore: %v", err)
	}

	// Re-point the reused WorktreeManager's own "origin" remote at the
	// restored Sim's new backing repo location — see property 3 in this
	// file's own doc comment above for why this is load-bearing, not
	// cosmetic. Scoped to env.OwnerRepo only: a second-repo Env
	// (EnvOptions.SecondRepo) has no beta WorktreeManager reachable from
	// here at all (env.go builds and registers it locally, never storing it
	// back on Env) — a pre-existing, separate gap this fix does not attempt
	// to close.
	if newBareDir, rerr := restored.Sim().RepoBareDir(env.OwnerRepo); rerr != nil {
		t.Fatalf("RestartEnv: resolving restored bare repo dir for %s: %v", env.OwnerRepo, rerr)
	} else if out, serr := exec.Command("git", "-C", env.WM.BaseDir(), "remote", "set-url", "origin", newBareDir).CombinedOutput(); serr != nil {
		t.Fatalf("RestartEnv: re-pointing origin to %s: %v: %s", newBareDir, serr, out)
	}

	eng := engine.NewWithDeps(env.cfg, restored, env.Claude, env.WM)
	eng.SetClock(env.Clock)
	// Deliberately does NOT call Engine.RegisterObservers (ADR-1592) — see
	// NewEnv's own doc comment for why this package leaves it uncalled by
	// default. A scenario needing reactive-observer behavior on a restarted
	// Engine (none currently do) must call it explicitly on the returned Env.

	env.issueSeqMu.Lock()
	nextIssue := env.issueSeqNext
	env.issueSeqMu.Unlock()

	return &Env{
		T:             t,
		Engine:        eng,
		Sim:           restored,
		Clock:         env.Clock,
		Claude:        env.Claude,
		Owner:         env.Owner,
		Repo:          env.Repo,
		OwnerRepo:     env.OwnerRepo,
		OwnerRepoBeta: env.OwnerRepoBeta,
		WM:            env.WM,
		ProjectNum:    env.ProjectNum,
		PollInterval:  env.PollInterval,
		issueSeqNext:  nextIssue,
		cfg:           env.cfg,
	}
}
