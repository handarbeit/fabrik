package sim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// Sim twin of the live TestCommentReentryShowsReworking (#1802 / ADR-1802,
// tests/e2e/mid_stage_label_test.go): when a human comment re-enters an
// already-completed stage, beginStageRework swaps stage:<S>:complete for the
// fabrik:reworking:<S> marker, and the engine restores it afterwards — via
// handleStageComplete on a completing exit, via endStageRework otherwise.
//
// The assertion is an ordering claim, so it is made on simgh's label-event log
// (LabelEvents), which — unlike the mutation log — records an event only on a
// real state change. A mutation-log check would pass vacuously with
// beginStageRework neutralised: complete would never leave, and the later
// restore add would still log as a successful (no-op) add.
//
// The prefix is a literal copy of the engine's unexported reworkingLabelPrefix,
// as in tests/e2e/label_events.go. A drifted literal is caught by these tests
// themselves: the marker event would never be found.
const reworkingLabelPrefix = "fabrik:reworking:"

// reworkStage is the stage under test, as in the live test.
const reworkStage = "Research"

// reworkMidRunProbe records, from inside the comment worker, the labels the
// item carries at the moment the worker runs — the window between
// beginStageRework and the restore. The live test only polls this
// informationally; here the script runs synchronously inside the worker, so it
// is deterministic. It is supplementary to the four-step event chain.
type reworkMidRunProbe struct {
	mu       sync.Mutex
	calls    int
	labels   []string
	labelErr error
}

func (p *reworkMidRunProbe) wrap(env *Env, num int, inner simclaude.CommentScript) simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		p.mu.Lock()
		p.calls++
		p.labels, p.labelErr = env.Sim.Sim().FetchLabels(env.Owner, env.Repo, num)
		p.mu.Unlock()
		return inner(ctx, stage, issue, comments, workDir, opts)
	}
}

// firstLabelEventAfter returns the Seq of the first event of kind for label
// with Seq > after, or 0 if none — the live test's firstEvent(kind, label,
// afterID).
func firstLabelEventAfter(evs []simgh.LabelEvent, kind simgh.LabelEventKind, label string, after int64) int64 {
	for _, e := range evs {
		if e.Seq > after && e.Kind == kind && e.Label == label {
			return e.Seq
		}
	}
	return 0
}

// checkReworkSequence is the sim analogue of the live checkReworkSequence: the
// four ordered label events after sinceSeq.
func checkReworkSequence(t *testing.T, evs []simgh.LabelEvent, stage string, sinceSeq int64) {
	t.Helper()
	marker := reworkingLabelPrefix + stage
	complete := "stage:" + stage + ":complete"
	steps := []struct {
		kind  simgh.LabelEventKind
		label string
		what  string
	}{
		{simgh.LabelEventLabeled, marker, "marker applied first"},
		{simgh.LabelEventUnlabeled, complete, "complete removed while the marker is present"},
		{simgh.LabelEventLabeled, complete, "complete restored"},
		{simgh.LabelEventUnlabeled, marker, "marker removed last"},
	}
	after := sinceSeq
	for i, st := range steps {
		seq := firstLabelEventAfter(evs, st.kind, st.label, after)
		if seq == 0 {
			t.Fatalf("step %d (%s): no %q event for %q after seq %d\nevents after seq %d: %+v",
				i+1, st.what, st.kind, st.label, after, sinceSeq, eventsAfter(evs, sinceSeq))
		}
		after = seq
	}
}

func eventsAfter(evs []simgh.LabelEvent, after int64) []simgh.LabelEvent {
	var out []simgh.LabelEvent
	for _, e := range evs {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

// runCommentReentryRework drives one item through the stage to a parked
// complete, posts a human comment, and asserts the rework label sequence and
// final state. script is the comment worker's behaviour (completing or not).
func runCommentReentryRework(t *testing.T, script simclaude.CommentScript) {
	t.Helper()
	env := gateEnv(t) // smokeStages, Yolo=false: a completed stage parks
	complete := "stage:" + reworkStage + ":complete"
	marker := reworkingLabelPrefix + reworkStage

	// Specify is seeded complete and the item placed at Research, so the
	// engine's own Research run applies stage:Research:complete. It must be
	// engine-applied: beginStageRework reads the dispatch-time snapshot and
	// no-ops when the complete label is absent. No autonomy label, so the item
	// parks there.
	num := FileIssue(t, env, "comment re-entry rework", "body", reworkStage, "stage:Specify:complete")
	probe := &reworkMidRunProbe{}
	env.Claude.ForStageComments(reworkStage, probe.wrap(env, num, script))
	WaitForIssueLabel(t, env, num, complete, 40)
	RunPolls(t, env, 3) // settle: nothing further may happen while parked
	if env.Claude.CommentCallCount(reworkStage) != 0 {
		t.Fatalf("comment worker ran before any comment was posted")
	}

	sinceSeq, err := env.Sim.Sim().LastLabelEventSeq(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("LastLabelEventSeq: %v", err)
	}

	// Advance the clock so the comment's updatedAt visibly changes and the
	// parked item is re-admitted (precedent: TestPostMergeGuard_LateCommentIsNotApplied).
	env.Clock.Advance(time.Second)
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "maintainer", "please tighten the research findings")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedComment: %v", err)
	}

	// The marker's removal is the last event of the sequence.
	AdvanceUntil(t, env, func(env *Env) bool {
		evs, _ := env.Sim.Sim().LabelEvents(env.Owner, env.Repo, num)
		return firstLabelEventAfter(evs, simgh.LabelEventUnlabeled, marker, sinceSeq) != 0
	}, 40)

	evs, err := env.Sim.Sim().LabelEvents(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("LabelEvents: %v", err)
	}
	checkReworkSequence(t, evs, reworkStage, sinceSeq)

	// Final state: complete present, marker gone, still at the stage's column.
	labels := IssueLabels(t, env, num)
	if !hasLabel(labels, complete) {
		t.Errorf("final labels %v: %s missing — the rework did not restore it", labels, complete)
	}
	if hasLabel(labels, marker) {
		t.Errorf("final labels %v: marker %s still present", labels, marker)
	}
	if got := projectItem(t, env, num).Status; got != reworkStage {
		t.Errorf("final Status = %q, want %q — comment re-entry must not advance the item", got, reworkStage)
	}

	// Supplementary (beyond the live four): while the worker ran, complete was
	// absent and the marker present.
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if probe.calls != 1 || probe.labelErr != nil {
		t.Fatalf("comment worker calls = %d (err %v), want exactly 1", probe.calls, probe.labelErr)
	}
	if hasLabel(probe.labels, complete) || !hasLabel(probe.labels, marker) {
		t.Errorf("mid-run labels %v: want %s present and %s absent while the worker runs", probe.labels, marker, complete)
	}
}

// TestCommentReentryShowsReworking_CompletingExit covers the restore through
// handleStageComplete (the worker signals completion) followed by
// endStageRework removing the marker.
func TestCommentReentryShowsReworking_CompletingExit(t *testing.T) {
	t.Parallel()
	runCommentReentryRework(t, simclaude.DefaultCommentScript)
}

// TestCommentReentryShowsReworking_NonCompletingExit covers the restore
// through endStageRework's direct re-add (the worker makes no progress).
func TestCommentReentryShowsReworking_NonCompletingExit(t *testing.T) {
	t.Parallel()
	runCommentReentryRework(t, simclaude.NoOpCommentReview())
}
