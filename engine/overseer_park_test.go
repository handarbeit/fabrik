package engine

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

// triageStages is apiStages() plus an unmanaged Triage column ahead of Backlog:
// the two-parking-column board #2188 is about.
func triageStages() []*stages.Stage {
	out := []*stages.Stage{{Name: "Triage", Order: 0, Unmanaged: true}}
	return append(out, apiStages()...)
}

// parkEngine is overseerEngine on a board with both Triage and Backlog.
func parkEngine(t *testing.T, client *mockGitHubClient, items ...ovItem) (*Engine, *overseerActor) {
	t.Helper()
	eng, _, act := overseerEngine(t, client, items...)
	eng.cfg.Stages = triageStages()
	eng.statusField.Options["Triage"] = "OPT_Triage"
	return eng, act
}

func park(act *overseerActor, issue, to string) (*localapi.ActionResult, error) {
	return act.MoveParked(localapi.MoveParkedParams{Subscriber: "my-session", Issue: issue, To: to})
}

func TestOverseerMoveParked_BothDirections(t *testing.T) {
	for _, tc := range []struct{ from, to string }{{"Triage", "Backlog"}, {"Backlog", "Triage"}} {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			client := &mockGitHubClient{}
			liveStatus(client, tc.from)
			forbidIDLookups(t, client)
			eng, act := parkEngine(t, client, ovItem{number: 5, status: tc.from})

			res, err := park(act, "5", tc.to)
			if err != nil {
				t.Fatalf("MoveParked: %v", err)
			}
			if !res.Changed || res.Action != localapi.MethodMoveParked || res.AuditComment != localapi.AuditPosted {
				t.Errorf("result = %+v", res)
			}
			want := []updateStatusCall{{ovProjectID, "PVTI_5", "FIELD_1", "OPT_" + tc.to}}
			if !reflect.DeepEqual(client.updateStatusCalls, want) {
				t.Errorf("status writes = %v, want %v", client.updateStatusCalls, want)
			}
			if len(client.addLabelCalls)+len(client.removeLabelCalls) != 0 {
				t.Errorf("move_parked must write no labels: %v %v", client.addLabelCalls, client.removeLabelCalls)
			}
			bodies := commentBodies(client)
			if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "🏭 **Fabrik — overseer action: move_parked**") ||
				!strings.Contains(bodies[0], "`my-session`") || !strings.Contains(bodies[0], tc.from+" to "+tc.to) {
				t.Errorf("audit comment = %q", bodies)
			}
			if got := statusOf(t, eng, 5); got != tc.to {
				t.Errorf("cache status = %q, want %q (write-through)", got, tc.to)
			}
			if !reflect.DeepEqual(client.fetchProjectItemStatusCalls, []string{"PVTI_5"}) {
				t.Errorf("live status reads = %v", client.fetchProjectItemStatusCalls)
			}
		})
	}
}

// Parking moves start no work and change no label: a cruise/yolo item moves
// like any other with no auto-advance note, and a paused item stays paused.
func TestOverseerMoveParked_AutonomyAndPauseUntouched(t *testing.T) {
	labels := []string{"fabrik:cruise", "fabrik:paused", "fabrik:awaiting-input"}
	client := &mockGitHubClient{}
	liveStatus(client, "Triage")
	eng, act := parkEngine(t, client, ovItem{number: 5, status: "Triage", labels: labels})
	res, err := park(act, "5", "Backlog")
	if err != nil {
		t.Fatalf("MoveParked: %v", err)
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want none (nothing is dispatched)", res.Notes)
	}
	got := labelsOf(t, eng, 5)
	for _, l := range labels {
		if !hasLabelStr(got, l) {
			t.Errorf("label %s lost: %v", l, got)
		}
	}
	if len(client.addLabelCalls)+len(client.removeLabelCalls) != 0 {
		t.Errorf("labels written: %v %v", client.addLabelCalls, client.removeLabelCalls)
	}
}

func TestOverseerMoveParked_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		item   ovItem
		to     string
		live   string
		liveEr error
		mutate func(*Engine)
		issue  string
		want   string
		// targets is the expected ValidTargets (nil = must be omitted).
		targets []string
	}{
		{name: "pipeline source", item: ovItem{number: 5, status: "Specify"}, to: "Backlog", live: "Specify", want: "only moves an item between parking columns"},
		{name: "holding source", item: ovItem{number: 5, status: "Queued"}, to: "Backlog", live: "Queued", want: "only moves an item between parking columns"},
		{name: "cleanup source", item: ovItem{number: 5, status: "Done"}, to: "Backlog", live: "Done", want: "only moves an item between parking columns"},
		{name: "source with no stage", item: ovItem{number: 5, status: "Nowhere"}, to: "Backlog", live: "Nowhere", want: "no configured stage"},
		{name: "unknown source", item: ovItem{number: 5, status: ""}, to: "Backlog", want: "no known board column"},
		{name: "pipeline destination", item: ovItem{number: 5, status: "Triage"}, to: "Specify", live: "Triage", want: "not a valid parking destination", targets: []string{"Backlog"}},
		{name: "holding destination", item: ovItem{number: 5, status: "Triage"}, to: "Queued", live: "Triage", want: "not a valid parking destination", targets: []string{"Backlog"}},
		{name: "cleanup destination", item: ovItem{number: 5, status: "Triage"}, to: "Done", live: "Triage", want: "not a valid parking destination", targets: []string{"Backlog"}},
		{name: "destination with no stage", item: ovItem{number: 5, status: "Triage"}, to: "Nowhere", live: "Triage", want: "not a valid parking destination", targets: []string{"Backlog"}},
		{name: "missing to", item: ovItem{number: 5, status: "Triage"}, to: "", live: "Triage", want: "`to` is required", targets: []string{"Backlog"}},
		{name: "parking destination without a board option", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "Triage",
			mutate: func(e *Engine) { delete(e.statusField.Options, "Backlog") }, want: "no matching option on the board"},
		{name: "same column", item: ovItem{number: 5, status: "Triage"}, to: "Triage", live: "Triage", want: "already in", targets: []string{"Backlog"}},
		{name: "same column on the live read only", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "Backlog", want: "already in"},
		{name: "cache stale: live column is a pipeline column", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "Implement", want: "(confirmed live)"},
		{name: "live read fails closed", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", liveEr: errors.New("boom"), want: "could not confirm"},
		{name: "live column unknown", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "", want: "no known board column"},
		{name: "only one parking column", item: ovItem{number: 5, status: "Backlog"}, to: "Triage", live: "Backlog",
			mutate: func(e *Engine) { e.cfg.Stages = apiStages() }, want: "no other parking column"},
		{name: "no status field loaded", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "Triage",
			mutate: func(e *Engine) { e.statusField = nil }, want: "not loaded yet"},
		{name: "no board cache", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "Triage",
			mutate: func(e *Engine) { e.readClient = &mockGitHubClient{} }, want: "not loaded yet"},
		{name: "closed item", item: ovItem{number: 5, status: "Triage", closed: true}, to: "Backlog", live: "Triage", want: "is closed"},
		{name: "configured managed Backlog is a pipeline column (source)", item: ovItem{number: 5, status: "Backlog"}, to: "Triage", live: "Backlog",
			mutate: func(e *Engine) { e.stageByName("Backlog").Unmanaged = false }, want: "only moves an item between parking columns"},
		{name: "configured managed Backlog is a pipeline column (destination)", item: ovItem{number: 5, status: "Triage"}, to: "Backlog", live: "Triage",
			mutate: func(e *Engine) { e.stageByName("Backlog").Unmanaged = false }, want: "not a valid parking destination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{}
			if tc.liveEr != nil {
				client.fetchProjectItemStatusFn = func(string) (string, error) { return "", tc.liveEr }
			} else {
				liveStatus(client, tc.live)
			}
			eng, act := parkEngine(t, client, tc.item)
			if tc.mutate != nil {
				tc.mutate(eng)
			}
			issue := tc.issue
			if issue == "" {
				issue = "5"
			}
			_, err := park(act, issue, tc.to)
			pe, st := refusedErr(t, err)
			if !strings.Contains(pe.Message, tc.want) {
				t.Errorf("message %q does not contain %q", pe.Message, tc.want)
			}
			if st.Issue != "owner/repo#5" {
				t.Errorf("refusal state %+v lacks the current state", st)
			}
			if tc.targets != nil && !reflect.DeepEqual(st.ValidTargets, tc.targets) {
				t.Errorf("ValidTargets = %v, want %v", st.ValidTargets, tc.targets)
			}
			if tc.targets == nil && strings.Contains(tc.name, "pipeline source") && len(st.ValidTargets) != 0 {
				t.Errorf("a non-parked source must not be offered targets: %v", st.ValidTargets)
			}
			wantNoWrites(t, client)
		})
	}
}

// SC-002: no pipeline, holding or cleanup column is ever a parking destination,
// and none is accepted as a source, whatever the board holds.
func TestOverseerMoveParked_NeverTouchesPipelineHoldingOrCleanup(t *testing.T) {
	client := &mockGitHubClient{}
	_, act := parkEngine(t, client)
	opts := act.e.statusField.Options
	got := act.parkingDestinations(opts)
	if want := []string{"Triage", "Backlog"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parkingDestinations = %v, want %v", got, want)
	}
	for _, col := range []string{"Specify", "Implement", "Validate", "Queued", "Done"} {
		if why := act.parkedSourceRefusal(col, "x"); why == "" {
			t.Errorf("%s accepted as a parking source", col)
		}
	}
}

func TestOverseerMoveParked_LegacyBacklogWithoutStage(t *testing.T) {
	client := &mockGitHubClient{}
	liveStatus(client, "Triage")
	eng, act := parkEngine(t, client, ovItem{number: 5, status: "Triage"})
	// Triage stays; the Backlog stage is removed -> the legacy literal.
	var ss []*stages.Stage
	for _, s := range eng.cfg.Stages {
		if s.Name != "Backlog" {
			ss = append(ss, s)
		}
	}
	eng.cfg.Stages = ss
	if _, err := park(act, "5", "Backlog"); err != nil {
		t.Fatalf("MoveParked into legacy Backlog: %v", err)
	}
	if got := statusOf(t, eng, 5); got != "Backlog" {
		t.Errorf("status = %q", got)
	}
}

func TestOverseerMoveParked_WriteFailurePostsNoComment(t *testing.T) {
	client := &mockGitHubClient{}
	liveStatus(client, "Triage")
	client.updateProjectItemStatusFn = func(string, string, string, string) error { return errors.New("boom") }
	eng, act := parkEngine(t, client, ovItem{number: 5, status: "Triage"})
	if _, err := park(act, "5", "Backlog"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if n := len(commentBodies(client)); n != 0 {
		t.Errorf("%d comments posted after a failed write", n)
	}
	if got := statusOf(t, eng, 5); got != "Triage" {
		t.Errorf("status = %q, want unchanged", got)
	}
}

func TestOverseerMoveParked_RequiresRequesterAndKnownItem(t *testing.T) {
	client := &mockGitHubClient{}
	_, act := parkEngine(t, client, ovItem{number: 5, status: "Triage"})
	if _, err := act.MoveParked(localapi.MoveParkedParams{Issue: "5", To: "Backlog"}); err == nil {
		t.Error("an unattributed move_parked was accepted")
	}
	if _, err := park(act, "99", "Backlog"); err == nil {
		t.Error("an unknown issue was accepted")
	}
	pr := ovItem{number: 6, status: "Triage", pr: true}
	_, act = parkEngine(t, client, pr)
	if _, err := park(act, "6", "Backlog"); err == nil {
		t.Error("a PR card was accepted")
	}
	wantNoWrites(t, client)
}
