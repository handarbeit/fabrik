package engine

import (
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/attention"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

func (e *Engine) deepWithBlockers(number int, deps ...gh.Dependency) {
	snap, _ := e.store.Peek("owner/repo", number)
	st := snap.State()
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: number, FreshState: gh.ProjectItem{
		ID: "I_" + string(rune('A'+number)), Number: number, Repo: "owner/repo", BlockedBy: deps,
		Status: st.Status, Labels: st.Labels,
	}})
}

func open(n int) gh.Dependency { return gh.Dependency{Number: n, State: "OPEN"} }

func (e *Engine) classifyItem(t *testing.T, number int) attention.Result {
	t.Helper()
	snap, ok := e.store.Peek("owner/repo", number)
	if !ok {
		t.Fatalf("#%d not cached", number)
	}
	st := snap.State()
	now := e.now()
	return attention.Classify(e.attentionInput(&st, now, 30*time.Minute, time.Time{}, e.peekBlockerLookup()))
}

func TestAttentionInput_SpecifyCompleteIsSuspendedWithGitHubAge(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify", "stage:Specify:complete")
	// The poll-side recorder (decision_anchor.go) has put the GitHub time in the map.
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 1, Label: "stage:Specify:complete", At: e.now().Add(-68 * 24 * time.Hour)})

	res := e.classifyItem(t, 1)
	if res.State != attention.Suspended || res.Code != attention.CodeDecisionPending {
		t.Fatalf("got %s/%s: %s", res.State, res.Code, res.Summary)
	}
	if res.Pending.NextStage != "Implement" || res.Pending.SinceBasis != attention.SinceGitHub {
		t.Fatalf("pending = %+v", res.Pending)
	}
	if res.ProgressAge < 67*24*time.Hour {
		t.Errorf("age = %s, want ~68 days", res.ProgressAge)
	}

	// Control: with cruise the engine advances it itself.
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 1, Label: "fabrik:cruise"})
	if res := e.classifyItem(t, 1); res.State == attention.Suspended {
		t.Fatalf("a cruise item is suspended")
	}
}

func TestAttentionInput_FirstBoardAfterRestartListsTheItem(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify", "stage:Specify:complete")
	e.seedAPIItem(t, 2, "Specify", "fabrik:paused", "fabrik:awaiting-input")
	res, err := e.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewAttention})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attention) != 2 {
		t.Fatalf("attention = %+v, want both items on the first served board", res.Attention)
	}
	// needs-human sorts ahead of suspended within the shared rank.
	if res.Attention[0].State != "needs-human" || res.Attention[1].State != "suspended" {
		t.Errorf("order = %s, %s", res.Attention[0].State, res.Attention[1].State)
	}
	p := res.Attention[1].Pending
	if p == nil || p.SinceBasis != attention.SinceFirstSeen || p.AgeSeconds.Valid || !p.AtLeastAgeSeconds.Valid {
		t.Errorf("an unknown GitHub time must be an at-least bound, never an age: %+v", p)
	}
	if len(res.Attention[1].Options) == 0 || res.Attention[1].Options[0].Tool != "fabrik_advance" {
		t.Errorf("options = %+v", res.Attention[1].Options)
	}
}

func TestResolveParkedBlockers(t *testing.T) {
	e := apiEngine(t, 0)
	// #1 blocked by #2 (Backlog); #3 blocked by #1 (chain); #4 blocked by #5 in
	// an active column; #6 blocked by #7 in Backlog and #5 (mixed).
	e.seedAPIItem(t, 1, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 2, "Backlog")
	e.seedAPIItem(t, 3, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 4, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 5, "Implement")
	e.seedAPIItem(t, 6, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 7, "Backlog")
	e.deepWithBlockers(1, open(2))
	e.deepWithBlockers(3, open(1))
	e.deepWithBlockers(4, open(5))
	e.deepWithBlockers(5)
	e.deepWithBlockers(6, open(7), open(5))

	cases := []struct {
		n      int
		only   bool
		leaves []string
	}{
		{1, true, []string{"owner/repo#2"}},
		{3, true, []string{"owner/repo#2"}}, // transitive
		{4, false, nil},                     // active-column blocker stays waiting
		{6, false, nil},                     // any non-parked blocker fails closed
	}
	for _, tc := range cases {
		snap, _ := e.store.Peek("owner/repo", tc.n)
		st := snap.State()
		leaves, only := e.resolveParkedBlockers(&st, e.peekBlockerLookup())
		if only != tc.only {
			t.Errorf("#%d: only=%v, want %v", tc.n, only, tc.only)
		}
		var refs []string
		for _, l := range leaves {
			refs = append(refs, l.Ref)
		}
		if strings.Join(refs, ",") != strings.Join(tc.leaves, ",") {
			t.Errorf("#%d: leaves=%v, want %v", tc.n, refs, tc.leaves)
		}
	}
	// And through the classifier.
	if res := e.classifyItem(t, 1); res.State != attention.Suspended || res.Code != attention.CodeParkedBlocker {
		t.Errorf("#1 = %s/%s", res.State, res.Code)
	}
	if res := e.classifyItem(t, 4); res.State != attention.Waiting {
		t.Errorf("#4 = %s, want waiting", res.State)
	}

	// The scan-built index must agree with the Peek-built one.
	snap, _ := e.store.Peek("owner/repo", 3)
	st := snap.State()
	if _, only := e.resolveParkedBlockers(&st, e.scanBlockerIndex()); !only {
		t.Errorf("scan index disagrees with peek lookup")
	}
}

func TestResolveParkedBlockers_FailsClosed(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 2, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 3, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 4, "Backlog")

	// Cycle: #2 <-> #3, never reaching a parked item.
	e.deepWithBlockers(2, open(3))
	e.deepWithBlockers(3, open(2))
	// Cross-repo blocker the store does not hold.
	e.deepWithBlockers(1, gh.Dependency{Number: 99, State: "OPEN", Repo: "other/repo"})

	for _, n := range []int{1, 2} {
		snap, _ := e.store.Peek("owner/repo", n)
		st := snap.State()
		if _, only := e.resolveParkedBlockers(&st, e.peekBlockerLookup()); only {
			t.Errorf("#%d resolved as parked; an unknown blocker or a cycle must fail closed", n)
		}
	}

	// Not yet deep-fetched: BlockedBy is unknown.
	e.seedAPIItem(t, 5, "Specify", "fabrik:blocked")
	snap, _ := e.store.Peek("owner/repo", 5)
	st := snap.State()
	if _, only := e.resolveParkedBlockers(&st, e.peekBlockerLookup()); only {
		t.Errorf("an un-deep-fetched item must not resolve")
	}

	// An un-deep-fetched active-column blocker cannot vouch for its own blockers.
	e.seedAPIItem(t, 6, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 7, "Implement")
	e.deepWithBlockers(6, open(7))
	snap, _ = e.store.Peek("owner/repo", 6)
	st = snap.State()
	if _, only := e.resolveParkedBlockers(&st, e.peekBlockerLookup()); only {
		t.Errorf("an un-deep-fetched active blocker must fail closed")
	}
}

// TestParkedBlockerAgeStartsWhenTheBlockerIsParked is the review scenario for R3
// case (b) end to end, on a sim clock set well before real time: an item blocked
// for 30 days by an active-column issue, whose blocker is moved to Backlog now.
// No label on the item changes, so the wait must start at the move, not at the
// 30-day-old fabrik:blocked label, and the stamps must come from the sim clock
// (with time.Now() they would land in the classifier's future).
func TestParkedBlockerAgeStartsWhenTheBlockerIsParked(t *testing.T) {
	e := apiEngine(t, 0)
	clk := &adjClock{t: time.Now().Add(-100 * time.Hour)}
	e.SetClock(clk)

	e.seedAPIItem(t, 6, "Specify", "fabrik:blocked")
	e.seedAPIItem(t, 7, "Implement")
	e.deepWithBlockers(6, open(7))
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 6, Label: "fabrik:blocked", At: clk.Now().Add(-30 * 24 * time.Hour)})
	if res := e.classifyItem(t, 6); res.State == attention.Suspended {
		t.Fatalf("blocked by an active-column issue must not be suspended: %s", res.Code)
	}

	clk.set(clk.Now().Add(20 * time.Minute))
	e.store.Apply(itemstate.LocalStatusUpdated{Repo: "owner/repo", Number: 7, NewStatus: "Backlog"})
	clk.set(clk.Now().Add(5 * time.Minute))

	res := e.classifyItem(t, 6)
	if res.State != attention.Suspended || res.Code != attention.CodeParkedBlocker {
		t.Fatalf("got %s/%s", res.State, res.Code)
	}
	if res.Pending.SinceBasis != attention.SinceBlockerParked || res.ProgressAge != 5*time.Minute {
		t.Fatalf("basis %s age %s, want blocker-parked / 5m (not the 30-day label age, never negative)", res.Pending.SinceBasis, res.ProgressAge)
	}

	// Control: a daemon that never watched the move (a restart after it) can only
	// say "at least since it first saw this", never the label's 30 days.
	r := apiEngine(t, 0)
	r.SetClock(&adjClock{t: time.Now().Add(-100 * time.Hour)})
	r.seedAPIItem(t, 6, "Specify", "fabrik:blocked")
	r.seedAPIItem(t, 7, "Backlog")
	r.deepWithBlockers(6, open(7))
	r.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 6, Label: "fabrik:blocked", At: r.now().Add(-30 * 24 * time.Hour)})
	res = r.classifyItem(t, 6)
	if res.Pending == nil || res.Pending.SinceBasis != attention.SinceFirstSeen || res.Pending.SinceExact() {
		t.Fatalf("after a restart the wait is a lower bound: %+v", res.Pending)
	}
}

func TestResumeMethodOf(t *testing.T) {
	mk := func(bodies ...string) *itemstate.ItemState {
		st := &itemstate.ItemState{}
		for i, b := range bodies {
			st.Comments = append(st.Comments, gh.Comment{DatabaseID: i + 1, Body: b})
		}
		return st
	}
	if got := resumeMethodOf(mk("🏭 **Fabrik — paused by a daemon clean stop**\n\nStage **Plan** was interrupted. Remove `fabrik:paused` to resume.")); got != attention.ResumeRemoveLabel {
		t.Errorf("clean-stop comment = %q", got)
	}
	// A later question supersedes the earlier pause comment.
	if got := resumeMethodOf(mk("🏭 **Fabrik — stage failed**\n\nthen remove the `fabrik:paused` label.", "🏭 **Fabrik — stage: Plan**\n\nWhich option do you prefer?")); got != attention.ResumeUnknown {
		t.Errorf("superseded = %q", got)
	}
	if got := resumeMethodOf(mk("just a human comment")); got != attention.ResumeUnknown {
		t.Errorf("no fabrik comment = %q", got)
	}
}

// TestCleanStopPauseResumeMatchesEngineComment pins R-options-2 against the
// engine's own text: the resume method the classifier reports for a clean-stop
// pause is the one the comment the engine posts actually says.
func TestCleanStopPauseResumeMatchesEngineComment(t *testing.T) {
	e := apiEngine(t, 0)
	body := cleanStopPauseComment(t)
	e.seedAPIItem(t, 1, "Specify", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{{DatabaseID: 11, Body: body}},
	}})
	res := e.classifyItem(t, 1)
	if !strings.Contains(res.Next.Action, "remove fabrik:paused") || strings.Contains(res.Next.Action, "human comment") {
		t.Fatalf("next action %q contradicts the engine comment %q", res.Next.Action, body)
	}
	if len(res.Options) != 1 || !strings.Contains(res.Options[0].Action, "remove the fabrik:paused label") {
		t.Fatalf("options = %+v", res.Options)
	}
	// Control: an awaiting-input question pause says a comment resumes it.
	e.seedAPIItem(t, 2, "Specify", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 2, FreshState: gh.ProjectItem{
		ID: "I_C", Number: 2, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{{DatabaseID: 12, Body: "🏭 **Fabrik — stage: Specify**\n\nWhich database should we use?"}},
	}})
	res = e.classifyItem(t, 2)
	if !strings.Contains(res.Next.Action, "human comment") {
		t.Fatalf("awaiting-input pause: %q", res.Next.Action)
	}
}

// TestResumeMethodSurvivesLaterFabrikComments pins the review finding on
// R-options-2: a Fabrik comment that is not a pause comment, posted after the
// clean-stop pause (an overseer audit comment from fabrik_set_autonomy, say),
// must not shadow the pause comment's own resume method.
func TestResumeMethodSurvivesLaterFabrikComments(t *testing.T) {
	pause := cleanStopPauseComment(t)
	audit := overseerAuditBody("set_autonomy", "set autonomy to cruise on owner/repo#1", "ops-session")
	info := "🏭 **Fabrik** — opened PR #99"

	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Specify", Labels: []string{"fabrik:paused", "fabrik:awaiting-input"},
		Comments: []gh.Comment{{DatabaseID: 11, Body: pause}, {DatabaseID: 12, Body: audit}, {DatabaseID: 13, Body: info}},
	}})
	res := e.classifyItem(t, 1)
	if res.ResumeVia != attention.ResumeViaLabel || !strings.Contains(res.Next.Action, "remove fabrik:paused") || strings.Contains(res.Next.Action, "human comment") {
		t.Fatalf("a later audit/info comment shadowed the pause comment: via=%q action=%q", res.ResumeVia, res.Next.Action)
	}
	if len(res.Options) != 1 || !strings.Contains(res.Options[0].Action, "remove the fabrik:paused label") {
		t.Fatalf("options = %+v", res.Options)
	}

	mk := func(bodies ...string) *itemstate.ItemState {
		st := &itemstate.ItemState{}
		for i, b := range bodies {
			st.Comments = append(st.Comments, gh.Comment{DatabaseID: i + 1, Body: b})
		}
		return st
	}
	// A later stage comment (an awaiting-input question) still supersedes the
	// older pause comment: that is a different pause.
	if got := resumeMethodOf(mk(pause, audit, "🏭 **Fabrik — stage: Plan**\n\nWhich option do you prefer?")); got != attention.ResumeUnknown {
		t.Errorf("a later question must supersede the older pause comment, got %q", got)
	}
	// A pause that asks for a reply is a comment-resume pause, also past an audit.
	if got := resumeMethodOf(mk("🏭 **Fabrik — push refused**\n\nInspect the branch, and once it is right, comment on this issue to resume.", audit)); got != attention.ResumeComment {
		t.Errorf("comment-resume pause = %q", got)
	}
}

// cleanStopPauseComment returns the comment the engine itself posts when a
// daemon clean stop pauses an in-flight item, captured by running the real
// shutdown path against a mock client (R-options-2 pins the classifier's resume
// method against this text, not against a copy of it).
func cleanStopPauseComment(t *testing.T) string {
	t.Helper()
	client := &mockGitHubClient{}
	eng := testEngine(t, client, &mockClaudeInvoker{})
	seedInFlightIssue(eng, "owner/repo", 30, "Validate")
	eng.wg.Add(1)
	eng.runShutdownPause(eng.inFlightSnapshot())
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, c := range client.addCommentCalls {
		if c.issueNumber == 30 && strings.Contains(c.body, "daemon clean stop") {
			return c.body
		}
	}
	t.Fatal("the shutdown path posted no clean-stop comment")
	return ""
}
