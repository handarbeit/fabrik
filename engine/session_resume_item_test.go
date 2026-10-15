package engine

import (
	"context"
	"os"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// stageResumeHarness runs processItem for a Research item against a scripted
// invoker that, like the real one, leaves a saved session behind.
type stageResumeHarness struct {
	eng    *Engine
	item   gh.ProjectItem
	board  *gh.ProjectBoard
	mu     sync.Mutex
	resume []bool
	script func(call int) (completed bool, err error)
}

func newStageResumeHarness(t *testing.T, mode string, tokens int) *stageResumeHarness {
	t.Helper()
	skipIfNoGit(t)
	h := &stageResumeHarness{
		board: &gh.ProjectBoard{ProjectID: "PVT_1"},
		item:  gh.ProjectItem{Number: 300, Title: "Resume", Status: "Research", ItemID: "PVTI_300"},
	}
	stgs := testStages()
	for _, s := range stgs {
		if s.Name == "Research" {
			s.ResumeSession = mode
			s.ResumeThresholdTokens = 250000
		}
	}
	claude := &mockClaudeInvoker{}
	h.eng = NewWithDeps(
		Config{Owner: "owner", Repo: "repo", ProjectNum: 1, User: "testuser", Token: "token", MaxRetries: 5, MaxSliceRetries: 5, Stages: stgs},
		&mockGitHubClient{}, claude, NewWorktreeManager(initBareRepo(t)),
	)
	h.eng.SetSessionDirForTest(t.TempDir())
	claude.invokeFn = func(stage *stages.Stage, issue gh.ProjectItem, _ []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
		h.mu.Lock()
		h.resume = append(h.resume, resume)
		n := len(h.resume)
		h.mu.Unlock()
		completed, err := h.script(n)
		// The real invoker saves the session pointer and its size; the sim never runs it.
		path := h.eng.SessionFileForTest(issue, stage.Name)
		if _, statErr := os.Stat(path); statErr != nil {
			h.eng.RecordSessionForTest(issue, stage.Name, "S1", tokens, false)
		}
		out := "work"
		if completed {
			out = "done\nFABRIK_STAGE_COMPLETE"
		}
		return out, completed, TokenUsage{TurnsUsed: 1, MaxTurns: 50}, err
	}
	return h
}

func (h *stageResumeHarness) run(t *testing.T) {
	t.Helper()
	if err := h.eng.processItem(context.Background(), h.board, h.item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
}

// restart forgets the in-memory per-stage state, as a daemon restart does.
func (h *stageResumeHarness) restart() {
	h.eng.store.Apply(itemstate.StageLastAttemptCleared{Repo: "owner/repo", Number: h.item.Number, StageName: "Research"})
}

func (h *stageResumeHarness) calls() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.resume...)
}

func (h *stageResumeHarness) meta(t *testing.T) sessionMeta {
	t.Helper()
	m, ok := readSessionMeta(h.eng.SessionFileForTest(h.item, "Research"))
	if !ok {
		t.Fatal("no session meta")
	}
	return m
}

// An incomplete run leaves the session open; the next run is a continuation and
// resumes under every option, also with the in-memory state gone (R2). Once the
// stage completes, a later run is a re-entry governed by the option.
func TestProcessItem_ContinuationVersusReentry_AcrossRestart(t *testing.T) {
	h := newStageResumeHarness(t, "fresh", 900000)
	h.script = func(call int) (bool, error) {
		if call == 1 {
			return false, &claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
		}
		return call == 2, nil // call 2 completes the stage, call 3 is a re-entry
	}

	h.run(t) // fresh start: nothing saved yet
	if m := h.meta(t); m.Closed {
		t.Fatalf("turn-capped run must leave the session open: %+v", m)
	}
	h.restart()
	h.run(t) // continuation after a "restart": resumes although the option says fresh
	if m := h.meta(t); !m.Closed {
		t.Fatalf("completed run must close the session: %+v", m)
	}
	h.restart()
	h.run(t) // re-entry: governed by resume_session: fresh

	got := h.calls()
	want := []bool{false, true, false}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("resume flags = %v, want %v", got, want)
	}
}

func TestProcessItem_ReentryUnderDefaultResumes(t *testing.T) {
	h := newStageResumeHarness(t, "", 900000)
	h.script = func(call int) (bool, error) { return true, nil }
	h.run(t)
	h.restart()
	h.run(t)
	if got := h.calls(); len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("resume flags = %v, want [false true]", got)
	}
}

func TestProcessItem_AutoReentryAboveThresholdIsFresh(t *testing.T) {
	h := newStageResumeHarness(t, "auto", 900000)
	h.script = func(call int) (bool, error) { return true, nil }
	h.run(t)
	h.restart()
	h.run(t)
	if got := h.calls(); len(got) != 2 || got[1] {
		t.Fatalf("resume flags = %v, want the re-entry fresh above the threshold", got)
	}
	// Non-vacuity: forced resume flips the same scenario.
	h2 := newStageResumeHarness(t, "auto", 900000)
	h2.eng.SetSessionDecisionForcedResumeForTest(true)
	h2.script = h.script
	h2.run(t)
	h2.restart()
	h2.run(t)
	if got := h2.calls(); len(got) != 2 || !got[1] {
		t.Fatalf("forced resume flags = %v", got)
	}
}

// R7: the explicit stage resets keep their fresh-start effect and now set the
// durable flag, so they no longer depend on the in-memory LastAttemptAt.
func TestSessionReset_AtTheFourClearSites(t *testing.T) {
	skipIfNoGit(t)
	cases := []struct {
		name  string
		stage string
		fn    func(e *Engine, item gh.ProjectItem)
	}{
		{"clearFailedStage", "Research", func(e *Engine, item gh.ProjectItem) { e.clearFailedStage(item, &stages.Stage{Name: "Research"}) }},
		{"reenterValidate", "Validate", func(e *Engine, item gh.ProjectItem) { e.reenterValidate(item, "owner", "repo") }},
		{"handleValidateSHAInvalidation", "Validate", func(e *Engine, item gh.ProjectItem) { e.handleValidateSHAInvalidation(item, "owner", "repo") }},
		{"unblockAwaitingInput", "Research", func(e *Engine, item gh.ProjectItem) { e.unblockAwaitingInput(item, &stages.Stage{Name: "Research"}) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
			e.SetSessionDirForTest(t.TempDir())
			item := gh.ProjectItem{Number: 5, Repo: "owner/repo", Status: c.stage}
			e.RecordSessionForTest(item, c.stage, "S1", 10, false)
			c.fn(e, item)
			if m, _ := readSessionMeta(e.SessionFileForTest(item, c.stage)); !m.Reset {
				t.Fatalf("reset flag not set: %+v", m)
			}
			if d := e.decideSessionResume(item, &stages.Stage{Name: c.stage}, sessionStageDispatch, false); d.Resume {
				t.Fatalf("stage dispatch after reset resumed: %+v", d)
			}
		})
	}
}

// Non-vacuity (R2): with the exemption removed the continuation above starts fresh.
func TestProcessItem_ContinuationExemptionRemovedBreaksContinuation(t *testing.T) {
	h := newStageResumeHarness(t, "fresh", 900000)
	h.eng.SetContinuationExemptionDisabledForTest(true)
	h.script = func(call int) (bool, error) {
		if call == 1 {
			return false, &claudeTurnLimitError{TerminalReason: "max_turns", NumTurns: 50}
		}
		return true, nil
	}
	h.run(t)
	h.restart()
	h.run(t)
	if got := h.calls(); len(got) != 2 || got[1] {
		t.Fatalf("resume flags = %v, want the continuation fresh once the exemption is gone", got)
	}
}
