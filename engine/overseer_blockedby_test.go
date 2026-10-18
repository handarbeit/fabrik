package engine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// ovDeepFetch marks item n as deep-fetched with exactly the given edges, the
// state in which its cached BlockedBy can be trusted (ovItem items start as
// probe-seeded minimal records with unknown edges).
func ovDeepFetch(t *testing.T, eng *Engine, n int, edges ...gh.Dependency) {
	t.Helper()
	// A deep fetch always carries labels, so keep the item's current ones.
	snap, ok := eng.store.Peek("owner/repo", n)
	if !ok {
		t.Fatalf("item %d not in store", n)
	}
	st := snap.State()
	eng.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: n, FreshState: gh.ProjectItem{
		Number: n, Repo: "owner/repo", ID: fmt.Sprintf("I_%d", n), Status: st.Status, Labels: st.Labels, BlockedBy: edges,
	}})
}

func blockedByOf(t *testing.T, eng *Engine, n int) []gh.Dependency {
	t.Helper()
	snap, ok := eng.store.Peek("owner/repo", n)
	if !ok {
		t.Fatalf("item %d not in store", n)
	}
	return snap.State().BlockedBy
}

// wantNoBlockedByWrites asserts a refusal or no-op made no GitHub write at all.
func wantNoBlockedByWrites(t *testing.T, c *mockGitHubClient) {
	t.Helper()
	wantNoWrites(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.addBlockedByIssueCalls)+len(c.removeBlockedByIssueCalls) != 0 {
		t.Errorf("expected no blockedBy writes, got add=%v remove=%v", c.addBlockedByIssueCalls, c.removeBlockedByIssueCalls)
	}
}

func dep(n int) gh.Dependency { return gh.Dependency{Number: n, State: "OPEN"} }

func TestOverseerSetBlockedBy_AddAndRemove(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog"}, ovItem{number: 6, status: "Backlog"}, ovItem{number: 7, status: "Backlog"})
	ovDeepFetch(t, eng, 6)
	ovDeepFetch(t, eng, 7)
	ovDeepFetch(t, eng, 5, dep(7)) // fetched same-repo edge: empty Repo

	res, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"6"}, Remove: []string{"owner/repo#7"}})
	if err != nil {
		t.Fatalf("SetBlockedBy: %v", err)
	}
	if !res.Changed || res.AuditComment != localapi.AuditPosted || res.Issue != "owner/repo#5" {
		t.Errorf("result = %+v", res)
	}
	if !reflect.DeepEqual(client.addBlockedByIssueCalls, []addBlockedByIssueCall{{"I_5", "I_6"}}) {
		t.Errorf("add calls = %v", client.addBlockedByIssueCalls)
	}
	if !reflect.DeepEqual(client.removeBlockedByIssueCalls, []addBlockedByIssueCall{{"I_5", "I_7"}}) {
		t.Errorf("remove calls = %v", client.removeBlockedByIssueCalls)
	}
	wantEdges := []localapi.EdgeResult{
		{Blocker: "owner/repo#6", Op: localapi.EdgeAdd, Outcome: localapi.EdgeApplied},
		{Blocker: "owner/repo#7", Op: localapi.EdgeRemove, Outcome: localapi.EdgeApplied},
	}
	if !reflect.DeepEqual(res.Edges, wantEdges) {
		t.Errorf("edges = %+v", res.Edges)
	}
	// Write-through: the cache holds exactly the new edge, with no duplicate or stale one.
	got := blockedByOf(t, eng, 5)
	if len(got) != 1 || got[0].Number != 6 || got[0].Repo != "owner/repo" {
		t.Errorf("cached BlockedBy = %+v, want only owner/repo#6", got)
	}
	// Exactly one audit comment, naming the subscriber and both edges.
	if len(client.addCommentCalls) != 1 {
		t.Fatalf("comments = %d, want 1", len(client.addCommentCalls))
	}
	body := client.addCommentCalls[0].body
	for _, want := range []string{"🏭 **Fabrik — overseer action: set_blocked_by**", "`ovr`", "added owner/repo#6", "removed owner/repo#7"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit comment %q lacks %q", body, want)
		}
	}
	// No label of any kind was written.
	if len(client.addLabelCalls)+len(client.removeLabelCalls) != 0 {
		t.Errorf("label writes: add=%v remove=%v", client.addLabelCalls, client.removeLabelCalls)
	}
}

func TestOverseerSetBlockedBy_CrossRepoAndBareN(t *testing.T) {
	client := &mockGitHubClient{}
	client.fetchProjectItemFn = func(owner, repo string, n int) (*gh.ProjectItem, error) {
		return &gh.ProjectItem{ID: fmt.Sprintf("I_%s_%s_%d", owner, repo, n), Number: n, Repo: owner + "/" + repo, IsClosed: true}, nil
	}
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog"})
	ovDeepFetch(t, eng, 5)
	// A closed blocker in another repo is allowed: it resolves live, and a
	// closed blocker is a leaf for the cycle check.
	res, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"other/lib#42"}})
	if err != nil {
		t.Fatalf("SetBlockedBy: %v", err)
	}
	if res.Edges[0].Blocker != "other/lib#42" {
		t.Errorf("edge = %+v", res.Edges)
	}
	if !reflect.DeepEqual(client.addBlockedByIssueCalls, []addBlockedByIssueCall{{"I_5", "I_other_lib_42"}}) {
		t.Errorf("add calls = %v", client.addBlockedByIssueCalls)
	}
	got := blockedByOf(t, eng, 5)
	if len(got) != 1 || got[0].Repo != "other/lib" || got[0].Number != 42 || got[0].State != "CLOSED" {
		t.Errorf("cached BlockedBy = %+v", got)
	}

	// A bare N means the TARGET's repo, resolved live when it is not cached.
	if _, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"99"}}); err != nil {
		t.Fatalf("bare N: %v", err)
	}
	last := client.fetchProjectItemCalls[len(client.fetchProjectItemCalls)-1]
	if last != (fetchProjectItemCall{"owner", "repo", 99}) {
		t.Errorf("bare N resolved %+v, want owner/repo#99", last)
	}
}

func TestOverseerSetBlockedBy_RepeatIsAllNoopAndSilent(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog"}, ovItem{number: 6, status: "Backlog"})
	ovDeepFetch(t, eng, 6)
	ovDeepFetch(t, eng, 5, dep(6))

	res, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"owner/repo#6"}, Remove: []string{"owner/repo#8"}})
	if err != nil {
		t.Fatalf("SetBlockedBy: %v", err)
	}
	if res.Changed || res.AuditComment != localapi.AuditNone {
		t.Errorf("result = %+v, want unchanged with no audit comment", res)
	}
	for _, e := range res.Edges {
		if e.Outcome != localapi.EdgeNoop || e.Reason == "" {
			t.Errorf("edge %+v, want a no-op with a reason", e)
		}
	}
	wantNoBlockedByWrites(t, client)
	if n := len(client.fetchProjectItemCalls); n != 0 {
		t.Errorf("a no-op resolved %d blockers live; it needs none", n)
	}
}

func TestOverseerSetBlockedBy_Refusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*testing.T, *Engine, *mockGitHubClient)
		items []ovItem
		p     localapi.SetBlockedByParams
		want  string
		notFd bool // the existing resolve() NotFound error rather than a refusal
	}{
		{name: "self block", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"5"}}, want: "cannot block itself"},
		{name: "self block qualified", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"owner/repo#5"}}, want: "cannot block itself"},
		{name: "empty request", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{}, want: "nothing to do"},
		{name: "malformed blocker", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"x/y"}}, want: "bad blocker"},
		{name: "same blocker in both lists", items: []ovItem{{number: 5, status: "Backlog"}, {number: 6, status: "Backlog"}},
			p: localapi.SetBlockedByParams{Add: []string{"6"}, Remove: []string{"owner/repo#6"}}, want: "both add and remove"},
		{name: "closed target", items: []ovItem{{number: 5, status: "Backlog", closed: true}, {number: 6, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}}, want: "is closed"},
		{name: "pull request target", items: []ovItem{{number: 5, status: "Backlog", pr: true}, {number: 6, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}}, notFd: true, want: "pull request"},
		{name: "off-board target", items: []ovItem{{number: 6, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}}, notFd: true, want: "not an item this daemon manages"},
		{name: "unresolvable blocker", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"owner/repo#404"}}, want: "does not resolve to an issue"},
		{name: "blocker lookup fails", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"owner/repo#404"}},
			setup: func(_ *testing.T, _ *Engine, c *mockGitHubClient) {
				c.fetchProjectItemFn = func(string, string, int) (*gh.ProjectItem, error) { return nil, errors.New("boom") }
			}, want: "could not be resolved"},
		{name: "blocker is a pull request", items: []ovItem{{number: 5, status: "Backlog"}, {number: 6, status: "Backlog", pr: true}}, p: localapi.SetBlockedByParams{Add: []string{"6"}}, want: "pull request"},
		{name: "live blocker is a pull request", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"owner/repo#77"}},
			setup: func(_ *testing.T, _ *Engine, c *mockGitHubClient) {
				c.fetchProjectItemFn = func(string, string, int) (*gh.ProjectItem, error) {
					return &gh.ProjectItem{ID: "PR_77", Number: 77, IsPR: true}, nil
				}
			}, want: "pull request"},
		{name: "direct cycle", items: []ovItem{{number: 5, status: "Backlog"}, {number: 6, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}},
			setup: func(t *testing.T, e *Engine, _ *mockGitHubClient) { ovDeepFetch(t, e, 5); ovDeepFetch(t, e, 6, dep(5)) }, want: "would close a cycle"},
		{name: "transitive cycle", items: []ovItem{{number: 5, status: "Backlog"}, {number: 6, status: "Backlog"}, {number: 7, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}},
			setup: func(t *testing.T, e *Engine, _ *mockGitHubClient) {
				ovDeepFetch(t, e, 5)
				ovDeepFetch(t, e, 6, dep(7))
				ovDeepFetch(t, e, 7, dep(5))
			}, want: "owner/repo#6 is blocked by owner/repo#7 is blocked by owner/repo#5"},
		{name: "unknown edge on the walk", items: []ovItem{{number: 5, status: "Backlog"}, {number: 6, status: "Backlog"}, {number: 7, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}},
			setup: func(t *testing.T, e *Engine, _ *mockGitHubClient) {
				ovDeepFetch(t, e, 5)
				ovDeepFetch(t, e, 6, dep(7))
				// #7 was never deep-fetched: its edges are unknown, not "none".
			}, want: "cached blockers of owner/repo#7 are unknown"},
		{name: "unknown edges of the blocker itself", items: []ovItem{{number: 5, status: "Backlog"}, {number: 6, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"6"}},
			setup: func(t *testing.T, e *Engine, _ *mockGitHubClient) { ovDeepFetch(t, e, 5) }, want: "cached blockers of owner/repo#6 are unknown"},
		{name: "uncached open cross-repo blocker", items: []ovItem{{number: 5, status: "Backlog"}}, p: localapi.SetBlockedByParams{Add: []string{"other/lib#3"}},
			setup: func(t *testing.T, e *Engine, c *mockGitHubClient) {
				ovDeepFetch(t, e, 5)
				c.fetchProjectItemFn = func(string, string, int) (*gh.ProjectItem, error) {
					return &gh.ProjectItem{ID: "I_lib3", Number: 3}, nil
				}
			}, want: "cached blockers of other/lib#3 are unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{}
			eng, _, act := overseerEngine(t, client, tc.items...)
			if tc.setup != nil {
				tc.setup(t, eng, client)
			}
			tc.p.Subscriber, tc.p.Issue = "ovr", "5"
			before := blockedByOf2(eng, 5)
			_, err := act.SetBlockedBy(tc.p)
			if tc.notFd {
				var pe *localapi.Error
				if !errors.As(err, &pe) || pe.Code != localapi.CodeNotFound || !strings.Contains(pe.Message, tc.want) {
					t.Fatalf("want a not-found error containing %q, got %v", tc.want, err)
				}
			} else {
				pe, st := refusedErr(t, err)
				if !strings.Contains(pe.Message, tc.want) {
					t.Errorf("message %q does not contain %q", pe.Message, tc.want)
				}
				if st.Issue != "owner/repo#5" {
					t.Errorf("refusal state %+v lacks the current state", st)
				}
			}
			wantNoBlockedByWrites(t, client)
			if after := blockedByOf2(eng, 5); !reflect.DeepEqual(before, after) {
				t.Errorf("a refusal changed the cache: %+v -> %+v", before, after)
			}
		})
	}
}

func blockedByOf2(eng *Engine, n int) []gh.Dependency {
	snap, ok := eng.store.Peek("owner/repo", n)
	if !ok {
		return nil
	}
	return snap.State().BlockedBy
}

// A refusal is judged for every edge before any write: a bad second edge must
// stop a good first one.
func TestOverseerSetBlockedBy_RefusalPreventsPartialChange(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog"}, ovItem{number: 6, status: "Backlog"})
	ovDeepFetch(t, eng, 5)
	ovDeepFetch(t, eng, 6)
	_, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"6", "owner/repo#404"}})
	refusedErr(t, err)
	wantNoBlockedByWrites(t, client)
}

func TestOverseerSetBlockedBy_AuditCommentFailureKeepsEdges(t *testing.T) {
	client := &mockGitHubClient{}
	client.addCommentFn = func(string, string, int, string) (int, error) { return 0, errors.New("comment down") }
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog"}, ovItem{number: 6, status: "Backlog"})
	ovDeepFetch(t, eng, 5)
	ovDeepFetch(t, eng, 6)
	res, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"6"}})
	if err != nil {
		t.Fatalf("SetBlockedBy: %v", err)
	}
	if res.AuditComment != localapi.AuditFailed || !res.Changed {
		t.Errorf("result = %+v, want the edge applied and audit_comment failed", res)
	}
	if got := blockedByOf(t, eng, 5); len(got) != 1 {
		t.Errorf("cached BlockedBy = %+v, want the edge kept", got)
	}
}

func TestOverseerSetBlockedBy_PartialWriteFailure(t *testing.T) {
	client := &mockGitHubClient{}
	client.addBlockedByIssueFn = func(_, blocker string) error {
		if blocker == "I_7" {
			return errors.New("github said no")
		}
		return nil
	}
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog"}, ovItem{number: 6, status: "Backlog"}, ovItem{number: 7, status: "Backlog"}, ovItem{number: 8, status: "Backlog"})
	for _, n := range []int{5, 6, 7, 8} {
		ovDeepFetch(t, eng, n)
	}
	_, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"6", "7", "8"}})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"already applied: added owner/repo#6", "not applied: add owner/repo#7, add owner/repo#8", "audit comment posted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	// The cache holds exactly what was written.
	got := blockedByOf(t, eng, 5)
	if len(got) != 1 || got[0].Number != 6 {
		t.Errorf("cached BlockedBy = %+v, want only #6", got)
	}
	if len(client.addCommentCalls) != 1 || !strings.Contains(client.addCommentCalls[0].body, "PARTIALLY applied") {
		t.Errorf("comments = %+v, want one PARTIALLY applied record", client.addCommentCalls)
	}

	// A first-edge failure writes nothing and posts nothing.
	client2 := &mockGitHubClient{}
	client2.addBlockedByIssueFn = func(string, string) error { return errors.New("down") }
	eng2, _, act2 := overseerEngine(t, client2, ovItem{number: 5, status: "Backlog"}, ovItem{number: 6, status: "Backlog"})
	ovDeepFetch(t, eng2, 5)
	ovDeepFetch(t, eng2, 6)
	if _, err := act2.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"6"}}); err == nil || !strings.Contains(err.Error(), "no writes had landed") {
		t.Errorf("err = %v", err)
	}
	if len(client2.addCommentCalls) != 0 || len(blockedByOf(t, eng2, 5)) != 0 {
		t.Errorf("a failed first write left comments=%v edges=%v", client2.addCommentCalls, blockedByOf(t, eng2, 5))
	}
}

// Removing the last blocker of an item that carries fabrik:blocked records an
// already-due dep-blocked cooldown, and never touches the label.
func TestOverseerSetBlockedBy_RemovalNudgesAdmissionButNeverTheLabel(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _, act := overseerEngine(t, client,
		ovItem{number: 5, status: "Backlog", labels: []string{"fabrik:blocked"}}, ovItem{number: 6, status: "Backlog"}, ovItem{number: 8, status: "Backlog", labels: []string{"fabrik:blocked"}})
	ovDeepFetch(t, eng, 6)
	ovDeepFetch(t, eng, 5, dep(6))
	ovDeepFetch(t, eng, 8)
	// Item 5 is inside a live dep-blocked cooldown.
	eng.store.Apply(itemstate.CooldownRecorded{Repo: "owner/repo", Number: 5, Reason: "dep-blocked", Until: eng.now().Add(time.Hour)})

	if _, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Remove: []string{"6"}}); err != nil {
		t.Fatalf("SetBlockedBy: %v", err)
	}
	snap, _ := eng.store.Peek("owner/repo", 5)
	if c := snap.State().CooldownAt["dep-blocked"]; c.After(eng.now()) {
		t.Errorf("dep-blocked cooldown = %v, want already due", c)
	}
	if !hasLabelStr(snap.State().Labels, "fabrik:blocked") {
		t.Error("the tool removed fabrik:blocked; only checkDependencies may")
	}
	if len(client.addLabelCalls)+len(client.removeLabelCalls) != 0 {
		t.Errorf("label writes: %v %v", client.addLabelCalls, client.removeLabelCalls)
	}

	// An add records no cooldown (checkDependencies reads the write-through cache).
	if _, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "8", Add: []string{"6"}}); err != nil {
		t.Fatalf("SetBlockedBy add: %v", err)
	}
	snap8, _ := eng.store.Peek("owner/repo", 8)
	if c := snap8.State().CooldownAt["dep-blocked"]; !c.IsZero() {
		t.Errorf("an add recorded a dep-blocked cooldown %v", c)
	}
}

// The whole SetBlockedBy path posts only the 🏭-prefixed audit comment, and the
// real predicates ignore it as it is cached after a real action.
func TestOverseerSetBlockedBy_PostedAuditCommentIsNotSteering(t *testing.T) {
	client := &mockGitHubClient{}
	eng, _, act := overseerEngine(t, client, ovItem{number: 5, status: "Backlog", labels: []string{"fabrik:paused", "fabrik:awaiting-input"}}, ovItem{number: 6, status: "Backlog"})
	ovDeepFetch(t, eng, 5)
	ovDeepFetch(t, eng, 6)
	if _, err := act.SetBlockedBy(localapi.SetBlockedByParams{Subscriber: "ovr", Issue: "5", Add: []string{"6"}}); err != nil {
		t.Fatalf("SetBlockedBy: %v", err)
	}
	snap, _ := eng.store.Peek("owner/repo", 5)
	st := snap.State()
	if len(st.Comments) != 1 || !strings.HasPrefix(st.Comments[0].Body, "🏭 **Fabrik") {
		t.Fatalf("comments = %+v", st.Comments)
	}
	item := gh.ProjectItem{Number: 5, Repo: "owner/repo", Labels: st.Labels, Comments: st.Comments}
	if got := eng.findNewComments(item); len(got) != 0 {
		t.Errorf("findNewComments = %v", got)
	}
	client.fetchLabelAppliedAtFn = func(_, _ string, _ int, _ string) (time.Time, error) { return time.Now().Add(-time.Hour), nil }
	if ok, _, _ := eng.resumeAuthorised(item); ok {
		t.Error("the set_blocked_by audit comment resumed a paused item")
	}
	if eng.feedbackGateBlocks(item, true, "test") {
		t.Error("the set_blocked_by audit comment holds the feedback gate")
	}
}

func TestBlockerCycle_PureWalk(t *testing.T) {
	items := map[string]itemstate.ItemState{
		"o/r#2": {Repo: "o/r", Number: 2, LastDeepFetchAt: time.Now(), BlockedBy: []gh.Dependency{{Number: 3}}},
		"o/r#3": {Repo: "o/r", Number: 3, LastDeepFetchAt: time.Now()},
		"o/r#4": {Repo: "o/r", Number: 4, IsClosed: true}, // closed: a leaf, never "unknown"
		"o/r#5": {Repo: "o/r", Number: 5, LastDeepFetchAt: time.Now(), BlockedBy: []gh.Dependency{{Number: 1}, {Number: 6}}},
		"o/r#6": {Repo: "o/r", Number: 6}, // unknown edges
	}
	peek := func(repo string, n int) (itemstate.ItemState, bool) {
		st, ok := items[issueRef(repo, n)]
		return st, ok
	}
	target := blockerRef{"o/r", 1}
	if c, u := blockerCycle(peek, target, blockerRef{"o/r", 2}, false); c != nil || u != nil {
		t.Errorf("clean chain: cycle=%v unknown=%v", c, u)
	}
	if c, u := blockerCycle(peek, target, blockerRef{"o/r", 4}, false); c != nil || u != nil {
		t.Errorf("closed node should be a leaf: cycle=%v unknown=%v", c, u)
	}
	if c, u := blockerCycle(peek, target, blockerRef{"o/x", 9}, true); c != nil || u != nil {
		t.Errorf("a closed start needs no walk: cycle=%v unknown=%v", c, u)
	}
	// #5 reaches the target directly even though its other edge (#6) is unknown:
	// a definite cycle wins.
	if c, u := blockerCycle(peek, target, blockerRef{"o/r", 5}, false); c == nil || u != nil || len(c) != 2 {
		t.Errorf("cycle=%v unknown=%v, want the definite cycle 5 -> 1", c, u)
	}
	if c, u := blockerCycle(peek, target, blockerRef{"o/r", 6}, false); c != nil || u == nil || u.number != 6 {
		t.Errorf("unknown edges: cycle=%v unknown=%v", c, u)
	}
	if c, u := blockerCycle(peek, target, blockerRef{"o/r", 77}, false); c != nil || u == nil || u.number != 77 {
		t.Errorf("absent node: cycle=%v unknown=%v", c, u)
	}
}

func TestParseBlockerRef(t *testing.T) {
	for in, want := range map[string]blockerRef{
		"7": {"a/b", 7}, "#7": {"a/b", 7}, " 7 ": {"a/b", 7}, "c/d#9": {"c/d", 9}, "#c/d#9": {"c/d", 9},
	} {
		got, err := parseBlockerRef(in, "a/b")
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "0", "-1", "c#9", "c/d#x", "c/d#0", "/d#9"} {
		if _, err := parseBlockerRef(in, "a/b"); err == nil {
			t.Errorf("%q parsed; want an error", in)
		}
	}
}
