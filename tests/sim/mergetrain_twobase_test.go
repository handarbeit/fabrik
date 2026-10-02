package sim

import (
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
)

// TestMergeTrainTwoBasesConcurrent is the sim twin of the live scenario of
// the same name (tests/e2e/mergetrain_twobase_test.go): ADR-1648's per-(repo,
// base) partitioning. Two clean members are queued on the repository default
// (main) and two on a throwaway base:<branch>; each partition must form its
// own independent train and land onto its own base, with no cross-base landing.
//
// Everything is queued before the first poll and the scenario then runs exactly
// ONE RunPoll and asserts with no retry loop. That is deliberate: dispatch is
// sequential in the poll goroutine but each worker is its own goroutine, so
// both partitions' workers start in the same PollOnce and RunPoll's quiescence
// wait covers both. A looser AdvanceUntil/WaitFor* would let a second partition
// that only lands on a later poll pass, making the twin vacuous for a
// key-scoping regression. A partition that does not land in one poll is a real
// finding, not something to mask with extra polls.
//
// Which assertion kills which regression (verified by local neutralisation):
//   - groupQueuedByRepoAndBase collapsing everything into one partition: the
//     maint members land on main, so the landing-PR base, distinct-train-PR and
//     branch-content assertions fail.
//   - mergeTrainInFlight keyed by bare repo instead of mergeTrainKey: the second
//     partition's dispatch is skipped ("already assembling"), so its members are
//     still Queued after the single poll and the Done assertion fails.
//
// Observability caveat: mergeTrainInFlight and store.repoWorkers are
// unexported and HasInFlightWorker is a bare boolean, and adding an engine
// accessor is out of scope. The "distinct in-flight markers" requirement is
// therefore proven by proxy: both partitions finish within one poll, with
// distinct trial branches, distinct landing PRs and disjoint member sets.
//
// Fidelity caveat: simgh resolves PR<->issue linkage by head-branch convention,
// not through the base-conditional GraphQL field, so this proves the partition
// mechanics, not the #1046-class GraphQL asymmetry; the live test remains the
// only cover for that.
func TestMergeTrainTwoBasesConcurrent(t *testing.T) {
	t.Parallel()
	const maint = "maint-2007"

	env := mergeTrainEnv(t, mergeTrainEnvOptions{
		// Each train worker holds one e.sem slot for its whole life; a lower
		// cap could serialise the two workers into a quiet pass.
		ConfigureCfg: func(cfg *engine.Config) { cfg.MaxConcurrent = 4 },
	})

	// The throwaway base must exist before any member is queued (commits
	// branch from it) and before the first poll (resolveBaseLabelBranch
	// ls-remotes origin; a missing branch silently collapses the partition).
	env.Sim.Sim().SeedBranch(env.OwnerRepo, maint, "")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedBranch(%s): %v", maint, err)
	}

	mainA, _ := QueueMember(t, env, "twobase-main-a", map[string]string{"main-a.txt": "main a\n"})
	mainB, _ := QueueMember(t, env, "twobase-main-b", map[string]string{"main-b.txt": "main b\n"})
	maintA, _ := QueueMemberOnBase(t, env, maint, "twobase-maint-a", map[string]string{"maint-a.txt": "maint a\n"})
	maintB, _ := QueueMemberOnBase(t, env, maint, "twobase-maint-b", map[string]string{"maint-b.txt": "maint b\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)

	// Single-poll assertions: no WaitFor*/AdvanceUntil (see doc comment).
	all := []int{mainA, mainB, maintA, maintB}
	for _, n := range all {
		item := projectItem(t, env, n)
		if item.Status != "Done" {
			t.Errorf("#%d: Status = %q after a single poll, want Done — both partitions must land independently within the same poll window", n, item.Status)
		}
		if !item.IsClosed {
			t.Errorf("#%d: issue still open after a single poll (closed via auto-close on main, explicit CloseIssue on the non-default base)", n)
		}
	}

	prs, err := env.Sim.ListPRs(env.Owner, env.Repo)
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}

	// Landing PRs: merged PRs whose head is a merge-train trial branch. A clean
	// two-member all-green batch yields exactly one per partition.
	type landing struct {
		head, base string
		closes     []int
	}
	var landings []landing
	for _, pr := range prs {
		if strings.HasPrefix(pr.HeadRefName, mergeTrainBranchPrefix) {
			if pr.State == "open" {
				t.Errorf("stale train artifact: PR #%d (%s -> %s) is still open", pr.Number, pr.HeadRefName, pr.BaseRef)
				continue
			}
			if !pr.Merged {
				continue
			}
			c := parseClosesNumbers(pr.Body)
			sort.Ints(c)
			landings = append(landings, landing{head: pr.HeadRefName, base: pr.BaseRef, closes: c})
		}
	}
	if len(landings) != 2 {
		t.Fatalf("expected exactly 2 merged merge-train landing PRs (one per partition), got %d: %+v", len(landings), landings)
	}
	if landings[0].head == landings[1].head {
		t.Errorf("both landing PRs share head branch %q — partitions did not form distinct trials", landings[0].head)
	}

	wantMembers := map[string][]int{"main": {mainA, mainB}, maint: {maintA, maintB}}
	for _, l := range landings {
		want, ok := wantMembers[l.base]
		if !ok {
			t.Errorf("landing PR %s targets unexpected base %q", l.head, l.base)
			continue
		}
		sort.Ints(want)
		if len(l.closes) != len(want) || l.closes[0] != want[0] || l.closes[1] != want[1] {
			t.Errorf("landing PR %s (base %s) closes %v, want exactly its own partition's members %v", l.head, l.base, l.closes, want)
		}
		delete(wantMembers, l.base)
	}
	for base := range wantMembers {
		t.Errorf("no landing PR targeted base %q", base)
	}

	// Each base moved only by its own members' content.
	dir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	for _, c := range []struct {
		base         string
		has, lacking []string
	}{
		{"main", []string{"main-a.txt", "main-b.txt"}, []string{"maint-a.txt", "maint-b.txt"}},
		{maint, []string{"maint-a.txt", "maint-b.txt"}, []string{"main-a.txt", "main-b.txt"}},
	} {
		for _, f := range c.has {
			if _, err := gitShowFile(t, dir, c.base, f); err != nil {
				t.Errorf("base %s is missing its own member content %s: %v", c.base, f, err)
			}
		}
		for _, f := range c.lacking {
			if _, err := gitShowFile(t, dir, c.base, f); err == nil {
				t.Errorf("base %s contains %s, which belongs to the other partition — cross-base landing", c.base, f)
			}
		}
	}

	// No stale trial branches remain in the backing repo.
	refs := exec.Command("git", "for-each-ref", "--format=%(refname)", "refs/heads/"+mergeTrainBranchPrefix)
	refs.Dir = dir
	if out, err := refs.Output(); err != nil {
		t.Errorf("listing trial branches: %v", err)
	} else if strings.TrimSpace(string(out)) != "" {
		t.Errorf("stale merge-train branches remain after both landings:\n%s", out)
	}
}
