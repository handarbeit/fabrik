package sim

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/channelevents"
)

// Channel-event scenarios (#1968, ADR-1966-b). They run the real engine through
// PollOnce with the channel hub started via the engine's test seam, so the
// validate-settled anchor (runCatchUpPhase2's settle point) is exercised against
// the real gate chain rather than a hand-built snapshot.
//
// These tests are deliberately NOT t.Parallel: SetChannelTimingForTest changes
// package-level timing the deriver reads, and serial tests finish before any
// parallel test resumes.

type simSink struct {
	mu  sync.Mutex
	got []channelevents.Event
}

func (s *simSink) Deliver(ev channelevents.Event) error {
	s.mu.Lock()
	s.got = append(s.got, ev)
	s.mu.Unlock()
	return nil
}
func (s *simSink) Superseded() {}

func (s *simSink) all() []channelevents.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelevents.Event(nil), s.got...)
}

func (s *simSink) ofType(typ channelevents.EventType, issue int) []channelevents.Event {
	var out []channelevents.Event
	for _, ev := range s.all() {
		if ev.Type == typ && (issue == 0 || ev.Issue == issue) {
			out = append(out, ev)
		}
	}
	return out
}

// startChannel starts the hub, subscribes "S" to everything (all label churn
// included) and attaches a sink.
func startChannel(t *testing.T, env *Env) (*channelevents.Hub, *simSink) {
	t.Helper()
	restore := engine.SetChannelTimingForTest(20*time.Millisecond, time.Hour)
	t.Cleanup(restore)
	hub := env.Engine.StartChannelEventsForTest(t.TempDir())
	if hub == nil {
		t.Fatal("channel hub did not start")
	}
	t.Cleanup(env.Engine.StopChannelEventsForTest)
	none := []string{}
	if _, err := hub.Subscribe(channelevents.Subscription{Subscriber: "S", ExcludeLabels: &none}); err != nil {
		t.Fatal(err)
	}
	sink := &simSink{}
	hub.Attach("S", sink)
	return hub, sink
}

// settleWait gives the consumer goroutine a moment to publish after a poll.
func settleWait() { time.Sleep(150 * time.Millisecond) }

func newChannelGateEnv(t *testing.T, yolo bool, mod func(*engine.Config)) *Env {
	t.Helper()
	env := NewEnv(t, EnvOptions{
		Stages:    conjunctiveGateStages(),
		StartTime: time.Now(),
		Yolo:      boolPtr(yolo),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = 15 * time.Minute
			cfg.MaxReviewCycles = 5
			if mod != nil {
				mod(cfg)
			}
		},
	})
	env.Sim.Sim().SeedRepoAccess(env.OwnerRepo, gh.RepoAccess{AllowAutoMerge: false, CanPush: true})
	env.Sim.Sim().SeedRequiredContexts(env.OwnerRepo, "main", []string{conjunctiveGateCheck})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return env
}

// TestChannelValidateSettledCruiseWaitsForEveryGate is the headline scenario:
// under cruise with wait_for_ci and wait_for_reviews, validate-settled must NOT
// fire at FABRIK_STAGE_COMPLETE (awaiting-ci present), nor once CI clears but
// the review gate holds; it fires exactly once when both gates have cleared,
// and says a human decides next.
func TestChannelValidateSettledCruiseWaitsForEveryGate(t *testing.T) {
	env := newChannelGateEnv(t, false, nil)
	_, sink := startChannel(t, env)

	num := FileIssue(t, env, "channel validate-settled (cruise)", "Prove validate-settled waits for every gate.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "stage:Review:complete", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)

	// FABRIK_STAGE_COMPLETE has been emitted but awaiting-ci holds.
	RunPolls(t, env, 5)
	settleWait()
	if got := sink.ofType(channelevents.ValidateSettled, num); len(got) != 0 {
		t.Fatalf("validate-settled emitted while fabrik:awaiting-ci is present: %+v", got)
	}

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil || pr.Number == 0 {
		t.Fatalf("expected a linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})

	// CI clears; the review gate now holds.
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-ci", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	RunPolls(t, env, 8)
	settleWait()
	if got := sink.ofType(channelevents.ValidateSettled, num); len(got) != 0 {
		t.Fatalf("validate-settled emitted while the review gate holds: %+v", got)
	}

	// The review arrives; both gates clear.
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(sink.ofType(channelevents.ValidateSettled, num)) > 0
	}, 40)

	RunPolls(t, env, 10)
	settleWait()
	got := sink.ofType(channelevents.ValidateSettled, num)
	if len(got) != 1 {
		t.Fatalf("want exactly one validate-settled, got %d: %+v", len(got), got)
	}
	ev := got[0]
	if ev.PR != pr.Number || ev.Meta["next"] != "waiting-for-human" || ev.Meta["autonomy"] != "cruise" {
		t.Fatalf("unexpected meta: %+v", ev)
	}
	// The sim delivers no check_run webhooks, so the cache holds no runs: the
	// verdict is honestly unknown, and the engine's own CI gate state is reported.
	if ev.Meta["ci"] != "unknown" || ev.Meta["ci_gate"] != "cleared" {
		t.Errorf("ci = %q ci_gate = %q, want unknown/cleared", ev.Meta["ci"], ev.Meta["ci_gate"])
	}
	if ev.Meta["reviews"] == "" || ev.Meta["reviews"] == "none" {
		t.Errorf("reviews summary missing the approval: %q", ev.Meta["reviews"])
	}
	if projectItem(t, env, num).IsClosed {
		t.Fatal("cruise must not land the PR")
	}
}

// TestChannelValidateSettledYoloNextIsAutoMerge: under yolo without the merge
// train the settle event is captured before the engine lands the PR in the very
// same pass, and says the engine will auto-merge.
func TestChannelValidateSettledYoloNextIsAutoMerge(t *testing.T) {
	env := newChannelGateEnv(t, true, nil)
	_, sink := startChannel(t, env)

	num := FileIssue(t, env, "channel validate-settled (yolo)", "Yolo settle.", "Implement", "fabrik:yolo")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	WaitForIssueClosed(t, env, num, 80)
	settleWait()

	got := sink.ofType(channelevents.ValidateSettled, num)
	if len(got) != 1 {
		t.Fatalf("want exactly one validate-settled before the merge, got %d", len(got))
	}
	if got[0].Meta["next"] != "auto-merge" || got[0].Meta["autonomy"] != "yolo" {
		t.Fatalf("unexpected meta: %+v", got[0].Meta)
	}
}

// TestChannelValidateSettledAgainAfterRevalidate: fabrik:revalidate starts a new
// settle episode, announced again once the item re-settles.
func TestChannelValidateSettledAgainAfterRevalidate(t *testing.T) {
	env := newChannelGateEnv(t, false, nil)
	_, sink := startChannel(t, env)

	num := FileIssue(t, env, "channel validate-settled revalidate", "Re-settle.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(sink.ofType(channelevents.ValidateSettled, num)) == 1
	}, 40)

	// Operator forces re-entry of Validate.
	if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, num, "fabrik:revalidate"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	// Validate re-runs and (the sim worker pushes a commit) the head moves, so
	// CI must be seeded green on the new head for the item to settle again.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr2, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr2 == nil {
		t.Fatalf("linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr2.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	AdvanceUntil(t, env, func(*Env) bool {
		settleWait()
		return len(sink.ofType(channelevents.ValidateSettled, num)) == 2
	}, 80)
	RunPolls(t, env, 6)
	settleWait()
	if n := len(sink.ofType(channelevents.ValidateSettled, num)); n != 2 {
		t.Fatalf("want exactly two validate-settled events across two episodes, got %d", n)
	}
}

// runCruiseToSettled drives the conjunctive-gate cruise scenario to its settled
// state and returns the ordered mutation signatures the engine made, plus the
// final labels. Used by the R10 parity test.
func runCruiseToSettled(t *testing.T, withHub bool) (mutations []string, labels []string) {
	t.Helper()
	env := newChannelGateEnv(t, false, nil)
	if withHub {
		startChannel(t, env)
	}
	num := FileIssue(t, env, "channel parity", "Parity.", "Implement", "fabrik:cruise")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-ci", 80)
	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil || pr == nil {
		t.Fatalf("linked PR: %v", err)
	}
	env.Sim.Sim().SeedCheckRun(env.OwnerRepo, pr.HeadSHA, gh.CheckRun{Name: conjunctiveGateCheck, Status: "completed", Conclusion: "success"})
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	env.Sim.Sim().SeedReview(env.OwnerRepo, pr.Number, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-review", 80)
	RunPolls(t, env, 10)
	for _, m := range env.Sim.Log().Mutations() {
		// Commit SHAs and generated IDs differ between runs; the method, target
		// issue, label and free-form values are the engine's decisions.
		mutations = append(mutations, m.Method+" #"+strconvItoa(m.Args.Number)+" "+m.Args.Label+" "+strings.Join(m.Args.Values, ","))
	}
	return mutations, IssueLabels(t, env, num)
}

// TestChannelEventsAreObservationOnly (R10): a run with the channel hub and a
// subscriber makes exactly the same GitHub mutations, in the same order, and
// ends in the same label state as a run without any hub.
func TestChannelEventsAreObservationOnly(t *testing.T) {
	baseMut, baseLabels := runCruiseToSettled(t, false)
	hubMut, hubLabels := runCruiseToSettled(t, true)
	if strings.Join(baseMut, "\n") != strings.Join(hubMut, "\n") {
		t.Fatalf("the hub changed engine mutations\n--- without hub ---\n%s\n--- with hub ---\n%s",
			strings.Join(baseMut, "\n"), strings.Join(hubMut, "\n"))
	}
	if strings.Join(sortedCopy(baseLabels), ",") != strings.Join(sortedCopy(hubLabels), ",") {
		t.Fatalf("final labels differ: %v vs %v", baseLabels, hubLabels)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func strconvItoa(n int) string { return strconv.Itoa(n) }
