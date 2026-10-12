package sim

import (
	"strconv"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2121 scenarios, through a real Engine: an item that has work to do while every
// worker slot is busy is "waiting for dispatch" with the all-slots-busy reason,
// never idle (F10); and the engine's dispatchable predicate agrees with the real
// dispatcher — whenever status says the item is waiting for dispatch and a slot
// frees, the next poll really dispatches it.

func attentionWorkerStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Research", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

func TestSimCruiseItemWithAllSlotsBusyIsWaitingForDispatchThenDispatched(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: attentionWorkerStages(), MaxConcurrent: 2})
	env.Claude.ForStage("Specify", simclaude.DefaultScript)
	num := FileIssue(t, env, "slot starved cruise", "Specify has not started.", "Specify", "fabrik:cruise")

	release := env.Engine.HoldGeneralWorkerSlotsForTest()
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	RunPoll(t, env)

	st, err := env.Engine.LocalAPIBackend().Status(localapi.StatusParams{Issue: strconv.Itoa(num)})
	if err != nil {
		t.Fatal(err)
	}
	if st.Attention.State == "idle" {
		t.Fatalf("a dispatchable item is never idle: %+v", st.Attention)
	}
	if st.Attention.State != "waiting" || st.Attention.Code != "waiting-for-dispatch" || !strings.Contains(st.Attention.Summary, "all 2 of 2 worker slots are busy") {
		t.Fatalf("attention = %+v", st.Attention)
	}
	if st.Attention.Cause != "slot-starved" {
		t.Errorf("cause = %q, want slot-starved", st.Attention.Cause)
	}
	if st.Worker.InFlight {
		t.Errorf("no worker holds a slot for the item: %+v", st.Worker)
	}
	if n := env.Claude.StageCallCount("Specify"); n != 0 {
		t.Fatalf("Specify invoked %d times with every slot held", n)
	}

	// Parity: free the slots and the dispatcher really takes the item.
	release()
	released = true
	AdvanceUntil(t, env, func(e *Env) bool { return e.Claude.StageCallCount("Specify") > 0 }, 20)
}
