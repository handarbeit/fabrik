package sim

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/stages"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// #2120 scenarios: the Suspended (pending decision) state, its one-per-episode
// push, opt-in reminders and withdrawn settlements. Parallel, like the other
// channel scenarios: the timing and neutralisation seams are per-Engine (#2210).

func decisionStages() []*stages.Stage {
	no := false
	return []*stages.Stage{
		{Name: "Specify", Order: 1, AutoAdvance: &no},
		{Name: "Research", Order: 2},
		{Name: "Implement", Order: 3},
		{Name: "Validate", Order: 4},
		{Name: "Done", Order: 5, CleanupWorktree: true},
	}
}

// startChannelAt is startChannel over a chosen state dir (so a restart reuses
// the persisted episode counters), a short real-time tick for the time-driven
// parts, and an optional extra subscription.
func startChannelAt(t *testing.T, env *Env, dir string, subs ...channelevents.Subscription) (*channelevents.Hub, *simSink) {
	t.Helper()
	env.Engine.SetChannelTimingForTest(20*time.Millisecond, 40*time.Millisecond)
	hub := env.Engine.StartChannelEventsForTest(dir)
	if hub == nil {
		t.Fatal("channel hub did not start")
	}
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	none := []string{}
	if len(hub.Subscriptions("S")) == 0 {
		if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "S", ExcludeLabels: &none}); err != nil {
			t.Fatal(err)
		}
	}
	sink := &simSink{}
	hub.Attach("S", sink)
	return hub, sink
}

func newDecisionEnv(t *testing.T, age time.Duration) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages:    decisionStages(),
		StartTime: time.Now().Add(-age),
		Yolo:      boolPtr(false),
	})
}

// TestSimDecisionPendingFirstBoardAfterRestartAndOneEventPerEpisode: a Specify
// item completed 68 days ago, no autonomy, is on the first board a freshly
// started daemon serves, aged from the GitHub label time (not from the daemon's
// first sight), and is announced exactly once per episode — across polls, over
// several stall thresholds, and across a restart.
func TestSimDecisionPendingFirstBoardAfterRestartAndOneEventPerEpisode(t *testing.T) {
	t.Parallel()
	// A lower-bound announcement must never preempt the exact one.
	const age = 68 * 24 * time.Hour
	env := newDecisionEnv(t, age)
	defer env.Engine.SetDecisionAnchorGraceForTest(time.Hour)()
	num := FileIssue(t, env, "pending decision", "Specify is done; nothing advances it.", "Specify", "stage:Specify:complete")
	env.Clock.Advance(age) // the label is now 68 days old

	dir := t.TempDir()
	_, sink := startChannelAt(t, env, dir)

	RunPoll(t, env)
	board, err := env.Engine.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewAttention})
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Attention) != 1 || board.Attention[0].State != "suspended" || board.Attention[0].Code != "decision-pending" {
		t.Fatalf("first served board = %+v", board.Attention)
	}
	p := board.Attention[0].Pending
	if p == nil || p.SinceBasis != "github-label" || !p.AgeSeconds.Valid ||
		time.Duration(p.AgeSeconds.V)*time.Second < age-time.Hour || time.Duration(p.AgeSeconds.V)*time.Second > age+time.Hour {
		t.Fatalf("pending = %+v, want an age of about 68 days anchored on the GitHub label time", p)
	}
	if na := board.Attention[0].NextAction; !strings.Contains(na, "advance to Research") || !strings.Contains(na, "cruise") {
		t.Errorf("next action %q must name the decision", na)
	}
	if board.Attention[0].NextAt.Valid {
		t.Errorf("next_at must be absent for a pending decision")
	}

	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.DecisionPending, num)) > 0
	}, 10)
	// Several stall thresholds pass; polls and ticks run; nothing is repeated and
	// nothing is announced as stalled.
	for i := 0; i < 6; i++ {
		env.Clock.Advance(31 * time.Minute)
		RunPoll(t, env)
		settleWait(env)
	}
	if got := sink.ofType(channelevents.DecisionPending, num); len(got) != 1 {
		t.Fatalf("decision-pending published %d times, want exactly 1: %+v", len(got), got)
	}
	if got := sink.ofType(channelevents.Stalled, num); len(got) != 0 {
		t.Fatalf("a pending decision was announced as stalled: %+v", got)
	}
	ev := sink.ofType(channelevents.DecisionPending, num)[0]
	if ev.Meta["since_basis"] != "github-label" || ev.Meta["next_stage"] != "Research" || ev.Meta["options"] == "" {
		t.Fatalf("event = %+v", ev.Meta)
	}

	// A restart: a fresh engine on the restored board, the same channel state dir.
	env.Engine.StopChannelEventsForTest()
	env2 := RestartEnv(t, env)
	_, sink2 := startChannelAt(t, env2, dir)
	RunPoll(t, env2)
	board2, err := env2.Engine.LocalAPIBackend().Board(localapi.BoardParams{View: localapi.ViewAttention})
	if err != nil {
		t.Fatal(err)
	}
	if len(board2.Attention) != 1 || board2.Attention[0].State != "suspended" {
		t.Fatalf("the first board after a restart must list the pending decision: %+v", board2.Attention)
	}
	if p := board2.Attention[0].Pending; p == nil || !p.AgeSeconds.Valid || time.Duration(p.AgeSeconds.V)*time.Second < age {
		t.Fatalf("after a restart the age must still be the GitHub age, not daemon-observed: %+v", p)
	}
	for i := 0; i < 4; i++ {
		env2.Clock.Advance(31 * time.Minute)
		RunPoll(t, env2)
		settleWait(env2)
	}
	if got := sink2.ofType(channelevents.DecisionPending, num); len(got) != 0 {
		t.Fatalf("a restart re-announced an episode already announced: %+v", got)
	}
}

// TestSimDecisionPendingNeutralised shows the scenario above is not vacuous.
func TestSimDecisionPendingNeutralised(t *testing.T) {
	t.Parallel()
	env := newDecisionEnv(t, time.Hour)
	defer env.Engine.SetDecisionPendingDisabledForTest()()
	num := FileIssue(t, env, "pending decision", "x", "Specify", "stage:Specify:complete")
	_, sink := startChannelAt(t, env, t.TempDir())
	RunPolls(t, env, 3)
	settleWait(env)
	if got := sink.ofType(channelevents.DecisionPending, num); len(got) != 0 {
		t.Fatalf("seam on, yet %d events", len(got))
	}
}

// TestSimDecisionPendingReminders: with no reminder configured nothing repeats
// over many stall thresholds; with remind_after the decision is re-sent at that
// interval, marked as a reminder, with the original's attributes and comment link.
func TestSimDecisionPendingReminders(t *testing.T) {
	t.Parallel()
	env := newDecisionEnv(t, 2*time.Hour)
	defer env.Engine.SetDecisionAnchorGraceForTest(time.Hour)()
	num := FileIssue(t, env, "pending decision", "x", "Specify", "stage:Specify:complete")
	env.Clock.Advance(2 * time.Hour)
	hub, plain := startChannelAt(t, env, t.TempDir())
	none := []string{}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "R", ExcludeLabels: &none, RemindAfterSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	remind := &simSink{}
	hub.Attach("R", remind)

	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(plain.ofType(channelevents.DecisionPending, num)) > 0 && len(remind.ofType(channelevents.DecisionPending, num)) > 0
	}, 20)
	orig := remind.ofType(channelevents.DecisionPending, num)[0]

	// 30 minutes in, nothing is due yet.
	env.Clock.Advance(30 * time.Minute)
	settleWait(env)
	if got := remind.ofType(channelevents.DecisionPending, num); len(got) != 1 {
		t.Fatalf("reminder before the interval: %d events", len(got))
	}
	// One interval later the subscriber that asked is reminded; the other is not.
	env.Clock.Advance(31 * time.Minute)
	RunPoll(t, env)
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(remind.ofType(channelevents.DecisionPending, num)) >= 2
	}, 10)
	got := remind.ofType(channelevents.DecisionPending, num)
	rem := got[len(got)-1]
	if rem.Meta["reminder"] != "true" || rem.Meta["reminder_count"] != "1" {
		t.Fatalf("not marked as a reminder: %+v", rem.Meta)
	}
	for _, k := range []string{"column", "completed_stage", "next_stage", "decision", "options", "since", "since_basis"} {
		if rem.Meta[k] != orig.Meta[k] {
			t.Errorf("reminder meta %q = %q, original %q", k, rem.Meta[k], orig.Meta[k])
		}
	}
	if rem.CommentURL != orig.CommentURL {
		t.Errorf("reminder comment link %q, original %q", rem.CommentURL, orig.CommentURL)
	}
	// Over many more stall thresholds the plain subscriber still has exactly one.
	for i := 0; i < 5; i++ {
		env.Clock.Advance(31 * time.Minute)
		RunPoll(t, env)
		settleWait(env)
	}
	if got := plain.ofType(channelevents.DecisionPending, num); len(got) != 1 {
		t.Fatalf("a subscriber with no remind_after got %d events, want 1", len(got))
	}
	if got := plain.ofType(channelevents.Stalled, num); len(got) != 0 {
		t.Fatalf("stalled published for a pending decision")
	}
}

// rebaseResolveScript is the rebase reinvoke's comment script when the rebase
// goes well: it merges origin/main into the PR branch, keeping the branch's own
// side of any conflict, and pushes.
func rebaseResolveScript() simclaude.CommentScript {
	return func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		run := func(args ...string) error {
			cmd := exec.Command("git", args...)
			cmd.Dir = workDir
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), out, err)
			}
			return nil
		}
		for _, args := range [][]string{
			{"fetch", "origin"},
			{"-c", "user.name=simclaude", "-c", "user.email=simclaude@fabrik.test", "merge", "-X", "ours", "--no-edit", "origin/main"},
			{"push", "origin", fmt.Sprintf("HEAD:refs/heads/fabrik/issue-%d", issue.Number)},
		} {
			if err := run(args...); err != nil {
				return "", false, engine.TokenUsage{}, err
			}
		}
		return "FABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.01, TurnsUsed: 2, MaxTurns: 50}, nil
	}
}

// TestSimSettlementWithdrawnWhenTheBaseMoves: an item settled at Validate under
// cruise announces validate-settled; when main then moves and the PR needs a
// rebase, exactly one settlement-withdrawn event says it is no longer ready (with
// the reason), and a fresh validate-settled follows once it resettles.
func TestSimSettlementWithdrawnWhenTheBaseMoves(t *testing.T) {
	t.Parallel()
	env := newChannelGateEnv(t, false, func(cfg *engine.Config) { cfg.MaxRebaseCycles = 3 })
	env.Claude.ForStageComments("Validate", rebaseResolveScript())
	_, sink := startChannel(t, env)

	num := FileIssue(t, env, "unsettle", "Settle under cruise, then the base moves.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.ValidateSettled, num)) == 1
	}, 40)

	// Main moves underneath the PR: a conflicting commit on the same path.
	env.Sim.Sim().SeedCommit(env.OwnerRepo, "main", map[string]string{conflictingPath: "external change on main\n"}, "external: conflicting change")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedCommit: %v", err)
	}
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.SettlementWithdrawn, num)) > 0
	}, 40)
	w := sink.ofType(channelevents.SettlementWithdrawn, num)
	// The sim engine has no write-through board cache, so the store learns of the
	// rebase only as the worker the engine dispatches for it (production, with the
	// cache, sees fabrik:rebase-needed itself and says "rebase-needed"). Either
	// is a reason on the closed list and means the item is no longer ready.
	if len(w) != 1 || w[0].Meta["withdrew"] != "validate-settled" ||
		(w[0].Meta["reason"] != "rebase-needed" && w[0].Meta["reason"] != "worker-dispatched") {
		t.Fatalf("withdrawal = %+v", w)
	}

	// The rebase lands; CI runs on the new head and the item resettles.
	AdvanceUntil(t, env, func(env *Env) bool {
		cur, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
		if err != nil || cur == nil || cur.HeadSHA == pr.HeadSHA {
			return false
		}
		env.Sim.Sim().SeedCheckRun(env.OwnerRepo, cur.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
		return true
	}, 80)
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.ValidateSettled, num)) == 2
	}, 80)
	RunPolls(t, env, 6)
	settleWait(env)
	if n := len(sink.ofType(channelevents.SettlementWithdrawn, num)); n != 1 {
		t.Fatalf("%d withdrawals across the whole run, want exactly 1", n)
	}
	if n := len(sink.ofType(channelevents.ValidateSettled, num)); n != 2 {
		t.Fatalf("%d validate-settled events, want 2 (one per episode)", n)
	}
}
