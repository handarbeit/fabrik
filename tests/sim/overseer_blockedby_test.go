package sim

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/internal/localapi"
)

// The overseer's set_blocked_by action (#2385, ADR 2385) against the real
// engine. Needs BoardCache: the action reads the Store, which only the board
// cache keeps populated for parked items.
//
// The cycle guard trusts only cached edges, and a parked card is never deep
// fetched (#778), so its edges are unknown: a parked item can be the TARGET of
// an add (its own edges are not walked) but a parked BLOCKER is refused until
// the daemon has fetched it. These scenarios therefore keep the blockers in a
// pipeline column (Specify, non-yolo, so the stage runs once and the item then
// sits complete, deep fetched) and park only the dependent.

func blockerEdgeWrites(env *Env) (adds, removes int) {
	log := env.Sim.Log()
	return len(log.ByMethod("AddBlockedByIssue")), len(log.ByMethod("RemoveBlockedByIssue"))
}

func statusBlockers(t *testing.T, env *Env, num int) []string {
	t.Helper()
	st, err := env.Engine.LocalAPIBackend().Status(localapi.StatusParams{Issue: strconv.Itoa(num)})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var out []string
	for _, b := range st.Blockers.Items {
		out = append(out, b.Issue)
	}
	return out
}

func TestOverseerSetBlockedByHoldsThenReleases(t *testing.T) {
	t.Parallel()
	yolo := false
	env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true, Yolo: &yolo})
	// The cache path admits items through the reactive mayNeedWork observer.
	t.Cleanup(env.Engine.RegisterObservers())
	blocker := FileIssue(t, env, "blocker", "must land first", "Specify")
	dependent := FileIssue(t, env, "dependent", "waits for the blocker", "Backlog")
	WaitForIssueLabel(t, env, blocker, "stage:Specify:complete", 20) // the blocker is deep fetched; the dependent is parked
	act := env.Engine.OverseerActorForTest()
	ref := func(n int) string { return env.OwnerRepo + "#" + strconv.Itoa(n) }

	// Order the work BEFORE the item gets a pipeline column.
	res, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "orderer", Issue: ref(dependent), Add: []string{strconv.Itoa(blocker)}})
	if err != nil {
		t.Fatalf("SetBlockedBy add: %v", err)
	}
	if !res.Changed || res.AuditComment != localapi.AuditPosted || len(res.Edges) != 1 || res.Edges[0].Outcome != localapi.EdgeApplied {
		t.Fatalf("add result = %+v", res)
	}
	// GitHub has the edge, and fabrik_status shows it with no refetch in between.
	if bb := projectItem(t, env, dependent).BlockedBy; len(bb) != 1 || bb[0].Number != blocker {
		t.Fatalf("GitHub blockedBy = %+v", bb)
	}
	if got := statusBlockers(t, env, dependent); len(got) != 1 || got[0] != ref(blocker) {
		t.Fatalf("fabrik_status blockers = %v, want [%s]", got, ref(blocker))
	}
	audits := auditComments(t, env, dependent)
	if len(audits) != 1 || !strings.HasPrefix(audits[0], "🏭 **Fabrik — overseer action: set_blocked_by**") || !strings.Contains(audits[0], "`orderer`") || !strings.Contains(audits[0], "added "+ref(blocker)) {
		t.Fatalf("audit comments = %q", audits)
	}
	if hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("the tool applied fabrik:blocked itself")
	}

	// Now give it a pipeline column: checkDependencies holds it.
	env.Sim.Sim().SeedProjectItem(env.Owner, env.ProjectNum, env.OwnerRepo, dependent, false, "Specify")
	WaitForIssueLabel(t, env, dependent, "fabrik:blocked", 20)
	RunPolls(t, env, 2)
	labels := IssueLabels(t, env, dependent)
	if !hasLabel(labels, "fabrik:blocked") {
		t.Fatalf("dependent is not held after the edge was set: labels %v", labels)
	}
	if hasLabel(labels, "stage:Specify:complete") {
		t.Fatalf("Specify ran while the dependent was blocked: labels %v", labels)
	}

	// Remove the edge: fabrik_status shows it gone at once ...
	res, err = act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "orderer", Issue: ref(dependent), Remove: []string{ref(blocker)}})
	if err != nil {
		t.Fatalf("SetBlockedBy remove: %v", err)
	}
	if !res.Changed || res.Edges[0].Outcome != localapi.EdgeApplied {
		t.Fatalf("remove result = %+v", res)
	}
	if got := statusBlockers(t, env, dependent); len(got) != 0 {
		t.Fatalf("fabrik_status blockers = %v after removal, want none", got)
	}
	if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("the tool removed fabrik:blocked itself; only the engine's dependency check may")
	}
	// ... and the engine's own path releases the item on a following poll,
	// without waiting out the dependency recheck interval (the admission nudge).
	WaitForLabelAbsent(t, env, dependent, "fabrik:blocked", 5)
	WaitForIssueLabel(t, env, dependent, "stage:Specify:complete", 10)
	if n := len(auditComments(t, env, dependent)); n != 2 {
		t.Errorf("audit comments = %d, want one per call (2)", n)
	}
}

func TestOverseerSetBlockedByRepeatAndRefusalsWriteNothing(t *testing.T) {
	t.Parallel()
	yolo := false
	env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true, Yolo: &yolo})
	t.Cleanup(env.Engine.RegisterObservers())
	a := FileIssue(t, env, "a", "first", "Specify")
	b := FileIssue(t, env, "b", "second", "Specify")
	parked := FileIssue(t, env, "parked", "never fetched", "Backlog")
	WaitForIssueLabel(t, env, a, "stage:Specify:complete", 20)
	WaitForIssueLabel(t, env, b, "stage:Specify:complete", 20)
	act := env.Engine.OverseerActorForTest()
	ref := func(n int) string { return env.OwnerRepo + "#" + strconv.Itoa(n) }

	if _, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "orderer", Issue: ref(b), Add: []string{ref(a)}}); err != nil {
		t.Fatalf("add: %v", err)
	}
	adds, removes := blockerEdgeWrites(env)
	comments := len(auditComments(t, env, a)) + len(auditComments(t, env, b))

	// A repeat is all no-op, writes nothing and posts nothing.
	res, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "orderer", Issue: ref(b), Add: []string{ref(a)}, Remove: []string{ref(1000)}})
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if res.Changed || res.AuditComment != localapi.AuditNone {
		t.Fatalf("repeat result = %+v", res)
	}

	refusals := []struct {
		name string
		p    localapi.SetBlockedByParams
		want string
	}{
		{"self block", localapi.SetBlockedByParams{Issue: ref(a), Add: []string{ref(a)}}, "cannot block itself"},
		{"cycle", localapi.SetBlockedByParams{Issue: ref(a), Add: []string{ref(b)}}, "would close a cycle"},
		{"unresolvable blocker", localapi.SetBlockedByParams{Issue: ref(a), Add: []string{env.OwnerRepo + "#9999"}}, "does not resolve to an issue"},
		// A parked card is never deep fetched, so its edges are unknown, not none.
		{"unknown edges of a parked blocker", localapi.SetBlockedByParams{Issue: ref(a), Add: []string{ref(parked)}}, "are unknown"},
		{"removing from a parked item", localapi.SetBlockedByParams{Issue: ref(parked), Remove: []string{ref(a)}}, "are unknown"},
	}
	for _, tc := range refusals {
		tc.p.Subscriber = "orderer"
		_, err := act.SetBlockedBy(tc.p)
		var pe *localapi.Error
		if !errors.As(err, &pe) || pe.Code != localapi.CodeRefused || !strings.Contains(pe.Message, tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	if na, nr := blockerEdgeWrites(env); na != adds || nr != removes {
		t.Errorf("refusals/no-ops wrote edges: adds %d -> %d, removes %d -> %d", adds, na, removes, nr)
	}
	if n := len(auditComments(t, env, a)) + len(auditComments(t, env, b)); n != comments {
		t.Errorf("refusals/no-ops posted audit comments (%d -> %d)", comments, n)
	}
	if bb := projectItem(t, env, b).BlockedBy; len(bb) != 1 || bb[0].Number != a {
		t.Errorf("GitHub blockedBy of b = %+v", bb)
	}
}
