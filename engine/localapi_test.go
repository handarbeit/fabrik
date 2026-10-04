package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// noGitHubClient is a GitHubClient with no implementation behind it: calling
// any method panics on the nil embedded interface. A test that reaches GitHub
// through the engine's client therefore fails loudly (R7).
type noGitHubClient struct{ GitHubClient }

// countingFetcher is a Store FallbackFetcher that fails the test's zero-call
// assertion if it is ever invoked.
type countingFetcher struct{ calls atomic.Int32 }

func (f *countingFetcher) FetchItem(string, int) (gh.ProjectItem, error) {
	f.calls.Add(1)
	return gh.ProjectItem{}, os.ErrNotExist
}

func apiStages() []*stages.Stage {
	return []*stages.Stage{
		{Name: "Specify", Order: 1},
		{Name: "Implement", Order: 2, MaxTurns: 50},
		{Name: "Validate", Order: 3},
		{Name: "Queued", Order: 4, HoldingStage: true},
		{Name: "Backlog", Order: 5, Unmanaged: true},
		{Name: "Done", Order: 6, CleanupWorktree: true},
	}
}

// apiEngine builds a test engine whose clock is `ahead` past real time (the
// store stamps StatusEnteredAt with real time.Now(), so a positive `ahead`
// ages every seeded item by that much).
func apiEngine(t *testing.T, ahead time.Duration) *Engine {
	t.Helper()
	e := NewWithDeps(Config{
		Owner:                 "owner",
		Repo:                  "repo",
		ProjectNum:            1,
		Version:               "v-test",
		MaxConcurrent:         3,
		MaxRetries:            3,
		MaxReviewCycles:       5,
		MaxCiFixCycles:        5,
		MaxRebaseCycles:       3,
		MaxToolsDeniedRetries: 3,
		CIWaitTimeout:         30 * time.Minute,
		CIBackstopTimeout:     4 * time.Hour,
		ReviewWaitTimeout:     time.Hour,
		StallThreshold:        30 * time.Minute,
		Stages:                apiStages(),
	}, &noGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	e.SetClock(fixedClock{time.Now().Add(ahead)})
	e.health.markStarted(time.Now().Add(-time.Hour))
	return e
}

func (e *Engine) seedAPIItem(t *testing.T, number int, status string, labels ...string) {
	t.Helper()
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{
		ID: "I_" + string(rune('A'+number)), Number: number, Repo: "owner/repo",
		Title: "item " + string(rune('0'+number)), URL: "https://github.com/owner/repo/issues/" + string(rune('0'+number)),
		Status: status, Labels: labels,
	}})
}

func (e *Engine) deepFetched(number int) {
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: number, FreshState: gh.ProjectItem{
		ID: "I_" + string(rune('A'+number)), Number: number, Repo: "owner/repo",
	}})
}

func status(t *testing.T, e *Engine, issue string) *localapi.StatusResult {
	t.Helper()
	res, err := e.LocalAPIBackend().Status(localapi.StatusParams{Issue: issue})
	if err != nil {
		t.Fatalf("Status(%s): %v", issue, err)
	}
	return res
}

// toJSON round-trips a result so assertions see exactly what the wire carries.
func toJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLocalAPIStatusPausedStates(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input", "stage:Implement:in_progress")
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.EnginePaused{Repo: "owner/repo", Number: 2, StageName: "Implement"})

	if got := status(t, e, "1").Attention; got.State != "needs-human" || got.Code != "fabrik:awaiting-input" {
		t.Errorf("question pause: %+v", got)
	}
	got := status(t, e, "owner/repo#2").Attention
	if got.State != "escalated" || got.Code != "paused-by-engine" {
		t.Errorf("engine pause: %+v", got)
	}
	if got.Next.Action == "" || len(got.Reasons) == 0 || got.Summary == "" {
		t.Errorf("attention must always explain itself: %+v", got)
	}
}

func TestLocalAPIStatusAwaitingCIDeadline(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 3, "Implement", "fabrik:awaiting-ci")
	applied := time.Now().Add(-10 * time.Minute)
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 3, Label: "fabrik:awaiting-ci", At: applied})

	a := status(t, e, "3").Attention
	if a.State != "waiting" || a.Code != "fabrik:awaiting-ci" {
		t.Fatalf("awaiting-ci: %+v", a)
	}
	var backstop, liveness *localapi.DeadlineInfo
	for i := range a.Next.Deadlines {
		switch a.Next.Deadlines[i].Kind {
		case "ci-backstop-timeout":
			backstop = &a.Next.Deadlines[i]
		case "ci-liveness-timeout":
			liveness = &a.Next.Deadlines[i]
		}
	}
	if backstop == nil || !backstop.At.Valid || !backstop.At.V.Equal(applied.Add(4*time.Hour).UTC()) {
		t.Errorf("backstop deadline = %+v, want applied+4h", backstop)
	}
	if liveness == nil || liveness.At.Valid {
		t.Errorf("liveness deadline has no CI progress cached and must be unknown, got %+v", liveness)
	}
	if !a.Next.At.Valid || !a.Next.At.V.Equal(applied.Add(4*time.Hour).UTC()) {
		t.Errorf("next.at should be the earliest KNOWN deadline, got %+v", a.Next.At)
	}

	// The same item after a restart: LabelAppliedAt is gone, so every deadline
	// derived from it is unknown, and next.at is unknown — never "now".
	e2 := apiEngine(t, 0)
	e2.seedAPIItem(t, 3, "Implement", "fabrik:awaiting-ci")
	m := toJSON(t, status(t, e2, "3"))
	next := m["attention"].(map[string]any)["next"].(map[string]any)
	if next["at"] != "unknown" {
		t.Errorf("post-restart next.at = %v, want \"unknown\"", next["at"])
	}
	for _, d := range next["deadlines"].([]any) {
		if d.(map[string]any)["at"] != "unknown" {
			t.Errorf("post-restart deadline should be unknown: %v", d)
		}
	}
}

func TestLocalAPIStatusLimitsAndNearCycleLimit(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 4, "Implement")
	for i := 0; i < 4; i++ {
		e.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 4, StageName: "Implement"})
	}
	e.store.Apply(itemstate.StageRetryIncremented{Repo: "owner/repo", Number: 4, StageName: "Implement"})
	e.store.Apply(itemstate.EnqueueCycleIncremented{Repo: "owner/repo", Number: 4, StageName: "Implement"})
	limits := map[string]localapi.Limit{}
	for _, l := range status(t, e, "4").Limits {
		limits[l.Name] = l
	}
	if l := limits["Implement:review-cycles"]; l.N != 4 || l.Max.Kind != "limit" || l.Max.Value != 5 {
		t.Errorf("review cycles = %+v, want 4 of 5", l)
	}
	if l := limits["Implement:attempts"]; l.N != 1 || l.Max.Value != 3 {
		t.Errorf("attempts = %+v, want 1 of 3", l)
	}
	if l, ok := limits["Implement:enqueue-cycles"]; !ok || l.N != 1 || l.Max.Kind != "unknown" {
		t.Errorf("an unconfigured limit must read unknown, never 0: %+v ok=%v", l, ok)
	}
	if _, ok := limits["Implement:slice-retries"]; ok {
		t.Errorf("a zero counter with no known limit is not reported")
	}
	// Not yet at the limit: not escalated.
	if st := status(t, e, "4").Attention.State; st == "escalated" {
		t.Errorf("4 of 5 must not be escalated")
	}

	// At the limit and paused: escalated, naming the counter.
	e.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: 4, StageName: "Implement"})
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 4, Label: "fabrik:paused"})
	a := status(t, e, "4").Attention
	if a.State != "escalated" || a.Code != "limit:review-cycles" {
		t.Errorf("at limit + paused: %+v", a)
	}
}

func TestLocalAPIStatusWorkerIdleAndUnknownFields(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 5, "Implement")
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 5, StageName: "Implement", StartedAt: time.Now()})
	st := status(t, e, "5")
	if !st.Worker.InFlight || st.Worker.Stage != "Implement" {
		t.Fatalf("worker: %+v", st.Worker)
	}
	if st.Worker.TurnsUsed.Valid {
		t.Errorf("turns for an in-flight worker must be unknown")
	}
	if !st.Worker.MaxTurns.Valid || st.Worker.MaxTurns.V != 50 {
		t.Errorf("max_turns = %+v, want 50 from the stage", st.Worker.MaxTurns)
	}
	if st.Attention.State != "working" {
		t.Errorf("attention: %+v", st.Attention)
	}

	e.seedAPIItem(t, 6, "Implement")
	idle := status(t, e, "6")
	if idle.Attention.State != "idle" {
		t.Errorf("idle item: %+v", idle.Attention)
	}
	m := toJSON(t, idle)
	for _, f := range []string{"as_of", "last_poll_attempt_at", "last_poll_success_at", "since_last_poll_success_seconds", "daemon_uptime_seconds"} {
		if _, ok := m[f]; !ok {
			t.Errorf("envelope field %q missing from status", f)
		}
	}
	if m["as_of"] == "" || m["as_of"] == nil {
		t.Error("as_of must be set")
	}
	// Absent from the cache => the literal "unknown".
	if m["milestone"] != "unknown" {
		t.Errorf("uncaptured milestone = %v, want \"unknown\"", m["milestone"])
	}
	if m["last_poll_success_at"] != "unknown" {
		t.Errorf("no poll has run: last_poll_success_at = %v, want \"unknown\"", m["last_poll_success_at"])
	}
	if c := m["cache"].(map[string]any); c["last_deep_fetch_at"] != "unknown" || c["age_seconds"] != "unknown" {
		t.Errorf("never deep-fetched => cache age unknown, got %v", c)
	}
	pr := m["pr"].(map[string]any)
	if pr["state"] != "unknown" || pr["mergeable"] != "unknown" || pr["ci"] != "unknown" {
		t.Errorf("un-hydrated PR fields must be unknown: %v", pr)
	}
	if m["blockers"].(map[string]any)["state"] != "unknown" {
		t.Errorf("un-hydrated blockers must be unknown: %v", m["blockers"])
	}
	if lc := m["last_invocation"].(map[string]any); lc["outcome"] != "unknown" || lc["tokens"] != "unknown" {
		t.Errorf("no invocation recorded: %v", lc)
	}
	if m["attention"].(map[string]any)["latest_fabrik_comment"] != "unknown" {
		t.Errorf("no comments cached => latest comment unknown")
	}
}

func TestLocalAPIStatusMilestoneNoneVsUnknown(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 7, "Implement")
	e.store.Apply(itemstate.IssueMilestoneUpdated{Repo: "owner/repo", Number: 7, Milestone: &gh.Milestone{Title: "v1", Number: 2}})
	if ms := status(t, e, "7").Milestone; ms.State != "set" || ms.Title != "v1" || ms.Number != 2 {
		t.Errorf("milestone = %+v", ms)
	}
	e.store.Apply(itemstate.IssueMilestoneUpdated{Repo: "owner/repo", Number: 7, Milestone: nil})
	if ms := toJSON(t, status(t, e, "7"))["milestone"]; ms != "none" {
		t.Errorf("cleared milestone = %v, want \"none\"", ms)
	}
}

func TestLocalAPIStatusPRBlockersAndComment(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 8, "Validate")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 8, FreshState: gh.ProjectItem{
		ID: "I_8", Number: 8, Repo: "owner/repo", Status: "Validate", URL: "https://github.com/owner/repo/issues/8",
		LinkedPRNumber: 20,
		BlockedBy:      []gh.Dependency{{Number: 3, State: "OPEN"}, {Number: 4, State: "CLOSED", Repo: "o/other"}},
		Comments: []gh.Comment{
			{DatabaseID: 11, Author: "a", Body: "hi"},
			{DatabaseID: 12, Author: "bot", Body: "🏭 **Fabrik — stage: Plan**\nx"},
			{DatabaseID: 13, Author: "a", Body: "thanks"},
		},
	}})
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 8, PRNumber: 20, State: "open"})
	e.store.Apply(itemstate.PRHeadSHAUpdated{Repo: "owner/repo", Number: 8, SHA: "abc"})
	e.store.Apply(itemstate.CheckRunCompleted{Repo: "owner/repo", SHA: "abc", Run: gh.CheckRun{ID: 1, Name: "ci", Status: "completed", Conclusion: "failure"}})

	st := status(t, e, "8")
	if st.PR.State != "linked" || !st.PR.Number.Valid || st.PR.Number.V != 20 {
		t.Fatalf("pr: %+v", st.PR)
	}
	if !st.PR.CI.Valid || st.PR.CI.V != "red" {
		t.Errorf("ci = %+v, want red", st.PR.CI)
	}
	if st.Blockers.State != "known" || len(st.Blockers.Items) != 2 ||
		st.Blockers.Items[0].Issue != "owner/repo#3" || st.Blockers.Items[0].State.V != "OPEN" ||
		st.Blockers.Items[1].Issue != "o/other#4" {
		t.Errorf("blockers: %+v", st.Blockers)
	}
	if lc := st.Attention.LatestFabrikComment; !lc.Valid || lc.V != "https://github.com/owner/repo/issues/8#issuecomment-12" {
		t.Errorf("latest fabrik comment = %+v", lc)
	}
}

func TestLocalAPIStatusMergeTrainPosition(t *testing.T) {
	e := apiEngine(t, 0)
	for _, n := range []int{10, 11, 12} {
		e.seedAPIItem(t, n, "Queued")
		e.deepFetched(n)
	}
	// 12 targets another base; 11 entered later than 10.
	e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: 12, Label: "base:release"})
	for _, n := range []int{10, 11, 12} {
		e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: n, FreshState: gh.ProjectItem{
			ID: "I_" + string(rune('A'+n)), Number: n, Repo: "owner/repo", Status: "Queued", LinkedPRNumber: 30 + n,
			Labels: func() []string {
				if n == 12 {
					return []string{"base:release"}
				}
				return nil
			}(),
		}})
	}
	e.store.EnterRepoWorker(mergeTrainKey("owner/repo", defaultPartitionBase))

	tp := status(t, e, "11").PR.MergeTrain
	if tp == nil || tp.Of != 2 || tp.Position < 1 || tp.Position > 2 || !tp.WorkerInFlight || tp.Partition != "owner/repo" {
		t.Fatalf("default partition position = %+v", tp)
	}
	tp = status(t, e, "12").PR.MergeTrain
	if tp == nil || tp.Of != 1 || tp.Position != 1 || tp.WorkerInFlight || tp.Partition != "owner/repo:release" {
		t.Fatalf("base:release partition position = %+v", tp)
	}
}

func TestLocalAPIStatusNotFoundAndRefParsing(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement")
	b := e.LocalAPIBackend()
	for _, tc := range []struct {
		ref  string
		code string
	}{
		{"99", localapi.CodeNotFound},
		{"owner/repo#99", localapi.CodeNotFound},
		{"", localapi.CodeBadRequest},
		{"nope", localapi.CodeBadRequest},
		{"owner/repo#x", localapi.CodeBadRequest},
	} {
		_, err := b.Status(localapi.StatusParams{Issue: tc.ref})
		pe, ok := err.(*localapi.Error)
		if !ok || pe.Code != tc.code {
			t.Errorf("Status(%q) err = %v, want code %s", tc.ref, err, tc.code)
		}
	}
	if _, err := b.Status(localapi.StatusParams{Issue: "#1"}); err != nil {
		t.Errorf("#1 should resolve on a single-repo daemon: %v", err)
	}

	// A multi-repo daemon (no configured repo) with two cached repos: bare N is ambiguous.
	m := NewWithDeps(Config{Stages: apiStages(), StallThreshold: time.Minute}, &noGitHubClient{}, &mockClaudeInvoker{}, nil)
	m.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "a", Number: 1, Repo: "o/a", Status: "Implement"}})
	m.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "b", Number: 1, Repo: "o/b", Status: "Implement"}})
	_, err := m.LocalAPIBackend().Status(localapi.StatusParams{Issue: "1"})
	if pe, ok := err.(*localapi.Error); !ok || pe.Code != localapi.CodeAmbiguous {
		t.Errorf("bare N on a 2-repo daemon: %v", err)
	}
	if _, err := m.LocalAPIBackend().Status(localapi.StatusParams{Issue: "o/b#1"}); err != nil {
		t.Errorf("qualified ref: %v", err)
	}
}

// A daemon with a configured default repo that also carries items from another
// repo manages two repos: a bare N must be ambiguous, never silently resolved
// to the default repo.
func TestLocalAPIBareNAmbiguousWhenDefaultRepoPlusOtherRepoInStore(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 12, "Implement")
	e.store.Apply(itemstate.IssueOpened{Item: gh.ProjectItem{ID: "x", Number: 12, Repo: "other/y", Status: "Implement"}})
	b := e.LocalAPIBackend()
	_, err := b.Status(localapi.StatusParams{Issue: "12"})
	if pe, ok := err.(*localapi.Error); !ok || pe.Code != localapi.CodeAmbiguous {
		t.Fatalf("bare N with a default repo plus another cached repo: err = %v, want %s", err, localapi.CodeAmbiguous)
	}
	if _, err := b.Status(localapi.StatusParams{Issue: "other/y#12"}); err != nil {
		t.Errorf("qualified ref to the non-default repo: %v", err)
	}
}

func TestLocalAPIBoardAttentionViewRanksStalledNotRecentWaiting(t *testing.T) {
	// The clock is 2h ahead, so every item's StatusEnteredAt is 2h old.
	e := apiEngine(t, 2*time.Hour)
	// needs-human, escalated, stalled (no labels, nothing recent) ...
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused", "fabrik:awaiting-input")
	e.store.Apply(itemstate.EnginePaused{Repo: "owner/repo", Number: 2, StageName: "Implement"})
	e.seedAPIItem(t, 3, "Implement")
	// ... a recently active waiting item and a recently active idle one ...
	e.seedAPIItem(t, 4, "Implement", "fabrik:awaiting-ci")
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 4, Label: "fabrik:awaiting-ci", At: time.Now().Add(2*time.Hour - 5*time.Minute)})
	e.seedAPIItem(t, 5, "Implement")
	e.store.Apply(itemstate.StageAttempted{Repo: "owner/repo", Number: 5, StageName: "Implement", At: time.Now().Add(2*time.Hour - time.Minute)})
	// ... and a settled-at-Validate cruise item.
	e.seedAPIItem(t, 6, "Validate", "fabrik:cruise", "stage:Validate:complete")
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 6, FreshState: gh.ProjectItem{
		ID: "I_6", Number: 6, Repo: "owner/repo", Status: "Validate", LinkedPRNumber: 60,
		Labels: []string{"fabrik:cruise", "stage:Validate:complete"},
	}})
	e.store.Apply(itemstate.PRDetailsUpdated{Repo: "owner/repo", Number: 6, PRNumber: 60, State: "open"})
	e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: 6, Label: "stage:Validate:complete", At: time.Now().Add(2*time.Hour - time.Minute)})

	res, err := e.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewAttention})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, a := range res.Attention {
		order = append(order, a.Issue+"="+a.State+"/"+a.Code)
	}
	want := []string{
		"owner/repo#1=needs-human/fabrik:awaiting-input",
		"owner/repo#2=escalated/paused-by-engine",
		"owner/repo#3=stalled/no-progress",
		"owner/repo#6=needs-human/awaiting-merge-decision",
	}
	if strings.Join(order, "\n") != strings.Join(want, "\n") {
		t.Fatalf("attention view order:\n%s\nwant:\n%s", strings.Join(order, "\n"), strings.Join(want, "\n"))
	}
	if res.StallThresholdSeconds != 1800 {
		t.Errorf("stall threshold = %d, want 1800 and reported", res.StallThresholdSeconds)
	}
	for _, a := range res.Attention {
		if a.Reason == "" {
			t.Errorf("%s has no one-line reason", a.Issue)
		}
	}

	// A per-request override changes what counts as stalled and is echoed.
	res, _ = e.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewAttention, StallThresholdSeconds: 6 * 3600})
	if res.StallThresholdSeconds != 6*3600 {
		t.Errorf("override not echoed: %d", res.StallThresholdSeconds)
	}
	for _, a := range res.Attention {
		if a.State == "stalled" {
			t.Errorf("nothing is 6h stale, but %s is stalled", a.Issue)
		}
	}
}

func TestLocalAPIBoardFilters(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement", "fabrik:paused", "bug")
	e.seedAPIItem(t, 2, "Implement", "fabrik:paused")
	e.seedAPIItem(t, 3, "Specify", "fabrik:paused", "bug")
	e.seedAPIItem(t, 4, "Implement", "fabrik:paused") // milestone never captured
	e.store.Apply(itemstate.IssueMilestoneUpdated{Repo: "owner/repo", Number: 1, Milestone: &gh.Milestone{Title: "v1", Number: 1}})
	e.store.Apply(itemstate.IssueMilestoneUpdated{Repo: "owner/repo", Number: 2, Milestone: nil})
	e.store.Apply(itemstate.IssueMilestoneUpdated{Repo: "owner/repo", Number: 3, Milestone: &gh.Milestone{Title: "v1", Number: 1}})

	issues := func(f localapi.BoardFilter) (got []string, r *localapi.BoardResult) {
		t.Helper()
		r, err := e.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewAttention, Filter: &f})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range r.Attention {
			got = append(got, a.Issue)
		}
		return got, r
	}
	join := func(s []string) string { return strings.Join(s, ",") }

	if got, r := issues(localapi.BoardFilter{Milestone: "v1"}); join(got) != "owner/repo#1,owner/repo#3" || r.Excluded.UnknownMilestone != 1 {
		t.Errorf("milestone v1 = %v excluded=%+v (the uncaptured item must be excluded and counted, not matched)", got, r.Excluded)
	}
	if got, _ := issues(localapi.BoardFilter{Milestone: "none"}); join(got) != "owner/repo#2" {
		t.Errorf("milestone none = %v (unknown must never read as none)", got)
	}
	if got, _ := issues(localapi.BoardFilter{Label: "bug"}); join(got) != "owner/repo#1,owner/repo#3" {
		t.Errorf("label bug = %v", got)
	}
	if got, _ := issues(localapi.BoardFilter{Column: "Specify"}); join(got) != "owner/repo#3" {
		t.Errorf("column Specify = %v", got)
	}
	if got, _ := issues(localapi.BoardFilter{Repo: "owner/repo", Label: "bug", Milestone: "v1"}); join(got) != "owner/repo#1,owner/repo#3" {
		t.Errorf("combined = %v", got)
	}
	if got, _ := issues(localapi.BoardFilter{Repo: "other/repo"}); len(got) != 0 {
		t.Errorf("other repo = %v", got)
	}

	// has_open_blockers: needs a deep fetch to be evaluable.
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 2, FreshState: gh.ProjectItem{
		ID: "I_2", Number: 2, Repo: "owner/repo", Status: "Implement", Labels: []string{"fabrik:paused"},
		BlockedBy: []gh.Dependency{{Number: 9, State: "OPEN"}},
	}})
	got, r := issues(localapi.BoardFilter{HasOpenBlockers: true})
	if join(got) != "owner/repo#2" || r.Excluded.NotDeepFetched != 3 {
		t.Errorf("has_open_blockers = %v excluded=%+v", got, r.Excluded)
	}
}

func TestLocalAPIBoardFlowView(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement")
	e.seedAPIItem(t, 2, "Implement")
	e.seedAPIItem(t, 3, "Specify")
	e.seedAPIItem(t, 4, "Done")
	e.store.Apply(itemstate.IssueClosed{Repo: "owner/repo", Number: 4})
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 2, StageName: "Implement", StartedAt: time.Now()})

	res, err := e.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewFlow})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Attention) != 0 {
		t.Errorf("flow view must not carry the attention list")
	}
	cols := map[string]localapi.ColumnFlow{}
	var names []string
	for _, c := range res.Flow {
		cols[c.Column] = c
		names = append(names, c.Column)
	}
	if strings.Join(names, ",") != "Specify,Implement" {
		t.Errorf("columns (closed items skipped, stage order) = %v", names)
	}
	impl := cols["Implement"]
	if impl.Count != 2 || len(impl.InFlightWorkers) != 1 || impl.InFlightWorkers[0] != "owner/repo#2" || !impl.OldestInColumn.Valid {
		t.Errorf("Implement column = %+v", impl)
	}
	if !strings.HasPrefix(res.StatusEnteredBasis, "daemon-observed") {
		t.Errorf("time-in-column basis must be disclosed: %q", res.StatusEnteredBasis)
	}
}

func TestLocalAPIHealth(t *testing.T) {
	e := apiEngine(t, 0)
	now := e.now()
	e.health.notePollAttempt(now.Add(-30 * time.Second))
	e.health.notePollSuccess(now.Add(-45 * time.Second))
	e.health.noteReconcileOK(now.Add(-2 * time.Minute))
	e.health.noteWebhookEvent(now.Add(-10 * time.Second))
	e.health.setGraphQLBackoff(true, 0.15, 750)
	e.claudeSuspendMu.Lock()
	e.claudeSuspendedUntil = now.Add(3 * time.Hour)
	e.claudeSuspendMu.Unlock()
	e.seedAPIItem(t, 1, "Queued")
	e.deepFetched(1)
	e.store.Apply(itemstate.ItemDeepFetched{Repo: "owner/repo", Number: 1, FreshState: gh.ProjectItem{ID: "I_B", Number: 1, Repo: "owner/repo", Status: "Queued"}})
	e.store.Apply(itemstate.WorkerEntered{Repo: "owner/repo", Number: 1, StageName: "Queued", StartedAt: now})
	e.store.EnterRepoWorker("owner/repo:release")

	h, err := e.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "v-test" || h.DaemonUptimeSeconds < 3590 {
		t.Errorf("version/uptime = %q/%d", h.Version, h.DaemonUptimeSeconds)
	}
	if !h.Poll.SinceAttemptSeconds.Valid || h.Poll.SinceAttemptSeconds.V != 30 || h.Poll.SinceSuccessSeconds.V != 45 {
		t.Errorf("poll = %+v", h.Poll)
	}
	if h.Envelope.SinceLastPollSuccessSeconds.V != 45 {
		t.Errorf("envelope since success = %+v", h.Envelope.SinceLastPollSuccessSeconds)
	}
	if h.Reconcile.SinceSuccessSeconds.V != 120 || h.Webhook.SinceEventSeconds.V != 10 || h.Webhook.Mode != "off" {
		t.Errorf("reconcile/webhook = %+v / %+v", h.Reconcile, h.Webhook)
	}
	if !h.Claude.Suspended || !h.Claude.SuspendedUntil.Valid || !h.Claude.SuspendedUntil.V.Equal(now.Add(3*time.Hour).UTC()) {
		t.Errorf("claude = %+v", h.Claude)
	}
	if b := h.Backoff; !b.Observed || !b.Low.V || b.Remaining.V != 750 || b.RestPaused.V {
		t.Errorf("backoff = %+v", b)
	}
	if h.Workers.InUse != 1 || !h.Workers.MaxConcurrent.Valid || h.Workers.MaxConcurrent.V != 3 {
		t.Errorf("workers = %+v", h.Workers)
	}
	if len(h.MergeTrain) != 2 {
		t.Fatalf("partitions = %+v", h.MergeTrain)
	}
	def, rel := h.MergeTrain[0], h.MergeTrain[1]
	if def.Base != "" || len(def.Members) != 1 || def.WorkerInFlight || rel.Base != "release" || !rel.WorkerInFlight || len(rel.Members) != 0 {
		t.Errorf("partitions = %+v", h.MergeTrain)
	}

	// Fresh daemon: nothing recorded => unknown, not a healthy default.
	fresh := apiEngine(t, 0)
	m := toJSON(t, mustHealth(t, fresh))
	if m["last_poll_success_at"] != "unknown" || m["since_last_poll_success_seconds"] != "unknown" {
		t.Errorf("fresh envelope = %v", m)
	}
	poll := m["poll"].(map[string]any)
	if poll["last_success_at"] != "unknown" || poll["since_success_seconds"] != "unknown" {
		t.Errorf("fresh poll = %v", poll)
	}
	if bo := m["graphql_backoff"].(map[string]any); bo["observed"] != false || bo["remaining"] != "unknown" || bo["low"] != "unknown" {
		t.Errorf("unobserved backoff must be unknown: %v", bo)
	}
}

func mustHealth(t *testing.T, e *Engine) *localapi.HealthResult {
	t.Helper()
	h, err := e.LocalAPIBackend().Health(localapi.HealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Every tool, through the real socket server, carries as_of.
func TestLocalAPIServedOverSocketCarriesAsOf(t *testing.T) {
	e := apiEngine(t, 0)
	e.seedAPIItem(t, 1, "Implement")
	dir, err := os.MkdirTemp("/tmp", "fl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	srv := localapi.NewServer(filepath.Join(dir, "s.sock"), e.LocalAPIBackend(), nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ctx := context.Background()
	for _, tc := range []struct {
		method string
		params any
	}{
		{localapi.MethodStatus, localapi.StatusParams{Issue: "1"}},
		{localapi.MethodBoard, localapi.BoardParams{View: localapi.ViewAttention}},
		{localapi.MethodBoard, localapi.BoardParams{View: localapi.ViewFlow}},
		{localapi.MethodHealth, nil},
	} {
		var raw map[string]any
		if err := localapi.Call(ctx, srv.Path(), tc.method, tc.params, &raw); err != nil {
			t.Fatalf("%s: %v", tc.method, err)
		}
		if s, _ := raw["as_of"].(string); s == "" {
			t.Errorf("%s response has no as_of: %v", tc.method, raw)
		}
		if _, ok := raw["daemon_uptime_seconds"]; !ok {
			t.Errorf("%s response has no envelope", tc.method)
		}
	}
}

// R7: serving every tool makes zero GitHub calls. The engine's client is a nil
// interface (any call panics), and the Store's fallback fetcher counts calls.
func TestLocalAPIMakesZeroGitHubCalls(t *testing.T) {
	e := apiEngine(t, 0)
	fb := &countingFetcher{}
	e.store = itemstate.NewStore(fb)
	e.seedAPIItem(t, 1, "Implement", "fabrik:awaiting-ci", "fabrik:awaiting-review")
	e.seedAPIItem(t, 2, "Queued")
	e.deepFetched(2)

	b := e.LocalAPIBackend()
	if _, err := b.Status(localapi.StatusParams{Issue: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Status(localapi.StatusParams{Issue: "2"}); err != nil {
		t.Fatal(err)
	}
	// An issue the cache has never seen is answered not_found — never fetched.
	if _, err := b.Status(localapi.StatusParams{Issue: "owner/repo#4040"}); err == nil {
		t.Fatal("unknown issue must not resolve")
	}
	for _, v := range []string{localapi.ViewAttention, localapi.ViewFlow} {
		if _, err := b.Board(localapi.BoardParams{View: v, Filter: &localapi.BoardFilter{Milestone: "v1", HasOpenBlockers: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Health(localapi.HealthParams{}); err != nil {
		t.Fatal(err)
	}
	if n := fb.calls.Load(); n != 0 {
		t.Fatalf("the Store's GitHub fallback was invoked %d times", n)
	}
}

// Static guard for the same rule: engine/localapi*.go (non-test) must not
// mention a path that can reach GitHub.
func TestLocalAPISourceDoesNotReachGitHub(t *testing.T) {
	files, err := filepath.Glob("localapi*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no localapi source files found: %v", err)
	}
	forbidden := []string{"e.client", "e.readClient", "FetchLabelAppliedAt", "labelAppliedAt(", "store.Get(", "e.cache()", ".FetchItem(", "FetchProjectItem"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(line, bad) {
					t.Errorf("%s:%d reaches a GitHub-capable path (%q): %s", f, i+1, bad, strings.TrimSpace(line))
				}
			}
		}
	}
}

// The API goroutines read while the poll side writes: -race must stay clean.
func TestLocalAPIConcurrentWithPollSideWrites(t *testing.T) {
	e := apiEngine(t, 0)
	for n := 1; n <= 10; n++ {
		e.seedAPIItem(t, n, "Implement")
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // poll-side writer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			n := i%10 + 1
			e.store.Apply(itemstate.LocalLabelAdded{Repo: "owner/repo", Number: n, Label: "fabrik:awaiting-ci"})
			e.store.Apply(itemstate.LabelAppliedAtRecorded{Repo: "owner/repo", Number: n, Label: "fabrik:awaiting-ci", At: time.Now()})
			e.store.Apply(itemstate.ReviewCycleIncremented{Repo: "owner/repo", Number: n, StageName: "Implement"})
			e.health.notePollAttempt(time.Now())
			e.health.notePollSuccess(time.Now())
			e.health.setGraphQLBackoff(i%2 == 0, 0.5, i)
			e.store.Apply(itemstate.LocalLabelRemoved{Repo: "owner/repo", Number: n, Label: "fabrik:awaiting-ci"})
		}
	}()
	go func() { // API reader
		defer wg.Done()
		b := e.LocalAPIBackend()
		for i := 0; i < 300; i++ {
			b.Status(localapi.StatusParams{Issue: "3"})
			b.Board(localapi.BoardParams{View: localapi.ViewAttention})
			b.Board(localapi.BoardParams{View: localapi.ViewFlow})
			b.Health(localapi.HealthParams{})
		}
		close(stop)
	}()
	wg.Wait()
}
