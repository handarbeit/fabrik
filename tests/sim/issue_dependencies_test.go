package sim

import (
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// The issue_dependencies webhook (#1995, ADR 1995), end to end through the real
// webhook decode, the real boardcache and the real Store.
//
// A dependency change must make the affected item re-evaluate on the next wake,
// not after the dep-blocked cooldown (PollSeconds*10) or the next poll interval.
// The event is only a trigger: whether fabrik:blocked is removed (or applied) is
// still decided by checkDependencies' live read (ADR 1419), so a stale delivery
// changes nothing.

func dependencyPayload(action, ownerRepo string, blocked, blocking int) []byte {
	url := func(n int) string { return fmt.Sprintf("https://github.com/%s/issues/%d", ownerRepo, n) }
	return []byte(fmt.Sprintf(
		`{"action":%q,"blocked_issue":{"number":%d,"html_url":%q},"blocking_issue":{"number":%d,"html_url":%q},`+
			`"blocked_issue_id":1,"blocking_issue_id":2,"repository":{"full_name":%q},"sender":{"login":"human"}}`,
		action, blocked, url(blocked), blocking, url(blocking), ownerRepo))
}

// drainWake empties the production wake channel.
func drainWake(env *Env) {
	for {
		select {
		case <-env.Wake:
		default:
			return
		}
	}
}

func woken(env *Env) bool {
	select {
	case <-env.Wake:
		return true
	default:
		return false
	}
}

// blockedOnBlocker returns an env whose dependent is fabrik:blocked on an open
// Backlog blocker, after the first poll has stamped the dep-blocked cooldown.
func blockedOnBlocker(t *testing.T) (env *Env, dependent, blocker int) {
	t.Helper()
	env = NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true})
	// The cycle-set and push-unblock observers are production wiring NewEnv leaves
	// off by default (ADR-1592); without the cycle-set observer no store change
	// admits an item, and this scenario is about exactly that admission.
	t.Cleanup(env.Engine.RegisterObservers())
	blocker = FileIssue(t, env, "blocker", "Open blocker that stays open.", "Backlog")
	dependent = FileIssue(t, env, "dependent", "Blocked on the blocker.", "Specify", "fabrik:blocked")
	env.Sim.Sim().SeedBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// The first poll bootstraps the cache from the probe; the second evaluates the
	// item: the blocker is open, so the dep-blocked cooldown is stamped.
	RunPolls(t, env, 2)
	if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("dependent lost fabrik:blocked while its blocker is open")
	}
	st, ok := env.Engine.StoreItemForTest(env.OwnerRepo, dependent)
	if !ok || st.CooldownAt["dep-blocked"].IsZero() {
		t.Fatal("precondition: the dependent was never evaluated, so no dep-blocked cooldown exists")
	}
	return env, dependent, blocker
}

// (a) The last blocker edge is removed in GitHub's UI. The removal does not bump
// the dependent's updatedAt (#977), so nothing but the webhook tells the engine.
// With the delivery the dependent unblocks on the very next poll; without it the
// dep-blocked cooldown keeps it suspended (the control).
func TestIssueDependencies_RemovedEdgeUnblocksOnNextWake(t *testing.T) {
	t.Parallel()
	for _, deliver := range []bool{true, false} {
		deliver := deliver
		t.Run(fmt.Sprintf("delivered=%v", deliver), func(t *testing.T) {
			t.Parallel()
			env, dependent, blocker := blockedOnBlocker(t)
			env.Sim.Sim().SeedRemoveBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, blocker)
			if err := env.Sim.Sim().Err(); err != nil {
				t.Fatalf("SeedRemoveBlockedBy: %v", err)
			}
			drainWake(env)

			if deliver {
				env.BoardCache.ApplyDelta("issue_dependencies", dependencyPayload("blocked_by_removed", env.OwnerRepo, dependent, blocker))
				if !woken(env) {
					t.Fatal("the delivery must wake the engine")
				}
			}

			RunPoll(t, env)
			stillBlocked := hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked")
			if deliver && stillBlocked {
				t.Fatal("the dependent must be unblocked on the next wake, not after the dep-blocked cooldown")
			}
			if !deliver && !stillBlocked {
				t.Fatal("non-vacuity: without the delivery the cooldown must keep the dependent suspended for this poll")
			}
		})
	}
}

// (b) A stale blocked_by_removed arrives while the live read still shows an open
// blocker (the edge was re-added, or the delivery is out of date). The event
// triggers a re-evaluation, but the live read decides: the item stays blocked.
func TestIssueDependencies_StaleRemovalDoesNotUnblock(t *testing.T) {
	t.Parallel()
	env, dependent, blocker := blockedOnBlocker(t)
	drainWake(env)

	env.BoardCache.ApplyDelta("issue_dependencies", dependencyPayload("blocked_by_removed", env.OwnerRepo, dependent, blocker))
	if !woken(env) {
		t.Fatal("precondition: the delivery must wake the engine so the re-evaluation really happens")
	}

	for i := 0; i < 3; i++ {
		RunPoll(t, env)
		if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
			t.Fatalf("a stale delivery unblocked the dependent (poll %d) while the live read still shows an open blocker", i+1)
		}
	}
	if hasLabel(IssueLabels(t, env, dependent), "stage:Specify:complete") {
		t.Fatal("the dependent's stage ran while still blocked")
	}
}

// (c) FR-007: closing a blocker sends no issue_dependencies delivery, only
// issues/closed. Items whose cached BlockedBy names it are re-evaluated; others
// are not.
func TestIssueDependencies_BlockerCloseRefreshesDependents(t *testing.T) {
	t.Parallel()
	env := NewEnv(t, EnvOptions{Stages: dependencyUnblockStages(), BoardCache: true})
	t.Cleanup(env.Engine.RegisterObservers())
	env.Sim.Sim().SeedIssue(env.OwnerRepo, simgh.IssueSeed{Number: 1000, Title: "blocker", Body: "Plain repo issue, not on the board."})
	dependent := FileIssue(t, env, "dependent", "Blocked on 1000.", "Specify", "fabrik:blocked")
	env.Sim.Sim().SeedBlockedBy(env.OwnerRepo, dependent, env.OwnerRepo, 1000)
	bystander := FileIssue(t, env, "bystander", "Not related.", "Specify", "fabrik:paused", "fabrik:awaiting-input")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	RunPolls(t, env, 2)
	if !hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("precondition: dependent must be blocked")
	}

	if err := env.Sim.CloseIssue(env.Owner, env.Repo, 1000); err != nil {
		t.Fatalf("CloseIssue: %v", err)
	}
	env.BoardCache.ApplyDelta("issues", []byte(fmt.Sprintf(
		`{"action":"closed","sender":{"login":"human"},"issue":{"number":1000,"state":"closed"},"repository":{"full_name":%q}}`, env.OwnerRepo)))

	// Within one poll — far inside the dep-blocked cooldown — the dependent is
	// re-evaluated and, the live read showing no open blocker, unblocked.
	RunPoll(t, env)
	if hasLabel(IssueLabels(t, env, dependent), "fabrik:blocked") {
		t.Fatal("the dependent must be re-evaluated promptly after its blocker closed")
	}
	if !hasLabel(IssueLabels(t, env, bystander), "fabrik:paused") {
		t.Fatal("an unrelated item must be left alone")
	}
}
