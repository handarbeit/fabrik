package sim

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
)

// #2542 (ADR 2542): a merge-train trial's CI verdict is acted on within one poll interval.
//
// R1 is the guarantee: while a run has an open trial the idle interval is capped at one
// configured interval, so a poll-only engine reads the verdict promptly however long it
// has been idle. R2 is an acceleration: a completed check_run/check_suite or any
// pull_request event for the open trial's PR requests an early poll, matched in the
// engine against the persisted run record (the #2483 heal guard is untouched).
// Each scenario has a non-vacuity seam that restores today's behaviour.

const trialLatencyInterval = 30 * time.Second

// openTrial opens a trial on a two-member partition and returns the trial PR number and head.
func openTrial(t *testing.T, env *Env) (members []int, prNum int, headSHA, headRef string) {
	t.Helper()
	a, _ := QueueMember(t, env, "latency-a", map[string]string{"a.txt": "a\n"})
	b, _ := QueueMember(t, env, "latency-b", map[string]string{"b.txt": "b\n"})
	pollUntilRun(t, env, func(p []engine.TrainRunPhase) bool { return len(p) == 1 && p[0].TrialPR != 0 }, 15)
	prs, err := env.Sim.Sim().ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	for _, pr := range prs {
		if pr.State == "open" && strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
			return []int{a, b}, pr.Number, pr.HeadSHA, pr.HeadRefName
		}
	}
	t.Fatal("no open trial PR")
	return nil, 0, "", ""
}

// R1: a poll-only engine that has been idle long enough to have backed off to its maximum
// still polls again within one interval while a trial is open, and acts on the verdict.
func TestMergeTrainTrialLatency_PollOnlyCapsIdleInterval(t *testing.T) {
	t.Parallel()
	for _, capOn := range []bool{true, false} {
		capOn := capOn
		name := "cap"
		if !capOn {
			name = "nonvacuity-cap-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := mergeTrainEnv(t, mergeTrainEnvOptions{AsyncTrain: true})
			env.Engine.SetOpenTrialCapDisabledForTest(!capOn)
			members, _, _, _ := openTrial(t, env)

			// Deeply idle: the backoff is at its maximum (real wall time, hence the seam).
			env.Engine.SetIdleStartForTest(time.Now().Add(-time.Hour))
			res := runPollWithBackoff(t, env, trialLatencyInterval)
			if capOn {
				if res.NextInterval > trialLatencyInterval {
					t.Fatalf("open trial: next interval = %v, want <= %v", res.NextInterval, trialLatencyInterval)
				}
			} else if res.NextInterval <= trialLatencyInterval {
				t.Fatalf("non-vacuity: with the cap off the idle backoff returns, got %v", res.NextInterval)
			}

			// CI completes; the next poll (one capped interval later) acts on the verdict.
			if capOn {
				startTrialVerdictSeeder(t, env, allGreenVerdict)
				for _, n := range members {
					WaitForProjectStatus(t, env, n, "Done", 15)
				}
			}
		})
	}
}

// R2: the trial's check_run, delivered through the intake, requests an early poll; a
// delivery for any other merge-train PR does not; and none of it links the trial PR to a member.
func TestMergeTrainTrialLatency_CheckRunDeliveryWakesPoll(t *testing.T) {
	t.Parallel()
	for _, wakeOn := range []bool{true, false} {
		wakeOn := wakeOn
		name := "wake"
		if !wakeOn {
			name = "nonvacuity-wake-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := mergeTrainEnv(t, mergeTrainEnvOptions{AsyncTrain: true, BoardCache: true})
			env.Engine.SetTrialWakeDisabledForTest(!wakeOn)
			wake := make(chan struct{}, 8)
			env.Engine.SetWakeCh(wake)
			members, prNum, sha, ref := openTrial(t, env)
			env.Engine.EnableDeliveryIntakeForTest("")
			for len(wake) > 0 { // wakes from the Store's own changes are not under test
				<-wake
			}
			before := snapshotTrainHeal(t, env, members)

			deliver := func(id string, payload []byte) {
				t.Helper()
				env.Engine.SubmitDeliveryForTest("webhook", id, "check_run", payload)
				env.Engine.DrainDeliveriesForTest()
			}

			// A merge-train PR that is not the open trial's: a different head, then a
			// different number. Neither wakes.
			deliver("D-other-head", trainCheckRunDelivery(t, env.OwnerRepo, prNum, "0000000000000000000000000000000000000000", ref, true))
			deliver("D-other-pr", trainCheckRunDelivery(t, env.OwnerRepo, prNum+500, sha, ref, true))
			if len(wake) != 0 {
				t.Fatalf("a delivery for a non-matching merge-train PR woke the poll (%d wakes)", len(wake))
			}

			// The open trial's own completion (no PR list, as for a fork: head SHA decides).
			deliver("D-trial", trainCheckRunDelivery(t, env.OwnerRepo, prNum, sha, ref, false))
			got := len(wake)
			if wakeOn && got == 0 {
				t.Fatal("the trial's completed check_run did not request an early poll")
			}
			if !wakeOn && got != 0 {
				t.Fatalf("non-vacuity: with the wake neutralised nothing may wake, got %d", got)
			}

			// SC-003: the wake path never links the trial PR to a member (#2483).
			env.Engine.SubmitDeliveryForTest("webhook", "D-pr", "pull_request", trainPRDelivery(t, env.OwnerRepo, prNum, sha, ref))
			env.Engine.DrainDeliveriesForTest()
			after := snapshotTrainHeal(t, env, members)
			for _, n := range members {
				if before[n] != after[n] {
					t.Errorf("#%d changed by trial-PR deliveries: before=%+v after=%+v", n, before[n], after[n])
				}
			}
		})
	}
}

// R3: the shape of the 0.0.91 gate (a failed test's cleanup pauses every member while the
// trial runs). The green trial's landing is refused as ADR 2341 specifies — nothing lands,
// nothing is ejected or charged — and the run releases the partition rather than holding it;
// once the pause lifts the members land. Recorded as the correct behaviour, not an engine fault.
func TestMergeTrainTrialLatency_AllMembersPausedReleasesPartition(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, mergeTrainEnvOptions{AsyncTrain: true})
	a, _ := QueueMember(t, env, "paused-a", map[string]string{"a.txt": "a\n"})
	b, _ := QueueMember(t, env, "paused-b", map[string]string{"b.txt": "b\n"})
	members := []int{a, b}

	var once sync.Once
	pauseAll := func(m []int) []gh.CheckRun {
		once.Do(func() {
			for _, n := range members {
				for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
					if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, n, l); err != nil {
						panic(err)
					}
				}
			}
		})
		return []gh.CheckRun{greenCheckRun("")}
	}
	startTrialSeeder(t, env, pauseAll, nil)

	// The run ends (the partition is released) without landing anything.
	for i := 0; i < 25; i++ {
		RunPoll(t, env)
		if len(env.Engine.TrainRunPhases()) == 0 && hasLabel(IssueLabels(t, env, a), "fabrik:paused") {
			break
		}
	}
	if ph := env.Engine.TrainRunPhases(); len(ph) != 0 {
		t.Fatalf("a refused landing must release the partition, run still open: %+v", ph)
	}
	for _, n := range members {
		if got := projectItem(t, env, n).Status; got != "Queued" {
			t.Errorf("paused #%d status = %q, want Queued (uncharged)", n, got)
		}
		if hasCommentContaining(t, env, n, "ejected") || hasCommentContaining(t, env, n, "merge failure") {
			t.Errorf("#%d: a refusal must not eject or post a failure comment", n)
		}
		if mergedTrialPRContaining(t, env, n) {
			t.Errorf("a trial PR containing paused #%d was merged", n)
		}
	}

	// The pause lifts and the members land.
	for _, n := range members {
		for _, l := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
			if err := env.Sim.RemoveLabelFromIssue(env.Owner, env.Repo, n, l); err != nil && !errors.Is(err, gh.ErrNotFound) {
				t.Fatalf("lifting the pause: %v", err)
			}
		}
	}
	for _, n := range members {
		WaitForProjectStatus(t, env, n, "Done", 30)
	}
}
