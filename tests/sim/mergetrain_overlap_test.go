package sim

import (
	"fmt"
	"strings"
	"testing"

	"github.com/handarbeit/fabrik/engine"
)

// This file is the sim twin of #2047 (ADR-2047): overlap-aware fresh batch composition and
// the post-landing invalidation of Queued members that genuinely conflict with the new base.
// There is no live e2e twin — the behaviour needs no real GitHub, and the sim derives each
// PR's changed files from real git (FetchPRFiles), so "overlap" and "conflict" here are
// whatever the commits actually produce, never declared.
//
// The starvation guard (3 consecutive deferrals → admitted first) has no sim scenario: the
// sim orders Queued members by issue number, and a deferred member only gets repeatedly
// deferred behind a stream of *earlier-ordered* overlapping arrivals, which the sim's
// board cannot express. engine/merge_train_overlap_test.go drives it directly.

// overlapSharedLines is a 30-line file; a member editing one line merges cleanly with a
// member editing a different line, yet both touch the same path.
func overlapSharedLines(replace map[int]string) string {
	var b strings.Builder
	for i := 1; i <= 30; i++ {
		if v, ok := replace[i]; ok {
			fmt.Fprintf(&b, "%s\n", v)
		} else {
			fmt.Fprintf(&b, "line %d\n", i)
		}
	}
	return b.String()
}

func seedOverlapBase(t *testing.T, env *Env, file string) {
	t.Helper()
	env.Sim.Sim().SeedCommit(env.OwnerRepo, "main", map[string]string{file: overlapSharedLines(nil)}, "overlap fixture base")
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seeding overlap base: %v", err)
	}
}

func overlapEnv(t *testing.T, cfgure func(*engine.Config)) *Env {
	t.Helper()
	return mergeTrainEnv(t, mergeTrainEnvOptions{ValidateWaitForCI: true, OverlapAware: true, ConfigureCfg: cfgure})
}

// A and B edit the same path (different hunks, so they merge cleanly); C is disjoint. The
// first batch is A+C; B stays Queued — NOT rerouted, because it merges cleanly with the new
// base (FR-008) — and lands alone in the next batch.
func TestMergeTrainOverlap_OverlappingMembersAreBatchedApart(t *testing.T) {
	t.Parallel()
	env := overlapEnv(t, nil)
	seedOverlapBase(t, env, "shared.txt")

	a, _ := QueueMember(t, env, "ov-a", map[string]string{"shared.txt": overlapSharedLines(map[int]string{2: "A's edit"})})
	b, _ := QueueMember(t, env, "ov-b", map[string]string{"shared.txt": overlapSharedLines(map[int]string{28: "B's edit"})})
	c, _ := QueueMember(t, env, "ov-c", map[string]string{"c.txt": "c\n"})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	landings := batchCapLandings(t, env)
	if len(landings) != 1 || !equalInts(landings[0].closes, []int{a, c}) {
		t.Fatalf("after poll 1: landings %+v, want exactly one landing of {%d,%d} (B=%d overlaps A and must wait)", landings, a, c, b)
	}
	if got := projectItem(t, env, b); got.Status != "Queued" {
		t.Fatalf("deferred overlapping member B is %q, want it left in Queued", got.Status)
	}

	if trialPRsContain(t, env, b) {
		t.Errorf("B=%d was assembled into a trial alongside the overlapping A", b)
	}
	if hasCommentContaining(t, env, b, "rerouted (conflicts with new base)") || hasLabel(IssueLabels(t, env, b), "fabrik:paused") {
		t.Errorf("B merges cleanly with the new base and must be neither rerouted nor paused (FR-008)")
	}
}

// The neutralisation twin: with the overlap filter off the same fixture forms one batch.
func TestMergeTrainOverlap_FilterDisabledBatchesTogether(t *testing.T) {
	t.Parallel()
	env := overlapEnv(t, nil)
	env.Engine.SetMergeTrainOverlapDisabledForTest(true)
	seedOverlapBase(t, env, "shared.txt")

	a, _ := QueueMember(t, env, "ovn-a", map[string]string{"shared.txt": overlapSharedLines(map[int]string{2: "A's edit"})})
	b, _ := QueueMember(t, env, "ovn-b", map[string]string{"shared.txt": overlapSharedLines(map[int]string{28: "B's edit"})})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	landings := batchCapLandings(t, env)
	if len(landings) != 1 || !equalInts(landings[0].closes, []int{a, b}) {
		t.Fatalf("landings %+v, want one batch of {%d,%d} with the filter neutralised", landings, a, b)
	}
}

// merge_train_overlap_ignore: a shared lockfile is not overlap.
func TestMergeTrainOverlap_IgnoreGlobLetsLockfileMembersBatch(t *testing.T) {
	t.Parallel()
	env := overlapEnv(t, func(cfg *engine.Config) { cfg.MergeTrainOverlapIgnore = []string{"**/*.lock"} })
	seedOverlapBase(t, env, "web/yarn.lock")

	a, _ := QueueMember(t, env, "ovi-a", map[string]string{"web/yarn.lock": overlapSharedLines(map[int]string{2: "A's edit"})})
	b, _ := QueueMember(t, env, "ovi-b", map[string]string{"web/yarn.lock": overlapSharedLines(map[int]string{28: "B's edit"})})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env)
	landings := batchCapLandings(t, env)
	if len(landings) != 1 || !equalInts(landings[0].closes, []int{a, b}) {
		t.Fatalf("landings %+v, want one batch of {%d,%d} — the only shared path is ignored", landings, a, b)
	}
}

// After A lands, B (same line as A — a genuine conflict) is rerouted off Queued before a
// trial is spent on it, while D (same file, different hunk — clean) is left in Queued and
// lands. The reroute is not an ejection and not a pause.
func TestMergeTrainOverlap_PostLandingInvalidationReroutesOnlyRealConflicts(t *testing.T) {
	t.Parallel()
	env := overlapEnv(t, nil)
	seedOverlapBase(t, env, "shared.txt")

	a, _ := QueueMember(t, env, "inv-a", map[string]string{"shared.txt": overlapSharedLines(map[int]string{2: "A's edit"})})
	b, _ := QueueMember(t, env, "inv-b", map[string]string{"shared.txt": overlapSharedLines(map[int]string{2: "B's conflicting edit"})})
	d, _ := QueueMember(t, env, "inv-d", map[string]string{"shared.txt": overlapSharedLines(map[int]string{28: "D's edit"})})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	// Poll 1: A, B and D all overlap, so the batch is A alone and it lands.
	RunPoll(t, env)
	landings := batchCapLandings(t, env)
	if len(landings) != 1 || !equalInts(landings[0].closes, []int{a}) {
		t.Fatalf("after poll 1: landings %+v, want A=%d alone", landings, a)
	}

	// Poll 2: the scan runs (no worker in flight) before any batch forms.
	RunPoll(t, env)
	gotB := projectItem(t, env, b)
	if gotB.Status != "Validate" {
		t.Fatalf("B=%d is %q after the post-landing scan, want it rerouted to Validate", b, gotB.Status)
	}
	if !hasLabel(gotB.Labels, "fabrik:rebase-needed") {
		t.Errorf("B labels %v, want fabrik:rebase-needed", gotB.Labels)
	}
	if hasLabel(gotB.Labels, "fabrik:paused") || hasLabel(gotB.Labels, "fabrik:awaiting-input") {
		t.Errorf("B must not be paused by the reroute: %v", gotB.Labels)
	}
	if !hasCommentContaining(t, env, b, "rerouted (conflicts with new base)") {
		t.Error("expected the reroute comment on B")
	}
	if hasCommentContaining(t, env, b, "merge-train — ejected") {
		t.Error("the reroute must not reuse ejection wording")
	}
	if trialPRsContain(t, env, b) {
		t.Error("B must never have been assembled into a trial")
	}

	// D overlapped A's file but merges cleanly: not rerouted, and it lands.
	WaitForProjectStatus(t, env, d, "Done", 20)
	if hasCommentContaining(t, env, d, "rerouted (conflicts with new base)") {
		t.Error("D merges cleanly and must not be rerouted")
	}
}

// The neutralisation twin: with invalidation off, B stays Queued after the landing (the
// train would only learn of the conflict by spending a trial on it).
func TestMergeTrainOverlap_InvalidationDisabledLeavesConflictingMemberQueued(t *testing.T) {
	t.Parallel()
	env := overlapEnv(t, nil)
	env.Engine.SetMergeTrainInvalidationDisabledForTest(true)
	seedOverlapBase(t, env, "shared.txt")

	QueueMember(t, env, "invn-a", map[string]string{"shared.txt": overlapSharedLines(map[int]string{2: "A's edit"})})
	b, _ := QueueMember(t, env, "invn-b", map[string]string{"shared.txt": overlapSharedLines(map[int]string{2: "B's conflicting edit"})})
	startTrialVerdictSeeder(t, env, allGreenVerdict)

	RunPoll(t, env) // A lands
	// The scan is neutralised, so B is never rerouted by it. B's next trial would be the
	// only thing to notice the conflict; assert only that the scan did not move it.
	if hasCommentContaining(t, env, b, "rerouted (conflicts with new base)") {
		t.Error("the post-landing reroute fired with the scan neutralised")
	}
}
