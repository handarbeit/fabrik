package engine

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

func causeLabelsOf(labels []string) []string { return pauseCauseLabelsIn(labels) }

// Every cause is written with the pause, exactly once, beside fabrik:paused and
// (unchanged, ADR 2487) fabrik:awaiting-input.
func TestPauseIssue_WritesExactlyOneCauseLabelPerCause(t *testing.T) {
	for _, c := range allPauseCauses {
		t.Run(string(c), func(t *testing.T) {
			eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
			item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
			eng.pauseIssue(item, "🏭 **Fabrik — test**\n\nmsg", pauseOpts{awaitingInput: true, labelEcho: true, cause: c})

			labels, _ := cache.FetchLabels("owner", "repo", 1)
			if !containsLabel(labels, "fabrik:paused") || !containsLabel(labels, "fabrik:awaiting-input") {
				t.Errorf("pair missing: %v", labels)
			}
			got := causeLabelsOf(labels)
			if len(got) != 1 || got[0] != c.label() {
				t.Errorf("cause labels = %v, want exactly [%s]", got, c.label())
			}
		})
	}
}

func TestPauseIssue_RePauseWithAnotherCauseReplacesIt(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "🏭 **Fabrik — a**\n\nmsg", pauseOpts{awaitingInput: true, labelEcho: true, cause: pauseCauseQuestion})
	// The caller's snapshot is stale (it predates the first pause): the Store's
	// labels must still drive the removal.
	eng.pauseIssue(item, "🏭 **Fabrik — b**\n\nmsg", pauseOpts{awaitingInput: true, labelEcho: true, cause: pauseCauseCITimeout})

	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if got := causeLabelsOf(labels); len(got) != 1 || got[0] != pauseCauseCITimeout.label() {
		t.Errorf("cause labels = %v, want exactly [%s]", got, pauseCauseCITimeout.label())
	}
}

func TestReapplyPauseLabels_WritesTheCause(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.reapplyPauseLabels(item, pauseCauseReviewTimeout)
	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if got := causeLabelsOf(labels); len(got) != 1 || got[0] != "fabrik:pause:review-timeout" {
		t.Errorf("cause labels = %v", got)
	}
}

// A stop on an item that is already paused must not replace the cause that says
// why it is paused; with no cause label (a pre-upgrade pause) the stop's cause is
// recorded.
func TestPauseInterruptedIssue_KeepsAnExistingCauseOnAnAlreadyPausedItem(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.pauseIssue(item, "🏭 **Fabrik — q**\n\nmsg", pauseOpts{awaitingInput: true, labelEcho: true, cause: pauseCauseQuestion})

	eng.pauseInterruptedIssue(item, pauseCauseOther, "🏭 **Fabrik — stop**\n\nmsg")

	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if got := causeLabelsOf(labels); len(got) != 1 || got[0] != pauseCauseQuestion.label() {
		t.Errorf("cause labels = %v, want exactly [%s]", got, pauseCauseQuestion.label())
	}

	eng2, cache2 := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng2.applyLabelAdd(item, "fabrik:paused", true)
	eng2.pauseInterruptedIssue(item, pauseCauseOther, "🏭 **Fabrik — stop**\n\nmsg")
	labels2, _ := cache2.FetchLabels("owner", "repo", 1)
	if got := causeLabelsOf(labels2); len(got) != 1 || got[0] != pauseCauseOther.label() {
		t.Errorf("cause labels (no prior cause) = %v, want exactly [%s]", got, pauseCauseOther.label())
	}
}

// A merge-train member is paused from owner/repo/number alone: the cause label it
// already carries on GitHub must be replaced even when the Store has never seen it.
func TestPauseMergeTrainMember_ReplacesACauseTheStoreHasNotSeen(t *testing.T) {
	var removed []string
	client := &mockGitHubClient{
		fetchLabelsFn: func(_, _ string, _ int) ([]string, error) {
			return []string{"fabrik:paused", "fabrik:awaiting-input", pauseCauseQuestion.label()}, nil
		},
		removeLabelFromIssueFn: func(_, _ string, _ int, label string) error {
			removed = append(removed, label)
			return nil
		},
	}
	eng, _ := testEngineWithCache(t, client, &mockClaudeInvoker{})

	eng.pauseMergeTrainMember("owner", "repo", 7, pauseCauseCycleLimit)

	if len(removed) != 1 || removed[0] != pauseCauseQuestion.label() {
		t.Errorf("removed labels = %v, want [%s]", removed, pauseCauseQuestion.label())
	}
}

func TestPauseIssue_UnsetCauseFallsBackToOther(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.pauseIssue(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "🏭 **Fabrik — x**\n\nmsg", pauseOpts{awaitingInput: true, labelEcho: true})
	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if got := causeLabelsOf(labels); len(got) != 1 || got[0] != pauseCauseOther.label() {
		t.Errorf("cause labels = %v, want [%s]", got, pauseCauseOther.label())
	}
}

// A failed cause write leaves the pause standing.
func TestPauseIssue_CauseWriteFailureKeepsThePause(t *testing.T) {
	client := &mockGitHubClient{
		addLabelToIssueFn: func(_, _ string, _ int, label string) error {
			if isPauseCauseLabel(label) {
				return errors.New("boom")
			}
			return nil
		},
	}
	eng, cache := testEngineWithCache(t, client, &mockClaudeInvoker{})
	eng.pauseIssue(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, "🏭 **Fabrik — x**\n\nmsg", pauseOpts{awaitingInput: true, labelEcho: true, cause: pauseCauseBreaker})
	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if !containsLabel(labels, "fabrik:paused") {
		t.Errorf("pause lost: %v", labels)
	}
	if got := causeLabelsOf(labels); len(got) != 0 {
		t.Errorf("a failed write left a cause label in the cache: %v", got)
	}
}

func TestApplyPauseLabels_DirectSiteWritesPausedAndCause(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.applyPauseLabels(gh.ProjectItem{Number: 1, Repo: "owner/repo"}, pauseCauseFailed, false)
	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if !containsLabel(labels, "fabrik:paused") || !containsLabel(labels, "fabrik:pause:failed") {
		t.Errorf("labels = %v", labels)
	}
	if containsLabel(labels, "fabrik:awaiting-input") {
		t.Errorf("awaitingInput=false must not add the label: %v", labels)
	}
}

func TestClearPauseCause_RemovesEveryCauseLabel(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyPauseLabels(item, pauseCauseBreaker, true)
	eng.clearPauseCause(item)
	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if got := causeLabelsOf(labels); len(got) != 0 {
		t.Errorf("cause labels left behind: %v", got)
	}
}

// The unblock paths lift the pause and must take the cause with it.
func TestUnblockAwaitingInput_ClearsTheCause(t *testing.T) {
	eng, cache := testEngineWithCache(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	item := gh.ProjectItem{Number: 1, Repo: "owner/repo"}
	eng.applyPauseLabels(item, pauseCauseQuestion, true)
	stage := testStages()[0]
	if !eng.unblockAwaitingInput(item, stage) {
		t.Fatal("unblock refused")
	}
	labels, _ := cache.FetchLabels("owner", "repo", 1)
	if containsLabel(labels, "fabrik:paused") || len(causeLabelsOf(labels)) != 0 {
		t.Errorf("labels after unblock = %v", labels)
	}
}

func TestSettlePauseCauseScan(t *testing.T) {
	cases := []struct {
		name       string
		boardLabel []string
		live       []string
		fetchErr   error
		disabled   bool
		wantRemove bool
	}{
		{name: "cause with no pause is stripped", boardLabel: []string{"fabrik:pause:question"}, live: []string{"fabrik:pause:question"}, wantRemove: true},
		{name: "live pause keeps it (stale board)", boardLabel: []string{"fabrik:pause:question"}, live: []string{"fabrik:paused", "fabrik:pause:question"}},
		{name: "board pause keeps it", boardLabel: []string{"fabrik:paused", "fabrik:pause:question"}, live: []string{"fabrik:paused", "fabrik:pause:question"}},
		{name: "no cause is a no-op", boardLabel: []string{"stage:Plan:complete"}},
		{name: "fetch error leaves it alone", boardLabel: []string{"fabrik:pause:other"}, fetchErr: errors.New("boom")},
		{name: "seam disabled", boardLabel: []string{"fabrik:pause:other"}, live: []string{"fabrik:pause:other"}, disabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLabelsFn: func(_, _ string, _ int) ([]string, error) { return tc.live, tc.fetchErr },
			}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			eng.SetPauseCauseSettleDisabledForTest(tc.disabled)
			board := &gh.ProjectBoard{Items: []gh.ProjectItem{{Number: 7, Repo: "owner/repo", Labels: tc.boardLabel}}}
			eng.settlePauseCauseScan(board)

			removed := len(client.removeLabelCalls) > 0
			if removed != tc.wantRemove {
				t.Fatalf("removed = %v, want %v (calls=%v)", removed, tc.wantRemove, client.removeLabelCalls)
			}
			if removed && client.removeLabelCalls[0].labelName != "fabrik:pause:question" {
				t.Errorf("removed %q", client.removeLabelCalls[0].labelName)
			}
		})
	}
}

// FR-006: nothing that decides whether a paused item resumes may read the cause.
// For every cause, the resume gate, isAwaitingInput and itemNeedsWork give the
// same answers as for the same pause with no cause label.
func TestResumeIsIdenticalWhateverTheCause(t *testing.T) {
	pausedAt := time.Now().Add(-time.Hour)
	for _, c := range append([]pauseCause{pauseCauseNone}, allPauseCauses...) {
		for _, tc := range []struct {
			name    string
			author  string
			at      time.Time
			want    bool
			refused int
		}{
			{"human after the pause resumes", "testuser", pausedAt.Add(30 * time.Minute), true, 0},
			{"human before the pause is refused", "testuser", pausedAt.Add(-30 * time.Minute), false, 1},
			{"bot after the pause is ignored", "some-bot[bot]", pausedAt.Add(30 * time.Minute), false, 0},
		} {
			t.Run(string(c)+"/"+tc.name, func(t *testing.T) {
				client := &mockGitHubClient{
					fetchLabelAppliedAtFn: func(_, _ string, _ int, _ string) (time.Time, error) { return pausedAt, nil },
				}
				eng := testEngine(t, client, &mockClaudeInvoker{})
				labels := []string{"fabrik:paused", "fabrik:awaiting-input"}
				if c != pauseCauseNone {
					labels = append(labels, c.label())
				}
				item := gh.ProjectItem{
					Number: 5, Repo: "owner/repo", Status: "Research", Labels: labels,
					Comments: []gh.Comment{{ID: "C_1", DatabaseID: 1, Body: "go on", Author: tc.author, CreatedAt: tc.at}},
				}
				ok, _, refused := eng.resumeAuthorised(item)
				if ok != tc.want || refused != tc.refused {
					t.Errorf("resumeAuthorised = (%v, refused %d), want (%v, %d)", ok, refused, tc.want, tc.refused)
				}
				if !isAwaitingInput(item) {
					t.Error("isAwaitingInput must hold for the pair whatever the cause")
				}
				base := item
				base.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
				if got, want := eng.itemNeedsWork(item), eng.itemNeedsWork(base); got != want {
					t.Errorf("itemNeedsWork = %v with the cause label, %v without", got, want)
				}
			})
		}
	}
}

func TestPauseCauseVocabularyIsClosedAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range allPauseCauses {
		l := c.label()
		if !c.valid() || !isPauseCauseLabel(l) || seen[l] {
			t.Errorf("bad or duplicate cause %q", c)
		}
		seen[l] = true
		if len(l) > 50 {
			t.Errorf("%s exceeds GitHub's 50-character label limit", l)
		}
	}
	if pauseCauseNone.valid() || pauseCause("bogus").valid() {
		t.Error("unset and unknown causes must be invalid")
	}
}
