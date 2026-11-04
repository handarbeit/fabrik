package sim

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twin of #2550 (ADR 2550): GitHub answers the fabrik:editing add with a 5xx
// but applies it. Before the fix the engine took the label for absent, backed off
// and then refused the item for ever (a running daemon had no sweep for
// fabrik:editing), so the user's comment went unprocessed until a restart.
//
// The fix has two layers and each is exercised alone: the live re-read after the
// ambiguous write (R1) and the runtime orphan reclaim (R2). With both neutralised
// the comment is never processed, which is what makes the twin non-vacuous.

// lockOrphanBound is the poll budget the scenarios are allowed. Each poll advances
// the engine clock by a minute, so the 3-minute grace is long past well inside it.
const lockOrphanBound = 14

type lockOrphanOpts struct {
	reconcileOff bool
	reclaimOff   bool
	// readAlsoFails makes the re-read right after the ambiguous add fail too.
	readAlsoFails bool
}

func runLockOrphan(t *testing.T, o lockOrphanOpts) (env *Env, num int, processedAt int) {
	t.Helper()
	env = NewEnv(t, EnvOptions{Stages: smokeStages()})
	env.Engine.SetAmbiguousWriteReconcileDisabledForTest(o.reconcileOff)
	env.Engine.SetLockOrphanReclaimDisabledForTest(o.reclaimOff)
	env.Claude.ForStageComments("Specify", simclaude.CommentReviewNoMarker())

	num = seedCompletedSpecify(t, env, "editing add answered 500 but applied")
	isEditing := func(a simgh.Args) bool { return a.Number == num && a.Label == "fabrik:editing" }
	var armed atomic.Bool
	env.Sim.Faults().BeforeCall("AddLabelToIssue", func(a simgh.Args) {
		if isEditing(a) {
			armed.Store(true)
		}
	})
	env.Sim.Faults().ApplyThenFailWhen("AddLabelToIssue", isEditing, 1, simgh.ErrServerError())
	if o.readAlsoFails {
		// The first label read after the ambiguous add is the engine's reconcile.
		env.Sim.Faults().FailWhen("FetchLabels", func(simgh.Args) bool { return armed.Load() }, 1, simgh.ErrServerError())
	}

	env.Clock.Advance(time.Minute)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "please tweak the wording")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	for i := 1; i <= lockOrphanBound; i++ {
		env.Clock.Advance(time.Minute)
		RunPoll(t, env)
		if env.Claude.CommentCallCount("Specify") > 0 && processedAt == 0 {
			processedAt = i
		}
	}
	if env.Sim.Faults().FiredCount("AddLabelToIssue") != 1 {
		t.Fatalf("the ambiguous add fault fired %d times, want 1 (the scenario would be vacuous)", env.Sim.Faults().FiredCount("AddLabelToIssue"))
	}
	return env, num, processedAt
}

func TestLockOrphan_Sim_AmbiguousEditingAddIsRecovered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts lockOrphanOpts
	}{
		{"R1 alone (reclaim off)", lockOrphanOpts{reclaimOff: true}},
		{"R2 alone (reconcile off)", lockOrphanOpts{reconcileOff: true}},
		{"R2 backs up a re-read that also fails", lockOrphanOpts{readAlsoFails: true}},
		{"both layers", lockOrphanOpts{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, num, at := runLockOrphan(t, tc.opts)
			if at == 0 {
				t.Fatalf("the pending comment was not processed within %d polls\n\n%s", lockOrphanBound, diagnostics(env))
			}
			if hasLabel(IssueLabels(t, env, num), "fabrik:editing") {
				t.Errorf("fabrik:editing still on the item after %d polls", lockOrphanBound)
			}
		})
	}
}

// Non-vacuity: with the reconcile and the reclaim both neutralised the item stays
// stuck for ever, and reads stalled — not a gate hold or a plain wait — meanwhile.
func TestLockOrphan_Sim_NeutralisedStaysStuckAndReadsStalled(t *testing.T) {
	t.Parallel()
	env, num, at := runLockOrphan(t, lockOrphanOpts{reconcileOff: true, reclaimOff: true})
	if at != 0 {
		t.Fatalf("the comment was processed (poll %d) with both fixes neutralised: the twin proves nothing", at)
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:editing") {
		t.Fatalf("fabrik:editing is gone with both fixes neutralised: the twin proves nothing")
	}
	st, err := env.Engine.LocalAPIBackend().Status(localapi.StatusParams{Issue: strconv.Itoa(num)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Attention.State != "stalled" || st.Attention.Code != "lock-orphaned" {
		t.Fatalf("attention = %+v, want stalled/lock-orphaned", st.Attention)
	}
}
