package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
)

// Sim twins for #2532 (ADR 2532): admission cooldowns are classified as timers,
// event-replaceable re-checks and the safety net, and scheduled accordingly.
//
// The sim has no transport layer, so the daemon's webhook health is a scripted
// engine.StubTransport. Cadence is observed through PollBackoffResult.NextInterval
// of a deeply idle daemon (SetIdleStartForTest: the idle backoff is wall-clock
// based). The Env polls every 1 s, so today's cadence for a long cooldown is
// 10 × 1 s + one interval = 11 s; the stretched cadence is the 30-minute
// safety net. Each scenario runs again with its neutralisation seam and must flip.

const (
	todaysCadenceCeiling = 15 * time.Second // 10 × poll + one interval, with slack
	safetyNetFloor       = 20 * time.Minute // far above today's cadence, below the 30 m default
	safetyNetCeiling     = 31 * time.Minute // 30 m + one interval, with slack
)

// stubbedDependent is blockedOnBlocker with a scripted transport attached BEFORE
// the first poll, so the cooldowns the polls stamp already follow its health.
func stubbedDependent(t *testing.T, healthy bool, tune func(*engine.Engine)) (env *Env, stub *engine.StubTransport, dependent, blocker int) {
	t.Helper()
	env = NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers())
	stub = engine.NewStubTransport()
	stub.SetHealthy(healthy)
	env.Engine.AttachStubTransportForTest(stub)
	if tune != nil {
		tune(env.Engine)
	}
	blocker = FileIssue(t, env, "blocker", "Open blocker that stays open.", "Backlog")
	dependent = FileIssue(t, env, "dependent", "Blocked on the blocker.", "Specify", "fabrik:blocked")
	env.Sim.Sim().SeedBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	RunPolls(t, env, 2)
	st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, dependent)
	if !ok || st.CooldownAt["dep-blocked"].IsZero() {
		t.Fatal("precondition: the dependent was never evaluated, so no dep-blocked cooldown exists")
	}
	return env, stub, dependent, blocker
}

// idleInterval polls once with the daemon deeply idle and returns the next interval.
func idleInterval(t *testing.T, env *Env) time.Duration {
	t.Helper()
	env.Engine.SetIdleStartForTest(time.Now().Add(-time.Hour))
	return runPollWithBackoff(t, env, env.PollInterval).NextInterval
}

// (a) An idle, webhook-healthy daemon holding an item on an event-replaceable
// cooldown (dep-blocked, whose blocker is cached) does not poll at the old
// cadence, and the event admits the item promptly. Neutralised, it polls at the
// old cadence.
func TestCooldownClasses_HealthyIdleDaemonStretchesEventReplaceable(t *testing.T) {
	t.Parallel()
	for _, stretch := range []bool{true, false} {
		stretch := stretch
		name := "stretch"
		if !stretch {
			name = "nonvacuity-stretch-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, dependent, blocker := stubbedDependent(t, true, func(e *engine.Engine) {
				e.SetEventReplaceableStretchDisabledForTest(!stretch)
			})
			got := idleInterval(t, env)
			if stretch && got < safetyNetFloor {
				t.Fatalf("healthy idle daemon polls again in %v; want the safety-net cadence (>= %v)", got, safetyNetFloor)
			}
			if !stretch && got > todaysCadenceCeiling {
				t.Fatalf("non-vacuity: neutralised, the old cadence must return, got %v", got)
			}
			if !stretch {
				return
			}
			// The event admits the item promptly, long before the stretched interval.
			env.Sim.Sim().SeedRemoveBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedRemoveBlockedBy: %v", err)
			}
			drainWake(env)
			env.BoardCache.ApplyDelta("issue_dependencies", dependencyPayload("blocked_by_removed", env.OwnerRepo, dependent, blocker))
			if !woken(env) {
				t.Fatal("the delivery must wake the engine")
			}
			RunPoll(t, env)
			if hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
				t.Fatal("the event must unblock the dependent on the next wake")
			}
		})
	}
}

// (b) The same daemon with the transport unhealthy, and a poll-only daemon, keep
// today's cadence.
func TestCooldownClasses_UnhealthyTransportKeepsTodaysCadence(t *testing.T) {
	t.Parallel()
	t.Run("unhealthy", func(t *testing.T) {
		t.Parallel()
		env, _, _, _ := stubbedDependent(t, false, nil)
		if got := idleInterval(t, env); got > time.Minute {
			t.Fatalf("unhealthy transport: next poll in %v; want today's cadence (about 11 s)", got)
		}
	})
	t.Run("unsubscribed-event", func(t *testing.T) {
		t.Parallel()
		env, stub, dependent, _ := stubbedDependent(t, true, nil)
		st, _ := env.Engine.StoreItemForTest(env.OwnerRepo, dependent)
		depDue := st.CooldownAt["dep-blocked"]
		// periodic-re-eval is stamped the safety-net interval ahead (healthy), so
		// the earliest long cooldown is the dep-blocked due only when dep-blocked
		// counts toward the idle cap, i.e. when it is not stretched.
		if got := env.Engine.EarliestLongCooldownForTest(); got.Equal(depDue) {
			t.Fatal("precondition: with issue_dependencies delivered, dep-blocked must be stretched")
		}
		stub.SetSubscribed("issue_comment") // issue_dependencies is not delivered
		if got := env.Engine.EarliestLongCooldownForTest(); !got.Equal(depDue) {
			t.Fatalf("dep-blocked must count toward the idle cap when its event is not subscribed: earliest = %v, dep-blocked due = %v", got, depDue)
		}
	})
	t.Run("poll-only", func(t *testing.T) {
		t.Parallel()
		env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true})
		t.Cleanup(env.Engine.RegisterObservers())
		blocker := FileIssue(t, env, "blocker", "Open.", "Backlog")
		dep := FileIssue(t, env, "dependent", "Blocked.", "Specify", "fabrik:blocked")
		env.Sim.Sim().SeedBlockedBy(env.OwnerRepo, dep, env.OwnerRepo, blocker)
		RunPolls(t, env, 2)
		if got := idleInterval(t, env); got > time.Minute {
			t.Fatalf("poll-only: next poll in %v; want today's cadence", got)
		}
	})
}

// (c) A timer deadline is acted on within about one poll interval of the deadline
// while the daemon is idle. The lock-orphan grace has no GitHub event: a
// fabrik:editing label with no worker is reclaimed once the grace runs out, and
// the idle daemon, though the transport is healthy and the safety net is 30
// minutes away, polls again at the deadline. Neutralised, the next poll is the
// safety net's.
func TestCooldownClasses_TimerWakesIdleDaemonAtItsDeadline(t *testing.T) {
	t.Parallel()
	for _, timers := range []bool{true, false} {
		timers := timers
		name := "timer"
		if !timers {
			name = "nonvacuity-timer-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true})
			t.Cleanup(env.Engine.RegisterObservers())
			env.Engine.AttachStubTransportForTest(engine.NewStubTransport())
			env.Engine.SetTimerWakeDisabledForTest(!timers)
			num := FileIssue(t, env, "orphan lock", "A lock label with no worker.", "Specify", "fabrik:editing")
			RunPolls(t, env, 2)
			got := idleInterval(t, env)
			grace := 3 * time.Minute // max(3m, 3 × poll)
			if timers {
				if got > grace+2*env.PollInterval {
					t.Fatalf("idle daemon schedules its next poll in %v; the grace runs out within %v", got, grace)
				}
				env.Clock.Advance(got)
				RunPoll(t, env)
				if hasLabel(IssueLabels(t, env, num), "fabrik:editing") {
					t.Fatal("the poll scheduled at the deadline must act on it (reclaim the orphaned lock)")
				}
			} else if got < safetyNetFloor {
				t.Fatalf("non-vacuity: without the timer the poll must wait for the safety net, got %v", got)
			}
		})
	}
}

// (c2) The review wait is a timer too: while a head restarts the wait, the
// deadline (head arrival + the window) is registered, and nothing is when timers
// are neutralised. (The item awaiting review is poll activity, so the idle cap is
// not observable here; the registry is.)
func TestCooldownClasses_ReviewWaitRegistersItsDeadline(t *testing.T) {
	t.Parallel()
	for _, timers := range []bool{true, false} {
		timers := timers
		name := "timer"
		if !timers {
			name = "nonvacuity-timer-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env := newHeadAnchorEnv(t, 5)
			env.Engine.SetTimerWakeDisabledForTest(!timers)
			num, prNum := seedWaitingOnHuman(t, env, "timer-registry-"+name)
			seedHeadTime(t, env, headOf(t, env, prNum), time.Now().Add(-14*time.Minute))
			WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
			RunPolls(t, env, 2)
			due, now := env.Engine.EarliestTimerDeadlineForTest()
			if !timers {
				if !due.IsZero() {
					t.Fatalf("non-vacuity: a neutralised registry records nothing, got %v", due)
				}
				return
			}
			if due.IsZero() || due.Sub(now) > 2*time.Minute || due.Before(now) {
				t.Fatalf("review-wait deadline = %v (engine now %v); want within two minutes, about one minute out", due, now)
			}
		})
	}
}

// (d) A dropped event is recovered by the safety net within its interval: the
// blocker edge is removed on GitHub, no delivery arrives, and the idle daemon
// still polls again within the safety-net interval and unblocks the item.
// Neutralised, the next poll is the idle cap, an hour away.
func TestCooldownClasses_DroppedEventRecoveredBySafetyNet(t *testing.T) {
	t.Parallel()
	for _, net := range []bool{true, false} {
		net := net
		name := "safety-net"
		if !net {
			name = "nonvacuity-safety-net-disabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, _, dependent, blocker := stubbedDependent(t, true, func(e *engine.Engine) {
				e.SetSafetyNetDisabledForTest(!net)
			})
			env.Sim.Sim().SeedRemoveBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedRemoveBlockedBy: %v", err)
			}
			// No delivery: the event was dropped.
			got := idleInterval(t, env)
			if !net {
				if got <= safetyNetCeiling {
					t.Fatalf("non-vacuity: without the safety net the next poll is the idle cap, got %v", got)
				}
				return
			}
			if got < safetyNetFloor || got > safetyNetCeiling {
				t.Fatalf("next poll in %v; want the safety-net interval (%v..%v)", got, safetyNetFloor, safetyNetCeiling)
			}
			env.Clock.Advance(got)
			RunPoll(t, env)
			if hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
				t.Fatal("the poll the safety net scheduled must recover the dropped event")
			}
		})
	}
}

// FR-007: ingestion turns unreliable while a stretched cooldown is outstanding;
// today's cadence returns within about one poll, not after the stretched interval.
func TestCooldownClasses_LostReliabilityRevertsStretchedCadence(t *testing.T) {
	t.Parallel()
	env, stub, _, _ := stubbedDependent(t, true, nil)
	if got := idleInterval(t, env); got < safetyNetFloor {
		t.Fatalf("precondition: stretched cadence expected, got %v", got)
	}
	stub.SetHealthy(false)
	prev := map[string]bool{env.OwnerRepo: true}
	env.Engine.HealthWatchTickForTest(prev)
	if got := idleInterval(t, env); got > time.Minute {
		t.Fatalf("after losing reliability the next poll is in %v; want today's cadence", got)
	}
}
