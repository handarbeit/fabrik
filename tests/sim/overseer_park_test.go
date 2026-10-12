package sim

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// The overseer's move_parked action (#2188, ADR 2188) against the real engine:
// a board with two parking columns, the move, its audit comment, its
// column-changed attribution and the absence of any dispatch. Serial for the
// same reason as the other channel scenarios (SetChannelTimingForTest), and it
// needs BoardCache: the action takes its project id from the board cache and the
// requester only reaches the event through the cache write-through.

// triageStages is the lifecycle pipeline with an unmanaged Triage column ahead
// of the unmanaged Backlog.
func triageStages() []*stages.Stage {
	return append([]*stages.Stage{{Name: "Triage", Order: -1, Unmanaged: true}}, lifecycleStages()...)
}

func TestOverseerMoveParkedTriageToBacklog(t *testing.T) {
	yolo := false
	env := NewEnv(t, EnvOptions{Stages: triageStages(), BoardCache: true, Yolo: &yolo})
	num := FileIssue(t, env, "triage me", "body", "Triage", "fabrik:cruise")
	RunPolls(t, env, 3) // the daemon now holds the item
	_, sink := startChannel(t, env)
	specifyCalls := env.Claude.StageCallCount("Specify")

	res, err := env.Engine.OverseerActorForTest().MoveParked(localapi.MoveParkedParams{
		Subscriber: "triager", Issue: env.OwnerRepo + "#" + strconv.Itoa(num), To: "Backlog",
	})
	if err != nil {
		t.Fatalf("MoveParked: %v", err)
	}
	if !res.Changed || res.AuditComment != localapi.AuditPosted {
		t.Fatalf("result = %+v", res)
	}
	RunPolls(t, env, 4)
	settleWait()

	if got := projectItem(t, env, num).Status; got != "Backlog" {
		t.Fatalf("board status = %q, want Backlog", got)
	}
	audits := auditComments(t, env, num)
	if len(audits) != 1 || !strings.HasPrefix(audits[0], "🏭 **Fabrik — overseer action: move_parked**") || !strings.Contains(audits[0], "`triager`") {
		t.Fatalf("audit comments = %q", audits)
	}
	evs := sink.ofType(channelevents.ColumnChanged, num)
	if len(evs) != 1 {
		t.Fatalf("column-changed = %d, want exactly 1 (all: %+v)", len(evs), sink.all())
	}
	m := evs[0].Meta
	if m["from"] != "Triage" || m["to"] != "Backlog" || m["actor"] != "overseer" || m["via"] != "triager" {
		t.Fatalf("column-changed meta = %v", m)
	}
	// Nothing starts: even a cruise item sits in an unmanaged column.
	if got := env.Claude.StageCallCount("Specify"); got != specifyCalls {
		t.Errorf("a stage was dispatched after a parking move (%d -> %d)", specifyCalls, got)
	}
	if !hasLabel(IssueLabels(t, env, num), "fabrik:cruise") {
		t.Error("the move changed the item's labels")
	}
}

// Non-vacuity (SC-001): the same Triage -> Backlog move made by a human drag is
// attributed to a human and carries no via, so the overseer/via assertions above
// are not satisfied by any column-changed event.
func TestOverseerMoveParkedAttributionIsNotVacuous(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: triageStages(), BoardCache: true})
	num := FileIssue(t, env, "dragged", "body", "Triage", "fabrik:paused", "fabrik:awaiting-input")
	RunPolls(t, env, 3)
	_, sink := startChannel(t, env)

	env.Sim.Sim().SeedProjectItem(env.Owner, env.ProjectNum, env.OwnerRepo, num, false, "Backlog")
	RunPolls(t, env, 3)
	settleWait()
	evs := sink.ofType(channelevents.ColumnChanged, num)
	if len(evs) != 1 {
		t.Fatalf("column-changed = %d, want 1 (all: %+v)", len(evs), sink.all())
	}
	if evs[0].Meta["actor"] == "overseer" {
		t.Fatalf("a hand move was attributed to the overseer: %v", evs[0].Meta)
	}
	if _, has := evs[0].Meta["via"]; has {
		t.Fatalf("a hand move carries via: %v", evs[0].Meta)
	}
}

func TestOverseerMoveParkedRefusesAPipelineItemAndWritesNothing(t *testing.T) {
	env := NewEnv(t, EnvOptions{Stages: triageStages(), BoardCache: true})
	num := FileIssue(t, env, "in the pipeline", "body", "Specify", "fabrik:paused", "fabrik:awaiting-input")
	RunPolls(t, env, 3)

	_, err := env.Engine.OverseerActorForTest().MoveParked(localapi.MoveParkedParams{
		Subscriber: "triager", Issue: strconv.Itoa(num), To: "Backlog",
	})
	var pe *localapi.Error
	if !errors.As(err, &pe) || pe.Code != localapi.CodeRefused || !strings.Contains(pe.Message, "only moves an item between parking columns") {
		t.Fatalf("MoveParked = %v, want a refusal", err)
	}
	if got := projectItem(t, env, num).Status; got != "Specify" {
		t.Errorf("a refused move changed the status to %q", got)
	}
	if n := len(auditComments(t, env, num)); n != 0 {
		t.Errorf("a refused move posted %d audit comments", n)
	}
}
