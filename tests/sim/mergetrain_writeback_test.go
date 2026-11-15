package sim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// Sim twin of the multi-member trial conflict write-back (#2572, ADR 2572).
//
// A two-member trial where M2 conflicts with a base that moved after M2's branch was cut:
// the conflict is member-vs-base, so rerere can replay the trial's resolution against M2's
// own branch. After the combined trial is abandoned (M1 is poisoned, so the batch bisects),
// M2's own branch must hold the resolution and M2's next trial must assemble with no
// conflict and no Claude invocation. The shared bare clone's rr-cache is cleared the moment
// the first trial's verdict is seeded: without that rerere would replay the resolution in
// the second trial and hide whether the member branch carries it.
//
// Neutralisation: every "writes back" scenario fails with SetTrainWriteBackDisabledForTest
// (TestMergeTrainWriteBack_DisabledSeamInvokesClaudeAgain demonstrates it), and the
// Validate-completion scenario fails with SetWriteBackSHAExemptionDisabledForTest.

var writeBackMarkerHeadRe = regexp.MustCompile(`kind=writeback`)

func writeBackEnvOptions(async bool, mutate func(*engine.Config)) mergeTrainEnvOptions {
	return mergeTrainEnvOptions{AsyncTrain: async, ConfigureCfg: func(c *engine.Config) {
		c.User = "simgh-bot"
		if mutate != nil {
			mutate(c)
		}
	}}
}

type writeBackScenario struct {
	env         *Env
	m1, m2      int
	m2PR        int
	m2Orig      string
	baseAfter   string
	trialsSeen  int
	rrCacheGone bool
}

// setupWriteBackScenario queues M1 (a.txt) and M2 (shared.txt), then advances main so
// shared.txt conflicts with M2, and arms a verdict seeder that reds any trial containing
// M1 and clears the rr-cache before seeding the first trial's verdict.
func setupWriteBackScenario(t *testing.T, env *Env) *writeBackScenario {
	t.Helper()
	s := &writeBackScenario{env: env}
	s.m1, _ = QueueMember(t, env, "wb-m1", map[string]string{"a.txt": "a\n"})
	s.m2, s.m2PR = QueueMember(t, env, "wb-m2", map[string]string{"shared.txt": "member\n"})
	s.m2Orig, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(s.m2))
	advanceMain(t, env, map[string]string{"shared.txt": "main\n"})
	s.baseAfter, _ = env.Sim.Sim().HeadSHA(env.OwnerRepo, "main")
	env.Claude.ForStageComments("Queued", claudeResolveConflict("shared.txt", "resolved\n"))

	var mu sync.Mutex
	poison := poisonVerdict(s.m1)
	startTrialVerdictSeeder(t, env, func(members []int) []gh.CheckRun {
		mu.Lock()
		defer mu.Unlock()
		s.trialsSeen++
		if !s.rrCacheGone {
			s.rrCacheGone = true
			os.RemoveAll(filepath.Join(env.WM.BaseDir(), "rr-cache"))
		}
		return poison(members)
	})
	return s
}

// writeBackHead returns the head named by M2's write-back marker, or "".
func writeBackHead(t *testing.T, s *writeBackScenario) string {
	t.Helper()
	for _, c := range commentsOn(t, s.env, s.m2PR) {
		if !strings.Contains(c.Body, "fabrik:train-catch-up") || !writeBackMarkerHeadRe.MatchString(c.Body) {
			continue
		}
		if m := regexp.MustCompile(`head=([0-9a-f]+)`).FindStringSubmatch(c.Body); m != nil {
			return m[1]
		}
	}
	return ""
}

func gitInSimRepo(t *testing.T, env *Env, args ...string) string {
	t.Helper()
	dir, err := env.Sim.Sim().RepoBareDir(env.OwnerRepo)
	if err != nil {
		t.Fatalf("RepoBareDir: %v", err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func testWriteBackLandsResolutionOnMemberBranch(t *testing.T, async bool) {
	t.Parallel()
	env := mergeTrainEnv(t, writeBackEnvOptions(async, nil))
	s := setupWriteBackScenario(t, env)

	WaitForProjectStatus(t, env, s.m2, "Done", 40)

	head := writeBackHead(t, s)
	if head == "" {
		t.Fatalf("no kind=writeback marker on M2's PR #%d:\n%v", s.m2PR, commentsOn(t, env, s.m2PR))
	}
	if head == s.m2Orig {
		t.Fatal("the marker names M2's original head")
	}
	// The member's own branch received the resolved base merge: a two-parent merge of
	// the original head and the pinned base, with the resolved content.
	parents := strings.Fields(gitInSimRepo(t, env, "rev-list", "--parents", "-n", "1", head))
	if len(parents) != 3 || parents[1] != s.m2Orig {
		t.Errorf("write-back commit parents = %v, want a merge on M2's original head %s", parents, s.m2Orig)
	}
	if got := gitInSimRepo(t, env, "show", head+":shared.txt"); got != "resolved" {
		t.Errorf("shared.txt on the written-back head = %q, want the resolution", got)
	}
	if out := gitInSimRepo(t, env, "merge-base", "--is-ancestor", s.baseAfter, head); out != "" {
		t.Errorf("the written-back head does not contain the moved base: %s", out)
	}

	// The one Claude call is the combined trial's own: the member's next trial (the
	// bisection half) assembles with the resolution already on its branch, no conflict.
	if got := env.Claude.CommentCallCount("Queued"); got != 1 {
		t.Errorf("Claude conflict resolutions = %d, want exactly 1 (the combined trial's)", got)
	}
	if !hasCommentContaining(t, env, s.m2PR, "conflict resolution written back") {
		t.Error("the write-back explanation comment is missing")
	}
}

func TestMergeTrainWriteBack_ResolutionOutlivesTheAbandonedTrial_Sync(t *testing.T) {
	testWriteBackLandsResolutionOnMemberBranch(t, false)
}

func TestMergeTrainWriteBack_ResolutionOutlivesTheAbandonedTrial_Async(t *testing.T) {
	testWriteBackLandsResolutionOnMemberBranch(t, true)
}

// Non-vacuity: with the write-back neutralised the member's branch carries nothing, so
// the next trial conflicts again and Claude is invoked a second time. singleton_catch_up
// is off so M2's solo trial is deterministic (otherwise it would be caught up in place).
func TestMergeTrainWriteBack_DisabledSeamInvokesClaudeAgain(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, writeBackEnvOptions(false, func(c *engine.Config) { c.SingletonCatchUp = "off" }))
	env.Engine.SetTrainWriteBackDisabledForTest(true)
	s := setupWriteBackScenario(t, env)

	WaitForProjectStatus(t, env, s.m2, "Done", 40)

	if head := writeBackHead(t, s); head != "" {
		t.Errorf("a write-back marker was posted with the seam set: %s", head)
	}
	if got := env.Claude.CommentCallCount("Queued"); got < 2 {
		t.Errorf("Claude conflict resolutions = %d, want >= 2 (the conflict returns without the write-back)", got)
	}
	if head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(s.m2)); head != "" && head != s.m2Orig {
		t.Errorf("M2's own branch moved to %s with the write-back disabled", head)
	}
}

// An ownership failure (fabrik:editing appears on the member while the trial's conflict is
// being resolved) skips the write-back: M2's branch is untouched, no marker is posted and
// nothing is charged (no pause).
func TestMergeTrainWriteBack_OwnershipFailureLeavesTodaysBehaviour(t *testing.T) {
	t.Parallel()
	env := mergeTrainEnv(t, writeBackEnvOptions(false, func(c *engine.Config) { c.SingletonCatchUp = "off" }))
	s := setupWriteBackScenario(t, env)
	resolve := claudeResolveConflict("shared.txt", "resolved\n")
	env.Claude.ForStageComments("Queued", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, workDir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		if err := env.Sim.AddLabelToIssue(env.Owner, env.Repo, s.m2, "fabrik:editing"); err != nil {
			t.Errorf("AddLabelToIssue: %v", err)
		}
		return resolve(ctx, stage, issue, comments, workDir, opts)
	})

	RunPolls(t, env, 3)

	if s.trialsSeen == 0 {
		t.Fatal("no trial was assembled, so the scenario proved nothing")
	}
	if head := writeBackHead(t, s); head != "" {
		t.Errorf("a write-back ran for a member carrying fabrik:editing: %s", head)
	}
	if head, _ := env.Sim.Sim().HeadSHA(env.OwnerRepo, memberBranch(s.m2)); head != s.m2Orig {
		t.Errorf("M2's own branch moved to %s despite the failed ownership check", head)
	}
	for _, l := range IssueLabels(t, env, s.m2) {
		if l == "fabrik:paused" {
			t.Error("a skipped write-back must not pause the member")
		}
	}
}
