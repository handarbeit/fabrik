package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// Scripted-train tests for the TUI row and History (#2050, ADR 2050): the live
// membership title, the phase hook, and exactly one History entry per episode
// with its real outcome. They run the real runMergeTrainWorker over the
// trainValidateFn seam, like status_line_train_test.go.

// runTrainWithEvents runs one worker over batch and returns every TUI event it
// emitted, in order.
func runTrainWithEvents(t *testing.T, eng *Engine, batch []gh.ProjectItem) []tui.Event {
	t.Helper()
	ch := make(chan tui.Event, 4096)
	eng.events = ch
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_1"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	captureStdout(func() { eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch) })
	return collectEvents(ch, 20*time.Millisecond)
}

// trainCompletions returns the JobCompletedEvents of the train row that would
// reach History (Skipped=false).
func trainCompletions(events []tui.Event) []tui.JobCompletedEvent {
	var out []tui.JobCompletedEvent
	for _, raw := range events {
		if ev, ok := raw.(tui.JobCompletedEvent); ok && ev.IssueNumber == 0 && ev.Repo == "owner/repo" && !ev.Skipped {
			out = append(out, ev)
		}
	}
	return out
}

func trainRows(events []tui.Event) []tui.TrainRowEvent {
	var out []tui.TrainRowEvent
	for _, raw := range events {
		if ev, ok := raw.(tui.TrainRowEvent); ok {
			out = append(out, ev)
		}
	}
	return out
}

// outcomeScenario scripts one train that must end in a given History outcome.
type outcomeScenario struct {
	name        string
	n           int
	redWhen     func(map[int]bool) bool
	setup       func(eng *Engine, client *mockGitHubClient)
	wantOutcome string
	wantSuccess bool
	wantDetail  []string
}

func outcomeScenarios() []outcomeScenario {
	prFails := func(bad ...int) func(eng *Engine, client *mockGitHubClient) {
		return func(eng *Engine, client *mockGitHubClient) {
			base := client.fetchLinkedPRFn
			client.fetchLinkedPRFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
				for _, b := range bad {
					if n == b {
						return nil, fmt.Errorf("no linked PR for #%d", n)
					}
				}
				return base(owner, repo, n)
			}
		}
	}
	return []outcomeScenario{
		{
			name: "landed", n: 3,
			redWhen:     func(map[int]bool) bool { return false },
			wantOutcome: "landed", wantSuccess: true,
			wantDetail: []string{"#1 #2 #3", "via PR #900"},
		},
		{
			name: "red-bisected", n: 4,
			redWhen:     func(present map[int]bool) bool { return present[2] },
			wantOutcome: "red → bisected", wantSuccess: true,
			wantDetail: []string{"poisoner #2 ejected", "landed #1 #3 #4"},
		},
		{
			name: "ejected-at-assembly", n: 2,
			redWhen:     func(map[int]bool) bool { return false },
			setup:       prFails(1, 2),
			wantOutcome: "ejected at assembly", wantSuccess: true,
			wantDetail: []string{"ejected #1 (no usable linked PR)", "#2 (no usable linked PR)"},
		},
		{
			name: "one-at-a-time", n: 2,
			// Red whenever two or more members are combined; each alone is green, so
			// bisection cannot isolate a single poisoner and degrades.
			redWhen:     func(present map[int]bool) bool { return len(present) > 1 },
			wantOutcome: "one-at-a-time", wantSuccess: true,
			wantDetail: []string{"landed #1 #2"},
		},
		{
			name: "abandoned-ci-infra", n: 2,
			redWhen: func(map[int]bool) bool { return false },
			setup: func(eng *Engine, client *mockGitHubClient) {
				eng.trainValidateFn = func(context.Context, []trainMember) (TrainCIResult, *trainCIDiagnostic) {
					return TrainCIInfra, &trainCIDiagnostic{Note: "no runs started"}
				}
			},
			wantOutcome: "abandoned", wantSuccess: false,
			wantDetail: []string{"CI never started"},
		},
		{
			name: "dissolved-nothing-to-land", n: 2,
			redWhen: func(map[int]bool) bool { return false },
			setup: func(eng *Engine, client *mockGitHubClient) {
				// Every member already left the holding column after the snapshot.
				client.fetchProjectItemStatusFn = func(string) (string, error) { return "Done", nil }
			},
			wantOutcome: "dissolved", wantSuccess: true,
			wantDetail: []string{"nothing to land", "deferred #1 (live status)", "#2 (live status)"},
		},
		{
			// A fact recorded earlier (the poisoner) must not make a later failed
			// landing look like a success: the survivors' merge fails, nothing
			// lands, so the episode is abandoned, not "red → bisected" (review
			// finding on nothingRecorded, now landedNothing).
			name: "bisected-then-landing-fails", n: 4,
			redWhen: func(present map[int]bool) bool { return present[2] },
			setup: func(eng *Engine, client *mockGitHubClient) {
				client.mergePRFn = func(owner, repo string, prNumber int) error {
					return fmt.Errorf("merge refused")
				}
			},
			wantOutcome: "abandoned", wantSuccess: false,
			wantDetail: []string{"batch landing did not complete", "poisoner #2 ejected"},
		},
		{
			name: "landed-with-assembly-ejection", n: 3,
			redWhen:     func(map[int]bool) bool { return false },
			setup:       prFails(2),
			wantOutcome: "landed", wantSuccess: true,
			wantDetail: []string{"#1 #3", "ejected #2 (no usable linked PR)"},
		},
	}
}

// outcomeProblems checks that events hold exactly one History-bound completion
// matching sc, and returns every deviation. The real test requires none; the
// neutralisation test (FR-013) requires some.
func outcomeProblems(sc outcomeScenario, events []tui.Event) []string {
	var problems []string
	done := trainCompletions(events)
	if len(done) != 1 {
		return []string{fmt.Sprintf("History-bound completions = %d, want exactly 1", len(done))}
	}
	got := done[0]
	if got.Outcome != sc.wantOutcome {
		problems = append(problems, fmt.Sprintf("Outcome = %q, want %q", got.Outcome, sc.wantOutcome))
	}
	if got.Success != sc.wantSuccess {
		problems = append(problems, fmt.Sprintf("Success = %v, want %v", got.Success, sc.wantSuccess))
	}
	for _, want := range sc.wantDetail {
		if !strings.Contains(got.Detail, want) {
			problems = append(problems, fmt.Sprintf("Detail %q lacks %q", got.Detail, want))
		}
	}
	return problems
}

func runOutcomeScenario(t *testing.T, sc outcomeScenario, neutralised bool) (events []tui.Event) {
	t.Helper()
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, sc.redWhen)
	if sc.setup != nil {
		sc.setup(eng, client)
	}
	eng.SetTrainOutcomeNeutralisedForTest(neutralised)
	return runTrainWithEvents(t, eng, makeSeamBatch(sc.n))
}

// Each outcome produces exactly one History entry with the right Success value.
func TestMergeTrainTUI_OneHistoryEntryPerOutcome(t *testing.T) {
	for _, sc := range outcomeScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			events := runOutcomeScenario(t, sc, false)
			if problems := outcomeProblems(sc, events); len(problems) > 0 {
				t.Fatalf("outcome problems: %s", strings.Join(problems, "; "))
			}
			// The row is removed: the same completion removes it, and no scripted exit
			// falls into the catch-all.
			if got := trainCompletions(events)[0].Outcome; got == trainOutcomeNothingLands {
				t.Errorf("scenario fell into the catch-all outcome %q", got)
			}
		})
	}
}

// FR-013 / SC-005: with the outcome emission neutralised (the blanket Skipped
// completion), the outcome checks above fail for every scenario.
func TestMergeTrainTUI_OutcomeTestFailsWhenNeutralised(t *testing.T) {
	for _, sc := range outcomeScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			events := runOutcomeScenario(t, sc, true)
			if problems := outcomeProblems(sc, events); len(problems) == 0 {
				t.Fatal("outcome checks passed with the outcome emission neutralised — they would not catch a regression")
			}
			// The neutralised run still removes the row (a Skipped completion).
			var removed bool
			for _, raw := range events {
				if ev, ok := raw.(tui.JobCompletedEvent); ok && ev.IssueNumber == 0 && ev.Skipped {
					removed = true
				}
			}
			if !removed {
				t.Error("neutralised run emitted no completion at all")
			}
		})
	}
}

// Ejecting members at assembly updates the row title, in place, from the first
// event on: "N of M" with the ejected members named.
func TestMergeTrainTUI_AssemblyEjectionUpdatesRowTitle(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	base := client.fetchLinkedPRFn
	client.fetchLinkedPRFn = func(owner, repo string, n int) (*gh.PRDetails, error) {
		if n == 1 || n == 4 {
			return nil, fmt.Errorf("no linked PR")
		}
		return base(owner, repo, n)
	}
	events := runTrainWithEvents(t, eng, makeSeamBatch(5))

	var startTitle string
	for _, raw := range events {
		if ev, ok := raw.(tui.JobStartedEvent); ok && ev.IssueNumber == 0 {
			startTitle = ev.Title
		}
	}
	// The scripted worker runs partition "main", so the shared row names its base.
	if startTitle != "[main] 5 of 5: #1 #2 #3 #4 #5" {
		t.Errorf("start title = %q, want %q", startTitle, "[main] 5 of 5: #1 #2 #3 #4 #5")
	}

	var titles []string
	for _, row := range trainRows(events) {
		if len(titles) == 0 || titles[len(titles)-1] != row.Title {
			titles = append(titles, row.Title)
		}
	}
	want := []string{
		"[main] 4 of 5: #2 #3 #4 #5 (ejected #1)",
		"[main] 3 of 5: #2 #3 #5 (ejected #1 #4)",
	}
	if len(titles) < len(want) || titles[0] != want[0] || titles[1] != want[1] {
		t.Errorf("row titles = %q, want to start with %q", titles, want)
	}
}

// Every phase transition sets the phase and restarts its clock; a membership-only
// change refreshes the title but keeps the phase and its start time.
func TestMergeTrainTUI_PhaseTransitionsAndTimeInPhase(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, _, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	ch := make(chan tui.Event, 64)
	eng.events = ch
	ep := newTrainEpisode(defaultPartitionBase, []int{1, 2, 3})
	members := []trainMember{{item: makeTrainItem(1, "a")}, {item: makeTrainItem(2, "b")}, {item: makeTrainItem(3, "c")}}

	steps := []trainPhase{phaseAssembling(), phaseResolvingConflicts(2), phaseTrialCI(4012), phaseBisecting(2, 7), phaseLanding(), phaseOneAtATime(), phaseCatchingUp(55), phaseWaitingForSlot()}
	wantLabels := []string{"assembling", "resolving conflicts on #2", "trial CI #4012", "bisecting (step 2)", "landing", "one-at-a-time", "catching up #55", "waiting for slot"}
	var starts []time.Time
	for i, ph := range steps {
		time.Sleep(2 * time.Millisecond)
		eng.noteTrainPhase(ep, "owner/repo", members, ph)
		rows := trainRows(collectEvents(ch, 5*time.Millisecond))
		if len(rows) != 1 || rows[0].Phase != wantLabels[i] {
			t.Fatalf("step %d: rows = %+v, want one row with phase %q", i, rows, wantLabels[i])
		}
		if len(starts) > 0 && !rows[0].PhaseStartedAt.After(starts[len(starts)-1]) {
			t.Errorf("step %d: phase clock did not restart (%v after %v)", i, rows[0].PhaseStartedAt, starts[len(starts)-1])
		}
		starts = append(starts, rows[0].PhaseStartedAt)
	}

	// Membership-only change: title moves, phase and its clock stay.
	eng.noteTrainEjected(ep, "owner/repo", 3, "unresolvable conflict", true)
	rows := trainRows(collectEvents(ch, 5*time.Millisecond))
	if len(rows) != 1 {
		t.Fatalf("ejection emitted %d rows, want 1", len(rows))
	}
	if rows[0].Phase != "waiting for slot" || !rows[0].PhaseStartedAt.Equal(starts[len(starts)-1]) {
		t.Errorf("membership change altered the phase clock: %+v", rows[0])
	}
	if want := "2 of 3: #1 #2 (ejected #3)"; rows[0].Title != want {
		t.Errorf("title = %q, want %q", rows[0].Title, want)
	}
}

// The board status line and the TUI row are driven by the same hook calls: for a
// scripted green train they see the same ordered phase sequence (FR-009, SC-004).
func TestMergeTrainTUI_BoardAndRowSeeSamePhaseSequence(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	enableStatusLine(t, eng, client)

	var mu sync.Mutex
	var recs []trainPhaseRecord
	eng.trainPhaseObserverForTest = func(r trainPhaseRecord) {
		mu.Lock()
		recs = append(recs, r)
		mu.Unlock()
	}
	events := runTrainWithEvents(t, eng, makeSeamBatch(3))

	mu.Lock()
	defer mu.Unlock()
	rows := trainRows(events)
	if len(rows) != len(recs) {
		t.Fatalf("TUI rows = %d, hook calls = %d — every transition must reach both", len(rows), len(recs))
	}
	for i := range recs {
		if rows[i].Phase != recs[i].Label {
			t.Errorf("transition %d: TUI phase %q, hook label %q", i, rows[i].Phase, recs[i].Label)
		}
	}
	wantLabels := []string{"assembling", "assembling", "trial CI", "landing"}
	for i, w := range wantLabels {
		if i >= len(recs) || recs[i].Label != w {
			t.Fatalf("hook labels = %+v, want prefix %q", recs, wantLabels)
		}
	}

	// The board projection of the same records (boarded phases, consecutive
	// duplicates collapsed by the writer, then the Done clear).
	var wantBoard []string
	for _, r := range recs {
		if r.Board != "" && (len(wantBoard) == 0 || wantBoard[len(wantBoard)-1] != r.Board) {
			wantBoard = append(wantBoard, r.Board)
		}
	}
	wantBoard = append(wantBoard, "<cleared>")
	for i := 1; i <= 3; i++ {
		if got := statusLineSeq(client, fmt.Sprintf("item-%d", i)); !equalSeq(got, wantBoard) {
			t.Errorf("item-%d board lines = %q, want %q (the board projection of the hook records)", i, got, wantBoard)
		}
	}
}

// Bisection shows its step on the row, and sub-trials do not overwrite it with
// their own "trial CI" label (the same quietness the board line has).
func TestMergeTrainTUI_BisectShowsStepAndStaysQuiet(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, _, _ := seamTrainEngine(t, wm, func(present map[int]bool) bool { return present[2] })
	events := runTrainWithEvents(t, eng, makeSeamBatch(4))

	var labels []string
	for _, row := range trainRows(events) {
		if len(labels) == 0 || labels[len(labels)-1] != row.Phase {
			labels = append(labels, row.Phase)
		}
	}
	var sawStep bool
	for i, l := range labels {
		if strings.HasPrefix(l, "bisecting (step ") {
			sawStep = true
			if i+1 < len(labels) && labels[i+1] == "trial CI" && i+1 < len(labels)-1 && strings.HasPrefix(labels[i+2], "bisecting") {
				t.Errorf("sub-trial overwrote the bisect phase: %q", labels)
			}
		}
	}
	if !sawStep {
		t.Fatalf("no bisecting phase on the row: %q", labels)
	}
}

// A train that fails during setup still removes its row and writes exactly one
// History entry (an abandoned-style outcome), never none.
func TestMergeTrainTUI_SetupFailureStillOneEntry(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, _, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	eng.cfg.Stages = nil // no holding stage configured: prepareTrainWorker aborts
	events := runTrainWithEvents(t, eng, makeSeamBatch(2))

	done := trainCompletions(events)
	if len(done) != 1 {
		t.Fatalf("completions = %d, want exactly 1", len(done))
	}
	if done[0].Outcome != "abandoned" || done[0].Success {
		t.Errorf("completion = %+v, want abandoned / unsuccessful", done[0])
	}
}

// The slot wait is announced on the row only when the wait is real, and the
// previous phase is restored once the slot is obtained.
func TestMergeTrainTUI_WaitingForSlotPhase(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, _, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	ch := make(chan tui.Event, 64)
	eng.events = ch
	ep := newTrainEpisode(defaultPartitionBase, []int{7})
	eng.noteTrainPhase(ep, "owner/repo", nil, phaseResolvingConflicts(7))
	collectEvents(ch, 5*time.Millisecond)

	// Free slot: no wait phase.
	rel, err := eng.acquireTrainSlotEp(context.Background(), ep, "owner/repo", makeTrainItem(7, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if rows := trainRows(collectEvents(ch, 5*time.Millisecond)); len(rows) != 0 {
		t.Fatalf("an uncontended acquire emitted %d rows, want 0", len(rows))
	}
	rel()

	// Saturate the semaphore, then release it from a goroutine mid-wait.
	for i := 0; i < cap(eng.sem); i++ {
		eng.sem <- struct{}{}
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		<-eng.sem
	}()
	rel, err = eng.acquireTrainSlotEp(context.Background(), ep, "owner/repo", makeTrainItem(7, "x"))
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, r := range trainRows(collectEvents(ch, 5*time.Millisecond)) {
		labels = append(labels, r.Phase)
	}
	rel()
	if len(labels) != 2 || labels[0] != "waiting for slot" || labels[1] != "resolving conflicts on #7" {
		t.Errorf("phases during a slot wait = %q, want [waiting for slot, resolving conflicts on #7]", labels)
	}
}
