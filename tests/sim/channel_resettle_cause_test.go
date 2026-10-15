package sim

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Re-settle cause (#2169 R3a). A validate-settled after the first says why the
// item settled again, so a subscriber on a semantic-only feed can tell a human's
// merge decision from a fresh settle.

// settleCruiseItem drives an item to its first validate-settled under cruise and
// returns its number and the first event.
func settleCruiseItem(t *testing.T, env *Env, sink *simSink, title string) (int, channelevents.Event) {
	t.Helper()
	num := FileIssue(t, env, title, "Re-settle.", "Implement", "fabrik:cruise")
	driveToSettled(t, env, num)
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.ValidateSettled, num)) == 1
	}, 40)
	RunPolls(t, env, 4)
	settleWait(env)
	got := sink.ofType(channelevents.ValidateSettled, num)
	if len(got) != 1 {
		t.Fatalf("want exactly one first settle, got %d", len(got))
	}
	return num, got[0]
}

// driveToSettled takes a filed cruise item through the conjunctive gates (CI
// green, one approving review) so it reaches the settle point.
func driveToSettled(t *testing.T, env *Env, num int) {
	t.Helper()
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	// The sim has no pull_request webhook feed, so give the store the head a
	// real daemon would hold.
	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, pr.Number, pr.HeadSHA)
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
}

// settleOnCruise drives a filed cruise item to settled and lets the consumer publish.
func settleOnCruise(t *testing.T, env *Env, num int) {
	t.Helper()
	driveToSettled(t, env, num)
	RunPolls(t, env, 12)
	settleWait(env)
}

// swapAutonomy replaces the cruise label with yolo the way a human would: on
// GitHub, and as the webhook the engine would receive, naming the sender.
func swapAutonomy(t *testing.T, env *Env, num int, sender string) {
	t.Helper()
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:yolo"); err != nil {
		t.Fatal(err)
	}
	if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, num, "fabrik:cruise"); err != nil {
		t.Fatal(err)
	}
	env.Engine.ApplyLabelWebhookForTest(env.OwnerRepo, num, "fabrik:yolo", true, sender)
	env.Engine.ApplyLabelWebhookForTest(env.OwnerRepo, num, "fabrik:cruise", false, sender)
}

// A human swaps cruise for yolo on a settled item: the item settles again at the
// same head, and the second validate-settled says why and who.
func TestChannelReSettleCauseAutonomyChangedByHuman(t *testing.T) {
	t.Parallel()
	env := newChannelGateEnv(t, false, nil)
	_, sink := startChannel(t, env)
	num, first := settleCruiseItem(t, env, sink, "re-settle cause: autonomy")

	if _, has := first.Meta[channelevents.MetaCause]; has {
		t.Fatalf("the first settle of an episode carries no cause: %+v", first.Meta)
	}
	if first.Meta["autonomy"] != "cruise" || first.Meta["head_sha"] == "" {
		t.Fatalf("first settle meta: %+v", first.Meta)
	}

	swapAutonomy(t, env, num, "alice")
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.ValidateSettled, num)) >= 2
	}, 40)
	got := sink.ofType(channelevents.ValidateSettled, num)
	if len(got) != 2 {
		t.Fatalf("want exactly two validate-settled, got %d: %+v", len(got), got)
	}
	second := got[1]
	if second.Meta["head_sha"] != first.Meta["head_sha"] {
		t.Errorf("head_sha changed: %q -> %q (the swap must not move the head)", first.Meta["head_sha"], second.Meta["head_sha"])
	}
	if second.Meta["cause"] != "autonomy-changed" || second.Meta["actor"] != "human" || second.Meta["actor_login"] != "alice" {
		t.Errorf("second settle: cause=%q actor=%q login=%q, want autonomy-changed/human/alice (%+v)",
			second.Meta["cause"], second.Meta["actor"], second.Meta["actor_login"], second.Meta)
	}
	if second.Meta["autonomy"] != "yolo" {
		t.Errorf("second settle autonomy = %q, want yolo", second.Meta["autonomy"])
	}
	if second.Meta["source"] != channelevents.SourceLive {
		t.Errorf("source = %q, want live", second.Meta["source"])
	}
}

// Non-vacuity: before #2169 the swap produced no second validate-settled at all
// (the open episode suppressed it), so there was no cause and no actor for a
// semantic-only subscriber to see. With the cause neutralised the swap is silent.
func TestChannelReSettleCauseNonVacuous(t *testing.T) {
	t.Parallel()
	env := newChannelGateEnv(t, false, nil)
	defer env.Engine.SetSettleCauseDisabledForTest()()
	_, sink := startChannel(t, env)
	num, _ := settleCruiseItem(t, env, sink, "re-settle cause: non-vacuous")
	swapAutonomy(t, env, num, "alice")
	RunPolls(t, env, 10)
	settleWait(env)
	for _, ev := range sink.ofType(channelevents.ValidateSettled, num) {
		if ev.Meta["cause"] != "" || ev.Meta["actor"] != "" {
			t.Fatalf("neutralised: no event may carry a cause or actor: %+v", ev.Meta)
		}
	}
	if n := len(sink.ofType(channelevents.ValidateSettled, num)); n != 1 {
		t.Fatalf("before #2169 the swap produced no second settle; got %d events", n)
	}
}

// A revalidated item that settles again says so; the head moves too (the sim
// worker pushes a commit), and revalidated outranks new-head.
func TestChannelReSettleCauseRevalidated(t *testing.T) {
	t.Parallel()
	env := newChannelGateEnv(t, false, nil)
	_, sink := startChannel(t, env)
	num, first := settleCruiseItem(t, env, sink, "re-settle cause: revalidate")

	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:revalidate"); err != nil {
		t.Fatal(err)
	}
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	env.Engine.SeedLinkedPRHeadForTest(env.OwnerRepo, num, pr.Number, pr.HeadSHA)
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait(env)
		return len(sink.ofType(channelevents.ValidateSettled, num)) >= 2
	}, 80)
	got := sink.ofType(channelevents.ValidateSettled, num)
	second := got[1]
	if second.Meta["cause"] != "revalidated" {
		t.Errorf("cause = %q, want revalidated (first head %q, second %q): %+v", second.Meta["cause"], first.Meta["head_sha"], second.Meta["head_sha"], second.Meta)
	}
}
