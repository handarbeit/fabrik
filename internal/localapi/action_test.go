package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

// fakeActor records the params it was called with.
type fakeActor struct {
	promote  PromoteParams
	autonomy SetAutonomyParams
	reval    RevalidateParams
	clear    ClearClaudeLimitParams
	advance  AdvanceParams
	park     MoveParkedParams
	blocked  SetBlockedByParams
	calls    int
}

func (f *fakeActor) Promote(p PromoteParams) (*ActionResult, error) {
	f.calls++
	f.promote = p
	switch p.Issue {
	case "refuse":
		return nil, Refused(RefusalState{Issue: "o/r#1", Status: "Implement", ValidTargets: []string{"Specify"}}, "already in a pipeline column")
	case "panic":
		panic("boom")
	}
	return &ActionResult{Action: MethodPromote, Issue: p.Issue, Changed: true, AuditComment: AuditPosted}, nil
}

func (f *fakeActor) SetAutonomy(p SetAutonomyParams) (*ActionResult, error) {
	f.calls++
	f.autonomy = p
	return &ActionResult{Action: MethodSetAutonomy, Issue: p.Issue}, nil
}

func (f *fakeActor) Revalidate(p RevalidateParams) (*ActionResult, error) {
	f.calls++
	f.reval = p
	return &ActionResult{Action: MethodRevalidate, Issue: p.Issue}, nil
}

func (f *fakeActor) ClearClaudeLimit(p ClearClaudeLimitParams) (*ActionResult, error) {
	f.calls++
	f.clear = p
	return &ActionResult{Action: MethodClearClaudeLimit}, nil
}

func (f *fakeActor) Advance(p AdvanceParams) (*ActionResult, error) {
	f.calls++
	f.advance = p
	return &ActionResult{Action: MethodAdvance, Issue: p.Issue, Changed: true}, nil
}

func (f *fakeActor) MoveParked(p MoveParkedParams) (*ActionResult, error) {
	f.calls++
	f.park = p
	return &ActionResult{Action: MethodMoveParked, Issue: p.Issue, Changed: true}, nil
}

func (f *fakeActor) SetBlockedBy(p SetBlockedByParams) (*ActionResult, error) {
	f.calls++
	f.blocked = p
	return &ActionResult{Action: MethodSetBlockedBy, Issue: p.Issue, Changed: true,
		Edges: []EdgeResult{{Blocker: "o/r#2", Op: EdgeAdd, Outcome: EdgeApplied}}}, nil
}

func startActionServer(t *testing.T, a Actor) *Server {
	t.Helper()
	path := filepath.Join(shortDir(t), "s.sock")
	srv := NewServer(path, echoBackend(), nil)
	srv.Actor = a
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func helloMethods(t *testing.T, srv *Server) []string {
	t.Helper()
	var hello struct {
		Methods []string `json:"methods"`
	}
	if err := Call(context.Background(), srv.Path(), MethodHello, nil, &hello); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return hello.Methods
}

func actionCalls(srv *Server) map[string]any {
	return map[string]any{
		MethodPromote:          PromoteParams{Subscriber: "s", Issue: "o/r#1", To: "Specify"},
		MethodSetAutonomy:      SetAutonomyParams{Subscriber: "s", Issue: "o/r#1", Mode: "cruise"},
		MethodRevalidate:       RevalidateParams{Subscriber: "s", Issue: "o/r#1"},
		MethodClearClaudeLimit: ClearClaudeLimitParams{Subscriber: "s"},
		MethodAdvance:          AdvanceParams{Subscriber: "s", Issue: "o/r#1"},
		MethodMoveParked:       MoveParkedParams{Subscriber: "s", Issue: "o/r#1", To: "Backlog"},
		MethodSetBlockedBy:     SetBlockedByParams{Subscriber: "s", Issue: "o/r#1", Add: []string{"o/r#2"}},
	}
}

// R3: a server with no Actor is the read-only method set: it rejects every
// action method and hello does not advertise them.
func TestReadOnlyServerRejectsActionMethods(t *testing.T) {
	srv := startServer(t, echoBackend())
	methods := helloMethods(t, srv)
	for _, m := range ActionMethods() {
		if slices.Contains(methods, m) {
			t.Errorf("hello advertises %q without an Actor", m)
		}
	}
	for m, params := range actionCalls(srv) {
		err := Call(context.Background(), srv.Path(), m, params, nil)
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != CodeUnknownMethod {
			t.Errorf("%s: want unknown_method, got %v", m, err)
		}
	}
}

func TestActorDispatchAndHello(t *testing.T) {
	fa := &fakeActor{}
	srv := startActionServer(t, fa)
	methods := helloMethods(t, srv)
	for _, m := range ActionMethods() {
		if !slices.Contains(methods, m) {
			t.Errorf("hello omits %q with an Actor", m)
		}
	}
	// The read methods are still advertised and still work.
	if !slices.Contains(methods, MethodStatus) {
		t.Errorf("hello lost status: %v", methods)
	}
	ctx := context.Background()

	var res ActionResult
	if err := Call(ctx, srv.Path(), MethodPromote, PromoteParams{Subscriber: "s1", Issue: "o/r#7", To: "Specify"}, &res); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if res.Action != MethodPromote || !res.Changed || fa.promote != (PromoteParams{Subscriber: "s1", Issue: "o/r#7", To: "Specify"}) {
		t.Errorf("promote result %+v params %+v", res, fa.promote)
	}
	if err := Call(ctx, srv.Path(), MethodSetAutonomy, SetAutonomyParams{Subscriber: "s", Issue: "7", Mode: "yolo"}, &res); err != nil || fa.autonomy.Mode != "yolo" {
		t.Fatalf("set_autonomy: %v %+v", err, fa.autonomy)
	}
	if err := Call(ctx, srv.Path(), MethodRevalidate, RevalidateParams{Subscriber: "s", Issue: "7"}, &res); err != nil || fa.reval.Issue != "7" {
		t.Fatalf("revalidate: %v %+v", err, fa.reval)
	}
	if err := Call(ctx, srv.Path(), MethodClearClaudeLimit, ClearClaudeLimitParams{Subscriber: "s"}, &res); err != nil || fa.clear.Subscriber != "s" {
		t.Fatalf("clear_claude_limit: %v %+v", err, fa.clear)
	}
	if err := Call(ctx, srv.Path(), MethodAdvance, AdvanceParams{Subscriber: "s", Issue: "o/r#7"}, &res); err != nil || fa.advance != (AdvanceParams{Subscriber: "s", Issue: "o/r#7"}) || res.Action != MethodAdvance {
		t.Fatalf("advance: %v %+v %+v", err, fa.advance, res)
	}
	if err := Call(ctx, srv.Path(), MethodMoveParked, MoveParkedParams{Subscriber: "s", Issue: "o/r#7", To: "Backlog"}, &res); err != nil || fa.park != (MoveParkedParams{Subscriber: "s", Issue: "o/r#7", To: "Backlog"}) || res.Action != MethodMoveParked {
		t.Fatalf("move_parked: %v %+v %+v", err, fa.park, res)
	}
	var edges ActionResult
	if err := Call(ctx, srv.Path(), MethodSetBlockedBy, SetBlockedByParams{Subscriber: "s", Issue: "o/r#7", Add: []string{"3"}, Remove: []string{"o/x#4"}}, &edges); err != nil ||
		fa.blocked.Issue != "o/r#7" || len(fa.blocked.Add) != 1 || fa.blocked.Add[0] != "3" || len(fa.blocked.Remove) != 1 || fa.blocked.Remove[0] != "o/x#4" ||
		edges.Action != MethodSetBlockedBy || len(edges.Edges) != 1 || edges.Edges[0].Outcome != EdgeApplied {
		t.Fatalf("set_blocked_by: %v %+v %+v", err, fa.blocked, edges)
	}
}

func TestActionRefusalEnvelopeRoundTrips(t *testing.T) {
	srv := startActionServer(t, &fakeActor{})
	err := Call(context.Background(), srv.Path(), MethodPromote, PromoteParams{Subscriber: "s", Issue: "refuse"}, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeRefused {
		t.Fatalf("want refused, got %v", err)
	}
	var st RefusalState
	if jerr := json.Unmarshal(pe.Data, &st); jerr != nil {
		t.Fatalf("refusal data: %v (%s)", jerr, pe.Data)
	}
	if st.Status != "Implement" || len(st.ValidTargets) != 1 {
		t.Errorf("state = %+v", st)
	}
}

func TestActionPanicIsRecovered(t *testing.T) {
	srv := startActionServer(t, &fakeActor{})
	err := Call(context.Background(), srv.Path(), MethodPromote, PromoteParams{Subscriber: "s", Issue: "panic"}, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeInternal {
		t.Fatalf("panic must become internal, got %v", err)
	}
	// The server survives.
	if err := Call(context.Background(), srv.Path(), MethodHealth, nil, nil); err != nil {
		t.Fatalf("server died after action panic: %v", err)
	}
}

func TestBadActionParams(t *testing.T) {
	fa := &fakeActor{}
	srv := startActionServer(t, fa)
	err := Call(context.Background(), srv.Path(), MethodPromote, []int{1}, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeBadRequest || fa.calls != 0 {
		t.Fatalf("want bad_request without a call, got %v calls=%d", err, fa.calls)
	}
}
