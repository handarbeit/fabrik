package sim

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestSpecifyPersistSpec_MultiRoundClarification is the sim twin for #2034
// (ADR 2034): Specify runs read-only with persist_spec, so the engine — not the
// worker — projects each round's FABRIK_ISSUE_UPDATE body to
// specs/<N>-<slug>/spec.md and commits that one file.
//
// Scripted shape (SC-002/SC-003): a first run that ends FABRIK_BLOCKED_ON_INPUT
// with open questions, then two clarification rounds that each change the spec.
// Asserts one `add` commit then one `update` commit per changed round, the slug
// locked at first commit even though the issue title is never re-read from the
// file, `## Open Questions` never in the committed file, no WIP commit from the
// read-only stage, and that every round reached the remote.
//
// Note for fidelity: the sim bed's default stages never set ReadOnly, so this
// scenario configures Specify explicitly with the production posture.
func TestSpecifyPersistSpec_MultiRoundClarification(t *testing.T) {
	t.Parallel() // R8: see poll.go's workerYield doc comment
	env := NewEnv(t, EnvOptions{Stages: []*stages.Stage{
		{Name: "Specify", Order: 1, ReadOnly: true, PersistSpec: true},
		{Name: "Research", Order: 2},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}})

	var mu sync.Mutex
	var workDir string
	capture := func(dir string) {
		mu.Lock()
		workDir = dir
		mu.Unlock()
	}
	body := func(round string) string {
		return "FABRIK_ISSUE_UPDATE_BEGIN\n## Problem\n" + round + "\n\n## Open Questions\n- still open?\nFABRIK_ISSUE_UPDATE_END\n"
	}

	env.Claude.ForStage("Specify", func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, newComments []gh.Comment, resume bool, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
		capture(dir)
		return body("round one") + "FABRIK_BLOCKED_ON_INPUT\nFABRIK_SUMMARY_BEGIN\nstill open?\nFABRIK_SUMMARY_END\n", false, engine.TokenUsage{}, nil
	})
	env.Claude.ForStageComments("Specify",
		func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
			capture(dir)
			return body("round two"), false, engine.TokenUsage{}, nil
		},
		func(ctx context.Context, stage *stages.Stage, issue gh.ProjectItem, comments []gh.Comment, dir string, opts engine.InvokeOptions) (string, bool, engine.TokenUsage, error) {
			capture(dir)
			// Final round: the open question is resolved, the section is gone.
			return "FABRIK_ISSUE_UPDATE_BEGIN\n## Problem\nround three\nFABRIK_ISSUE_UPDATE_END\nFABRIK_STAGE_COMPLETE\n", true, engine.TokenUsage{}, nil
		},
	)

	num := FileIssue(t, env, "Clarify The Thing", "## Problem\nrough idea", "Specify")
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 80)

	dir := func() string {
		mu.Lock()
		defer mu.Unlock()
		return workDir
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	branch := "fabrik/issue-" + strconv.Itoa(num)
	specRel := "specs/" + strconv.Itoa(num) + "-clarify-the-thing/spec.md"
	spec := func() string {
		data, err := os.ReadFile(filepath.Join(dir(), filepath.FromSlash(specRel)))
		if err != nil {
			t.Fatalf("spec not persisted: %v", err)
		}
		return string(data)
	}
	specSubjects := func() []string {
		out := git("log", "--reverse", "--format=%s", "--", specRel)
		if out == "" {
			return nil
		}
		return strings.Split(out, "\n")
	}
	assertRemoteHasTip := func(stage string) {
		t.Helper()
		if remote := git("ls-remote", "origin", branch); !strings.HasPrefix(remote, git("rev-parse", "HEAD")) {
			t.Errorf("%s: remote branch tip %q != local HEAD — spec commit was not pushed", stage, remote)
		}
	}

	// Round 1 (blocked): spec committed already, open questions stripped.
	if got := spec(); !strings.Contains(got, "round one") || strings.Contains(got, "Open Questions") {
		t.Errorf("round 1 spec = %q", got)
	}
	if subj := specSubjects(); len(subj) != 1 || !strings.HasPrefix(subj[0], "docs(spec): add "+specRel) {
		t.Errorf("round 1 commits = %q, want a single add", subj)
	}
	assertRemoteHasTip("round 1")

	// Round 2.
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "answer one")
	AdvanceUntil(t, env, func(*Env) bool {
		return env.Claude.CommentCallCount("Specify") >= 1 && strings.Contains(spec(), "round two")
	}, 80)
	assertRemoteHasTip("round 2")

	// Round 3 (completes the stage).
	env.Sim.Sim().SeedComment(env.OwnerRepo, num, "a-human-operator", "answer two")
	WaitForIssueLabel(t, env, num, "stage:Specify:complete", 80)

	if got := spec(); !strings.Contains(got, "round three") {
		t.Errorf("final spec = %q", got)
	}
	subj := specSubjects()
	if len(subj) != 3 {
		t.Fatalf("spec commits = %q, want add + 2 updates", subj)
	}
	if !strings.HasPrefix(subj[0], "docs(spec): add ") || !strings.HasPrefix(subj[1], "docs(spec): update ") || !strings.HasPrefix(subj[2], "docs(spec): update ") {
		t.Errorf("spec commits = %q", subj)
	}
	if all := git("log", "--format=%s"); strings.Contains(all, "partial") {
		t.Errorf("read-only Specify must not produce a WIP commit; log:\n%s", all)
	}
	// The directory was never renamed or duplicated.
	if entries, _ := filepath.Glob(filepath.Join(dir(), "specs", strconv.Itoa(num)+"-*")); len(entries) != 1 {
		t.Errorf("spec directories = %v, want exactly one", entries)
	}
	assertRemoteHasTip("final")
}
