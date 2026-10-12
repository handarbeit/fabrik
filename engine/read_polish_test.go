package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// Tests for the MCP read-surface polish (#2127). Each requirement has a test and
// a neutralised twin (SetReadPolishDisabledForTest) showing the test fails
// without the change.

func boardJSON(t *testing.T, e *Engine, p localapi.BoardParams) (*localapi.BoardResult, map[string]any) {
	t.Helper()
	res, err := e.LocalAPIBackend().Board(p)
	if err != nil {
		t.Fatal(err)
	}
	return res, toJSON(t, res)
}

// seedFlowBoard has open items in Specify and Implement and two closed Done items.
func seedFlowBoard(t *testing.T) *Engine {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify")
	e.seedAPIItem(t, 2, "Implement")
	e.seedAPIItem(t, 3, "Implement", "milestone-less")
	e.seedAPIItem(t, 4, "Done")
	e.seedAPIItem(t, 5, "Done")
	e.store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 4})
	e.store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 5})
	return e
}

func flowSum(r *localapi.BoardResult) int {
	n := 0
	for _, c := range r.Flow {
		n += c.Count
	}
	return n
}

// R1: the columns plus the excluded closed items account for every matched item.
func TestReadPolishR1_FlowReconciles(t *testing.T) {
	e := seedFlowBoard(t)
	res, _ := boardJSON(t, e, localapi.BoardParams{View: localapi.ViewFlow})
	if res.ItemsScanned != 5 || res.Matched != 5 {
		t.Fatalf("scanned/matched = %d/%d, want 5/5", res.ItemsScanned, res.Matched)
	}
	if res.Excluded.Closed != 2 {
		t.Errorf("excluded.closed = %d, want 2", res.Excluded.Closed)
	}
	if got := flowSum(res) + res.Excluded.Closed; got != res.Matched {
		t.Errorf("columns (%d) + excluded.closed (%d) = %d, want matched %d", flowSum(res), res.Excluded.Closed, got, res.Matched)
	}

	// A filtered view reconciles the same way: matched is what passed the filter.
	res, _ = boardJSON(t, e, localapi.BoardParams{View: localapi.ViewFlow, Filter: &localapi.BoardFilter{Column: "Done"}})
	if res.Matched != 2 || res.Excluded.Closed != 2 || len(res.Flow) != 0 {
		t.Errorf("column=Done: matched=%d closed=%d flow=%v, want 2/2/empty", res.Matched, res.Excluded.Closed, res.Flow)
	}
	res, _ = boardJSON(t, e, localapi.BoardParams{View: localapi.ViewFlow, Filter: &localapi.BoardFilter{Column: "Implement"}})
	if res.Matched != 2 || res.Excluded.Closed != 0 || flowSum(res) != 2 {
		t.Errorf("column=Implement: matched=%d closed=%d flow sum=%d, want 2/0/2", res.Matched, res.Excluded.Closed, flowSum(res))
	}
}

func TestReadPolishR1_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R1")()
	e := seedFlowBoard(t)
	res, _ := boardJSON(t, e, localapi.BoardParams{View: localapi.ViewFlow})
	if flowSum(res)+res.Excluded.Closed == res.Matched {
		t.Fatal("neutralised: the flow still reconciles with matched, so the R1 test is vacuous")
	}
}

// R2: status reports every label, a derivable URL and no cycle limits for
// parking or holding columns.
func TestReadPolishR2_StatusShowsEveryLabelAndURL(t *testing.T) {
	e := apiEngine(t, 0)
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_X", Number: 7, Repo: "owner/repo", Title: "labelled bug", Status: "Backlog",
		Labels: []string{"bug", "model:opus", "fabrik:cruise", "stage:Specify:complete"},
	}})
	e.seedAPIItem(t, 8, "Implement")

	res := status(t, e, "7")
	if got := strings.Join(res.Place.OtherLabels, ","); got != "bug,model:opus" {
		t.Errorf("other_labels = %q, want bug,model:opus", got)
	}
	if len(res.Place.StageLabels) != 1 || len(res.Place.FabrikLabels) != 1 {
		t.Errorf("stage/fabrik groupings must stay: %+v", res.Place)
	}
	if !res.URL.Valid || res.URL.V != "https://github.com/owner/repo/issues/7" {
		t.Errorf("url = %+v, want the derived issue URL", res.URL)
	}
	for _, l := range res.Limits {
		if strings.HasPrefix(l.Name, "Backlog:") {
			t.Errorf("a parking column reports cycle limit %q", l.Name)
		}
	}
	var implementLimits int
	for _, l := range status(t, e, "8").Limits {
		if strings.HasPrefix(l.Name, "Implement:") {
			implementLimits++
		}
	}
	if implementLimits == 0 {
		t.Error("a pipeline column must keep its cycle limits")
	}

	// The derived URL honours a GHES host.
	e.cfg.GHESHost = "git.example.com"
	if u := status(t, e, "7").URL; u.V != "https://git.example.com/owner/repo/issues/7" {
		t.Errorf("GHES url = %q", u.V)
	}
}

func TestReadPolishR2_HoldingColumnHasNoLimits(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 9, "Queued")
	for _, l := range status(t, e, "9").Limits {
		if strings.HasPrefix(l.Name, "Queued:") {
			t.Errorf("a holding column reports cycle limit %q", l.Name)
		}
	}
}

func TestReadPolishR2_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R2")()
	e := apiEngine(t, 0)
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_X", Number: 7, Repo: "owner/repo", Title: "labelled bug", Status: "Backlog", Labels: []string{"bug"},
	}})
	res := status(t, e, "7")
	if len(res.Place.OtherLabels) != 0 || res.URL.Valid {
		t.Errorf("neutralised: other_labels=%v url=%+v, want none", res.Place.OtherLabels, res.URL)
	}
	var backlog int
	for _, l := range res.Limits {
		if strings.HasPrefix(l.Name, "Backlog:") {
			backlog++
		}
	}
	if backlog == 0 {
		t.Error("neutralised: the Backlog limits are gone, so the R2 limits test is vacuous")
	}
}

// R3: an attention view always carries an attention list and the matched count.
func TestReadPolishR3_EmptyFilteredAttentionHasKeyAndMatched(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify")
	e.seedAPIItem(t, 2, "Specify")
	f := &localapi.BoardFilter{Column: "Specify", HasOpenBlockers: true}
	for _, n := range []int{1, 2} {
		e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: n, FreshState: gh.ProjectItem{
			ID: "I_" + string(rune('A'+n)), Number: n, Repo: "owner/repo", Status: "Specify",
			BlockedBy: []gh.Dependency{{Number: 90, State: "OPEN"}},
		}})
	}
	res, m := boardJSON(t, e, localapi.BoardParams{View: localapi.ViewAttention, Filter: f})
	att, ok := m["attention"]
	if !ok {
		t.Fatal("an attention view without matches must still carry the attention key")
	}
	if l, isList := att.([]any); !isList || len(l) != 0 {
		t.Errorf("attention = %#v, want []", att)
	}
	if res.Matched != 2 || m["matched"] != float64(2) {
		t.Errorf("matched = %d (%v), want 2: the items passed the filter though none is attention-worthy", res.Matched, m["matched"])
	}

	// The flow view carries no attention list.
	_, fm := boardJSON(t, e, localapi.BoardParams{View: localapi.ViewFlow})
	if _, has := fm["attention"]; has {
		t.Error("the flow view must not carry attention")
	}
	if _, has := fm["matched"]; !has {
		t.Error("the flow view carries matched")
	}
}

func TestReadPolishR3_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R3")()
	defer localapi.SetLegacyAttentionKeyForTest()()
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Specify")
	res, m := boardJSON(t, e, localapi.BoardParams{View: localapi.ViewAttention, Filter: &localapi.BoardFilter{Column: "Specify", HasOpenBlockers: true}})
	if _, has := m["attention"]; has || res.Matched != 0 {
		t.Fatalf("neutralised: attention key present=%v matched=%d, so the R3 test is vacuous", has, res.Matched)
	}
}

// R4: every figure of last_invocation says what it measures.
func TestReadPolishR4_LastInvocationFiguresAreLabelled(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement")
	e.store.Apply(itemstate.InvocationRecorded{
		Repo: "owner/repo", Number: 1, Completed: true,
		Usage:    itemstate.TokenUsage{InputTokens: 10, OutputTokens: 25691, CacheReadTokens: 11491047, CostUSD: 2.98, TurnsUsed: 1},
		Duration: 13 * time.Second,
	})
	m := toJSON(t, status(t, e, "1"))
	li := m["last_invocation"].(map[string]any)
	fb, ok := li["figure_basis"].(map[string]any)
	if !ok {
		t.Fatalf("last_invocation has no figure_basis: %v", li)
	}
	want := map[string]string{
		"duration_seconds": "per-invocation", "turns_used": "per-invocation",
		"tokens": "session-cumulative", "cost_usd": "session-cumulative",
	}
	for k, v := range want {
		if fb[k] != v {
			t.Errorf("figure_basis.%s = %v, want %s", k, fb[k], v)
		}
	}
	if note, _ := fb["note"].(string); !strings.Contains(note, "as reported by the Claude CLI") {
		t.Errorf("the basis must be hedged as the CLI's: %q", note)
	}
	// The recorded figures themselves are unchanged.
	if tok := li["tokens"].(map[string]any); tok["cost_usd"] != 2.98 || tok["cache_read"] != float64(11491047) {
		t.Errorf("figures changed: %v", tok)
	}
}

func TestReadPolishR4_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R4")()
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement")
	e.store.Apply(itemstate.InvocationRecorded{Repo: "owner/repo", Number: 1, Completed: true,
		Usage: itemstate.TokenUsage{CostUSD: 1}, Duration: time.Second})
	if fb := status(t, e, "1").LastInvoke.FigureBasis; fb.Tokens != "" {
		t.Fatalf("neutralised: figure_basis = %+v, so the R4 test is vacuous", fb)
	}
}

// ---- R5 ----

const (
	headSHA = "abc123"
	oldSHA  = "old111"
)

func seedReviewed(t *testing.T, e *Engine, rev gh.PRReview, threads ...gh.Comment) {
	t.Helper()
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_B", Number: 1, Repo: "owner/repo", Title: "item 1", Status: "Validate",
		Labels:         []string{"stage:Validate:complete"},
		LinkedPRNumber: 34, LinkedPRHeadSHA: headSHA,
		LinkedPRReviews: []gh.PRReview{rev}, LinkedPRReviewThreadComments: threads,
	}})
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 1, PRNumber: 34, Title: "pr", State: "open"})
	e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 1, LinkedPRNum: 34, SHA: headSHA})
}

func verdictFor(t *testing.T, e *Engine, reviewer string) localapi.ReviewerVerdict {
	t.Helper()
	snap, ok := e.store.Peek("owner/repo", 1)
	if !ok {
		t.Fatal("item not in the store")
	}
	st := snap.State()
	for _, v := range e.reviewerVerdicts(&st) {
		if v.Reviewer == reviewer {
			return v
		}
	}
	t.Fatalf("no verdict for %s", reviewer)
	return localapi.ReviewerVerdict{}
}

func botThread(id, commit string) gh.Comment {
	return gh.Comment{ID: id, Author: "pruefer[bot]", ReviewThreadID: "T" + id, Path: "a.go", CommitOID: commit, Body: "finding"}
}

func TestReadPolishR5_CleanCommentedPassIsCleanAtHead(t *testing.T) {
	e := apiEngine(t, 0)
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA})
	v := verdictFor(t, e, "pruefer[bot]")
	if !v.AtHead.Valid || !v.AtHead.V || !v.CleanPass.Valid || !v.CleanPass.V || !v.Outstanding.Valid || v.Outstanding.V != 0 {
		t.Errorf("clean pass at head: %+v", v)
	}
	if v.InlineAtHead.V != 0 || v.Unaddressed || v.State != "COMMENTED" {
		t.Errorf("clean pass: %+v", v)
	}
	if v.PRURL != "https://github.com/owner/repo/pull/34" {
		t.Errorf("pr_url = %q", v.PRURL)
	}
}

func TestReadPolishR5_InlineFindingsAtHeadAreNotClean(t *testing.T) {
	e := apiEngine(t, 0)
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA},
		botThread("c1", headSHA), botThread("c2", headSHA))
	v := verdictFor(t, e, "pruefer[bot]")
	if v.InlineAtHead.V != 2 || v.Outstanding.V != 2 {
		t.Errorf("inline/outstanding = %d/%d, want 2/2", v.InlineAtHead.V, v.Outstanding.V)
	}
	if !v.CleanPass.Valid || v.CleanPass.V {
		t.Errorf("findings must not be a clean pass: %+v", v)
	}
	if !v.Unaddressed {
		t.Error("unprocessed findings are unaddressed per the feedback-gate predicate")
	}
	// The same reviewer, same state: only the verdict fields differ from the clean pass.
	if v.State != "COMMENTED" {
		t.Errorf("state = %q", v.State)
	}
}

func TestReadPolishR5_PassOnOlderHeadIsNeverClean(t *testing.T) {
	e := apiEngine(t, 0)
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: oldSHA})
	v := verdictFor(t, e, "pruefer[bot]")
	if !v.AtHead.Valid || v.AtHead.V {
		t.Errorf("at_head = %+v, want false", v.AtHead)
	}
	if !v.CleanPass.Valid || v.CleanPass.V {
		t.Errorf("a pass on an older head must not be reported clean: %+v", v)
	}
}

func TestReadPolishR5_MissingInputsAreUnknownNeverClean(t *testing.T) {
	e := apiEngine(t, 0)
	// A review with no commit id (a webhook that predates the field, say).
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7})
	v := verdictFor(t, e, "pruefer[bot]")
	if v.AtHead.Valid || v.CleanPass.Valid {
		t.Errorf("no commit id: at_head=%+v clean_pass=%+v, want unknown", v.AtHead, v.CleanPass)
	}

	// An engine head and a facts head that disagree: no agreed head.
	e2 := apiEngine(t, 0)
	seedReviewed(t, e2, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA})
	e2.store.Apply(itemstate.PRFactsObserved{Repo: "owner/repo", Number: 1, PRNumber: 34, Facts: itemstate.PRFacts{State: "open", HeadSHA: "different", ObservedAt: time.Now()}})
	v2 := verdictFor(t, e2, "pruefer[bot]")
	if v2.CleanPass.Valid && v2.CleanPass.V {
		t.Errorf("disagreeing heads reported a clean pass: %+v", v2)
	}
	if v2.AtHead.Valid {
		t.Errorf("disagreeing heads: at_head = %+v, want unknown", v2.AtHead)
	}
}

// An actionable review body is an outstanding finding until durably addressed.
func TestReadPolishR5_ReviewBodyIsOutstandingUntilAddressed(t *testing.T) {
	e := apiEngine(t, 0)
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA, Body: "3 findings"})
	v := verdictFor(t, e, "pruefer[bot]")
	if v.Outstanding.V != 1 || v.CleanPass.V {
		t.Errorf("body finding: %+v", v)
	}
}

func TestReadPolishR5_ValidateSettledDistinguishesCleanFromFindings(t *testing.T) {
	settleEvent := func(e *Engine) map[string]string {
		snap, _ := e.store.Peek("owner/repo", 1)
		st := snap.State()
		return e.validateSettledEvent(snap, &st, nil).Meta
	}
	parse := func(t *testing.T, meta map[string]string) localapi.ReviewerVerdict {
		t.Helper()
		var vs []struct {
			Reviewer    string
			CleanPass   any `json:"clean_pass"`
			Outstanding any `json:"outstanding"`
			AtHead      any `json:"at_head"`
			InlineAt    any `json:"inline_at_head"`
		}
		if err := json.Unmarshal([]byte(meta["reviewers"]), &vs); err != nil || len(vs) != 1 {
			t.Fatalf("meta.reviewers = %q (%v)", meta["reviewers"], err)
		}
		v := localapi.ReviewerVerdict{Reviewer: vs[0].Reviewer}
		if b, ok := vs[0].CleanPass.(bool); ok {
			v.CleanPass = localapi.Some(b)
		}
		if n, ok := vs[0].Outstanding.(float64); ok {
			v.Outstanding = localapi.Some(int(n))
		}
		if b, ok := vs[0].AtHead.(bool); ok {
			v.AtHead = localapi.Some(b)
		}
		return v
	}

	clean := apiEngine(t, 0)
	seedReviewed(t, clean, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA})
	findings := apiEngine(t, 0)
	seedReviewed(t, findings, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA}, botThread("c1", headSHA))
	older := apiEngine(t, 0)
	seedReviewed(t, older, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: oldSHA})

	cm, fm, om := settleEvent(clean), settleEvent(findings), settleEvent(older)
	if cm["reviews"] != fm["reviews"] || cm["reviews"] != om["reviews"] || cm["reviews"] != "pruefer[bot]: COMMENTED" {
		t.Fatalf("the reviews string must be unchanged and identical: %q / %q / %q", cm["reviews"], fm["reviews"], om["reviews"])
	}
	if v := parse(t, cm); !v.CleanPass.V || !v.CleanPass.Valid {
		t.Errorf("clean COMMENTED review: %+v", v)
	}
	if v := parse(t, fm); v.CleanPass.V || v.Outstanding.V != 1 {
		t.Errorf("COMMENTED review with an inline finding: %+v", v)
	}
	if v := parse(t, om); !v.CleanPass.Valid || v.CleanPass.V || !v.AtHead.Valid || v.AtHead.V {
		t.Errorf("a pass on an older head: %+v", v)
	}
}

func TestReadPolishR5_FabrikStatusCarriesReviewers(t *testing.T) {
	e := apiEngine(t, 0)
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA})
	pr := status(t, e, "1").PR
	if len(pr.Reviewers) != 1 || pr.Reviewers[0].Reviewer != "pruefer[bot]" || !pr.URL.Valid {
		t.Errorf("pr.reviewers/url = %+v / %+v", pr.Reviewers, pr.URL)
	}
}

func TestReadPolishR5_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R5")()
	e := apiEngine(t, 0)
	seedReviewed(t, e, gh.PRReview{Author: "pruefer[bot]", State: "COMMENTED", DatabaseID: 7, CommitID: headSHA})
	snap, _ := e.store.Peek("owner/repo", 1)
	st := snap.State()
	if ev := e.validateSettledEvent(snap, &st, nil); ev.Meta["reviewers"] != "" {
		t.Fatalf("neutralised: reviewers still present: %q", ev.Meta["reviewers"])
	}
}

// ---- R7 ----

func seedGreen(t *testing.T, e *Engine) {
	t.Helper()
	e.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: headSHA, Run: gh.CheckRun{ID: 1, Name: "Test and vet", Status: "completed", Conclusion: "success"}})
}

func setFlakeRerun(e *Engine, head string, checks ...string) {
	e.flakeRerunMu.Lock()
	defer e.flakeRerunMu.Unlock()
	if e.flakeReruns == nil {
		e.flakeReruns = map[string]*prFlakeState{}
	}
	e.flakeReruns[startupWatchKey("owner/repo", 34)] = &prFlakeState{headSHA: head, r1Done: true, rerunChecks: checks, touched: e.now()}
}

func settleMeta(e *Engine) map[string]string {
	snap, _ := e.store.Peek("owner/repo", 1)
	st := snap.State()
	return e.validateSettledEvent(snap, &st, nil).Meta
}

func TestReadPolishR7_GreenAfterRerunCarriesCIRerun(t *testing.T) {
	e := apiEngine(t, 0)
	seedValidate(t, e, 1, "stage:Validate:complete")
	seedGreen(t, e)

	if m := settleMeta(e); m["ci"] != "green" || m["ci_rerun"] != "" {
		t.Fatalf("no re-run: ci=%q ci_rerun=%q, want green and no attribute", m["ci"], m["ci_rerun"])
	}
	setFlakeRerun(e, headSHA, "Test and vet")
	if m := settleMeta(e); m["ci"] != "green" || m["ci_rerun"] != "Test and vet" {
		t.Errorf("after a re-run: ci=%q ci_rerun=%q", m["ci"], m["ci_rerun"])
	}
	// A re-run recorded for another head says nothing about this one.
	setFlakeRerun(e, "other999", "Test and vet")
	if m := settleMeta(e); m["ci_rerun"] != "" {
		t.Errorf("re-run on another head leaked: %q", m["ci_rerun"])
	}
}

func TestReadPolishR7_NotGreenCarriesNone(t *testing.T) {
	e := apiEngine(t, 0)
	seedValidate(t, e, 1, "stage:Validate:complete")
	e.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: headSHA, Run: gh.CheckRun{ID: 1, Name: "Test and vet", Status: "completed", Conclusion: "failure"}})
	setFlakeRerun(e, headSHA, "Test and vet")
	if m := settleMeta(e); m["ci"] == "green" || m["ci_rerun"] != "" {
		t.Errorf("ci=%q ci_rerun=%q: the attribute is for a green verdict only", m["ci"], m["ci_rerun"])
	}
}

func TestReadPolishR7_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R7")()
	e := apiEngine(t, 0)
	seedValidate(t, e, 1, "stage:Validate:complete")
	seedGreen(t, e)
	setFlakeRerun(e, headSHA, "Test and vet")
	if m := settleMeta(e); m["ci_rerun"] != "" {
		t.Fatalf("neutralised: ci_rerun = %q, so the R7 test is vacuous", m["ci_rerun"])
	}
}

// The gate records the names of the checks it re-ran, and only for a re-run
// that was actually made.
func TestReadPolishR7_GateRecordsRerunCheckNames(t *testing.T) {
	f := newFlakeFixture(t)
	if f.poll(t) {
		t.Fatal("first failure dispatched; want a re-run")
	}
	if got := f.eng.flakeRerunChecks("owner/repo", 43, f.head); len(got) != 1 || got[0] != "test" {
		t.Errorf("re-run checks = %v, want [test]", got)
	}
	if got := f.eng.flakeRerunChecks("owner/repo", 43, "otherhead"); got != nil {
		t.Errorf("another head: %v", got)
	}

	refused := newFlakeFixture(t)
	refused.mu.Lock()
	refused.rerunErr = gh.ErrForbidden
	refused.mu.Unlock()
	refused.poll(t)
	if got := refused.eng.flakeRerunChecks("owner/repo", 43, refused.head); got != nil {
		t.Errorf("a refused re-run was recorded as made: %v", got)
	}
}

// ---- R8 ----

func TestReadPolishR8_EveryItemScopedPushCarriesTitle(t *testing.T) {
	e, sink := channelEngine(t, 0, nil)
	e.seedAPIItem(t, 1, "Implement")
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "I_Z", Number: 2, Repo: "owner/repo", Title: "Fix `the`\nthing", Status: "Implement"}})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 2, Label: "fabrik:paused"})
	var ev channelevents.Event
	for i := 0; i < 20; i++ {
		ev = sink.next(t)
		if ev.Issue == 2 && ev.Type == channelevents.LabelApplied {
			break
		}
	}
	if ev.Issue != 2 || ev.Meta["title"] != "Fix the thing" {
		t.Fatalf("label push: issue=%d meta.title=%q, want %q", ev.Issue, ev.Meta["title"], "Fix the thing")
	}

	// The other builders share the one choke point.
	snap, _ := e.store.Peek("owner/repo", 1)
	st := snap.State()
	for _, typ := range []channelevents.EventType{channelevents.ValidateSettled, channelevents.ItemAdded, channelevents.Merged} {
		if got := e.baseEvent(&st, typ).Meta["title"]; got != st.Title {
			t.Errorf("%s: title = %q, want %q", typ, got, st.Title)
		}
	}
	if got := e.itemAddedEvent(&st).Meta["title"]; got != st.Title {
		t.Errorf("item-added title = %q", got)
	}
}

func TestReadPolishR8_TitleIsSanitisedAndCapped(t *testing.T) {
	if got := pushTitle("  a\r\n`b`\t c  "); got != "a b c" {
		t.Errorf("pushTitle = %q", got)
	}
	long := strings.Repeat("é", 500)
	got := pushTitle(long)
	if n := len([]rune(got)); n != maxPushTitleRunes || !strings.HasSuffix(got, "…") {
		t.Errorf("capped title has %d runes: %q", n, got[len(got)-4:])
	}
	if pushTitle("") != "" {
		t.Error("an empty title adds no meta")
	}
}

func TestReadPolishR8_Neutralised(t *testing.T) {
	defer SetReadPolishDisabledForTest("R8")()
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement")
	snap, _ := e.store.Peek("owner/repo", 1)
	st := snap.State()
	if _, has := e.baseEvent(&st, channelevents.Merged).Meta["title"]; has {
		t.Fatal("neutralised: title still present, so the R8 test is vacuous")
	}
}

// The verdict builder is called from the channel and local-API paths, whose
// scans forbid any GitHub read; it must stay a function of the Store alone.
func TestReadPolishVerdictBuilderReachesNoGitHub(t *testing.T) {
	src, err := os.ReadFile("review_verdict.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"e.client", "e.readClient", "store.Get(", "labelAppliedAt(", "FetchProjectItem",
		"FetchPRReviews", "feedbackGateBlocks", "unprocessedFeedback("} {
		if strings.Contains(string(src), banned) {
			t.Errorf("review_verdict.go mentions %q: the verdicts must be built from cached state only", banned)
		}
	}
}
