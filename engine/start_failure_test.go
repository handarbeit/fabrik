package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/warnings"
)

func startErr() error {
	_, err := exec.Command("this-binary-does-not-exist-fabrik-test").Output()
	return err
}

// startFailureEngine builds an engine whose Claude start fails while *fail is
// true, on a settable clock.
func startFailureEngine(t *testing.T, fail *bool, calls *int) (*Engine, *mockGitHubClient, *fakeClock) {
	t.Helper()
	skipIfNoGit(t)
	client := &mockGitHubClient{}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			*calls++
			if *fail {
				return "", false, TokenUsage{}, startErr()
			}
			return "done\nFABRIK_STAGE_COMPLETE", true, TokenUsage{}, nil
		},
	}
	eng := NewWithDeps(
		Config{Owner: "o", Repo: "r", User: "u", Token: "t", Stages: testStages(), PollSeconds: 30, RetryBackoff: 60 * time.Second},
		client, claude, NewWorktreeManager(initBareRepo(t)),
	)
	clk := &fakeClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	eng.SetClock(clk)
	return eng, client, clk
}

func TestIsClaudeStartFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"exec.Error", &exec.Error{Name: "claude", Err: exec.ErrNotFound}, true},
		{"wrapped exec.Error", fmt.Errorf("starting claude: %w", &exec.Error{Name: "claude", Err: exec.ErrNotFound}), true},
		{"path error", &os.PathError{Op: "chdir", Path: "/x", Err: os.ErrNotExist}, true},
		{"wrapped ErrNotFound", fmt.Errorf("x: %w", exec.ErrNotFound), true},
		{"plain error resembling a start error", errors.New(`exec: "claude": executable file not found in $PATH`), false},
		{"exit error", errors.New("exit status 1"), false},
	}
	for _, c := range cases {
		if got := isClaudeStartFailure(c.err); got != c.want {
			t.Errorf("%s: isClaudeStartFailure = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStartFailureDelaySchedule(t *testing.T) {
	eng := NewWithDeps(Config{PollSeconds: 30, RetryBackoff: 60 * time.Second}, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	// cap = githubRecheckInterval = 300s
	want := []time.Duration{0, 0, 60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second, 300 * time.Second}
	for n, w := range want {
		if got := eng.startFailureDelay(n); got != w {
			t.Errorf("startFailureDelay(%d) = %v, want %v", n, got, w)
		}
	}
	// n=1 is already due, n=2 is the first backoff step.
	if got := eng.startFailureDelay(1); got != 0 {
		t.Errorf("first failure delay = %v, want 0", got)
	}
}

func TestStartFailure_RecordsDueCooldown_NoRetryCharge(t *testing.T) {
	for _, neutralised := range []bool{false, true} {
		t.Run(map[bool]string{false: "fix", true: "neutralised"}[neutralised], func(t *testing.T) {
			fail := true
			var calls int
			eng, client, clk := startFailureEngine(t, &fail, &calls)
			defer SetPostRunRetryRecordDisabledForTest(neutralised)()

			item := gh.ProjectItem{Number: 6, Title: "Err", Status: "Research", ItemID: "PVTI_6"}
			if err := eng.processItem(context.Background(), &gh.ProjectBoard{ProjectID: "PVT_1"}, item); err != nil {
				t.Fatalf("processItem: %v", err)
			}
			snap, _ := eng.store.Get("o/r", 6)
			until := snap.CooldownAt("retry-backoff")
			if neutralised {
				if !until.IsZero() {
					t.Fatalf("neutralised: retry-backoff recorded (%v)", until)
				}
			} else if !until.Equal(clk.t) {
				t.Fatalf("retry-backoff = %v, want already due at %v (first failure)", until, clk.t)
			}
			if !snap.LastAttemptAt("Research").IsZero() {
				t.Error("LastAttemptAt must stay zero after a start failure")
			}
			if got := snap.Attempts("Research"); got != 0 {
				t.Errorf("Attempts = %d, want 0 (start failure is not charged to max_retries)", got)
			}
			for _, c := range client.addLabelCalls {
				if strings.HasSuffix(c.labelName, ":failed") || c.labelName == "fabrik:paused" {
					t.Errorf("label %q applied after a start failure", c.labelName)
				}
			}
		})
	}
}

func TestStartFailure_BackoffGateAndRecovery(t *testing.T) {
	fail := true
	var calls int
	eng, client, clk := startFailureEngine(t, &fail, &calls)
	defer warnings.Resolve(warnings.Condition{Type: startFailureWarningType, Subject: startFailureWarningSubject}.Key())

	item := gh.ProjectItem{Number: 7, Title: "Err", Status: "Research", ItemID: "PVTI_7"}
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	run := func() {
		t.Helper()
		if err := eng.processItem(context.Background(), board, item); err != nil {
			t.Fatalf("processItem: %v", err)
		}
	}
	warned := func() bool {
		entries, _ := warnings.Load()
		for _, en := range entries {
			if en.Key == startFailureWarningType+":"+startFailureWarningSubject {
				return true
			}
		}
		return false
	}

	// 1st failure: already due, no warning yet.
	run()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if warned() {
		t.Error("warning reported on the first failure; want none for a single transient failure")
	}
	if need, in := eng.itemNeedsWorkRetryAt(item); !need || in != 0 {
		t.Errorf("after the 1st failure itemNeedsWorkRetryAt = (%v, %v), want admitted", need, in)
	}

	// 2nd failure: held for the stage backoff; warning appears.
	run()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if !warned() {
		t.Error("warning not reported on the 2nd consecutive failure")
	}
	need, in := eng.itemNeedsWorkRetryAt(item)
	if need || in != 60*time.Second {
		t.Fatalf("held gate = (%v, %v), want (false, 60s)", need, in)
	}
	snap, _ := eng.store.Get("o/r", 7)
	if want := clk.t.Add(60 * time.Second); !snap.CooldownAt("retry-backoff").Equal(want) {
		t.Errorf("retry-backoff = %v, want %v (the logged delay)", snap.CooldownAt("retry-backoff"), want)
	}

	// processItem refuses while held: no new launch.
	run()
	if calls != 2 {
		t.Errorf("calls = %d while held, want 2 (no launch at poll rate)", calls)
	}

	// Gate neutralised: the same call would launch again (non-vacuity).
	eng.SetStartFailureGateDisabledForTest(true)
	if need, _ := eng.itemNeedsWorkRetryAt(item); !need {
		t.Error("gate neutralised but item still held")
	}
	eng.SetStartFailureGateDisabledForTest(false)

	// Hold lapses: next failure doubles.
	clk.t = clk.t.Add(61 * time.Second)
	run()
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if _, in := eng.itemNeedsWorkRetryAt(item); in != 120*time.Second {
		t.Errorf("3rd failure hold = %v, want 120s", in)
	}

	// Recovery: a real start clears the count and the warning.
	fail = false
	clk.t = clk.t.Add(121 * time.Second)
	run()
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
	if warned() {
		t.Error("warning still active after a successful start")
	}
	if eng.startFailureRetryIn(item) != 0 {
		t.Error("hold survived a successful start")
	}
	_ = client
}

func TestStartFailure_NonStartErrorResetsCount(t *testing.T) {
	eng := NewWithDeps(Config{PollSeconds: 30, RetryBackoff: time.Minute}, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	item := gh.ProjectItem{Number: 9}
	eng.recordStartFailure(item, startErr())
	if d := eng.recordStartFailure(item, startErr()); d != time.Minute {
		t.Fatalf("2nd delay = %v", d)
	}
	defer warnings.Resolve(warnings.Condition{Type: startFailureWarningType, Subject: startFailureWarningSubject}.Key())
	if claudeProcessStarted(errors.New("exit status 1")) {
		eng.noteClaudeStarted(item)
	}
	if d := eng.recordStartFailure(item, startErr()); d != 0 {
		t.Errorf("delay after a started-then-errored invocation = %v, want the first-failure 0", d)
	}
	// A refusal before any spawn proves nothing about the binary.
	if claudeProcessStarted(&skillUnresolvedError{}) || claudeProcessStarted(startErr()) {
		t.Error("claudeProcessStarted true for a non-spawn outcome")
	}
}

func TestStartFailure_PrunesLapsedEntries(t *testing.T) {
	eng := NewWithDeps(Config{PollSeconds: 30, RetryBackoff: time.Minute}, &mockGitHubClient{}, &mockClaudeInvoker{}, NewWorktreeManager(t.TempDir()))
	clk := &fakeClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	eng.SetClock(clk)
	defer warnings.Resolve(warnings.Condition{Type: startFailureWarningType, Subject: startFailureWarningSubject}.Key())
	for n := 1; n <= 20; n++ {
		eng.recordStartFailure(gh.ProjectItem{Number: n}, startErr())
	}
	clk.t = clk.t.Add(time.Hour)
	eng.recordStartFailure(gh.ProjectItem{Number: 99}, startErr())
	if got := len(eng.startFailures.items); got != 1 {
		t.Errorf("items after pruning = %d, want 1", got)
	}
}
