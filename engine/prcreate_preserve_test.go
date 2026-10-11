package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// runMalformedPRCreateImplement drives an Implement stage whose worker commits
// one file, leaves another uncommitted, and emits an unparseable
// FABRIK_PR_CREATE block (#2091 R4). breakRemote makes the push fail.
func runMalformedPRCreateImplement(t *testing.T, issue int, breakRemote bool) (client *mockGitHubClient, repoDir string) {
	t.Helper()
	skipIfNoGit(t)
	origLock := lockVerifyDelay
	lockVerifyDelay = 0
	t.Cleanup(func() { lockVerifyDelay = origLock })

	client = &mockGitHubClient{}
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("git %v: %s: %v", args, out, err)
		}
	}
	claude := &mockClaudeInvoker{
		invokeFn: func(stage *stages.Stage, item gh.ProjectItem, comments []gh.Comment, resume bool, workDir string, opts InvokeOptions) (string, bool, TokenUsage, error) {
			if err := os.WriteFile(filepath.Join(workDir, "committed.txt"), []byte("c"), 0o644); err != nil {
				t.Errorf("write: %v", err)
			}
			run(workDir, "add", "committed.txt")
			run(workDir, "commit", "-m", "feat: the stage's work")
			if err := os.WriteFile(filepath.Join(workDir, "leftover.txt"), []byte("l"), 0o644); err != nil {
				t.Errorf("write: %v", err)
			}
			if breakRemote {
				run(workDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "does-not-exist.git"))
			}
			out := "All done.\nFABRIK_PR_CREATE_BEGIN\nSummary of the change\n\nbody\nFABRIK_PR_CREATE_END\nFABRIK_STAGE_COMPLETE\n"
			return out, true, TokenUsage{TurnsUsed: 3, MaxTurns: 30}, nil
		},
	}
	var eng *Engine
	eng, repoDir = testEngineWithRepoAndStages(t, client, claude, coordinatorStages())
	board := &gh.ProjectBoard{ProjectID: "PVT_1"}
	item := gh.ProjectItem{Number: issue, Title: "Impl", Status: "Implement", ItemID: "PVTI_x"}
	if err := eng.processItem(t.Context(), board, item); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	return client, repoDir
}

func assertMalformedPRCreatePaused(t *testing.T, client *mockGitHubClient) {
	t.Helper()
	var paused, complete bool
	for _, c := range client.addLabelCalls {
		switch c.labelName {
		case "fabrik:paused":
			paused = true
		case "stage:Implement:complete":
			complete = true
		}
	}
	if !paused || complete {
		t.Errorf("paused=%v complete=%v, want paused and not complete", paused, complete)
	}
	var found bool
	for _, c := range client.addCommentCalls {
		if strings.Contains(c.body, "malformed FABRIK_PR_CREATE marker") && strings.Contains(c.body, "`Summary of the change`") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected malformed-marker comment quoting the found line, got %v", client.addCommentCalls)
	}
	if len(client.createDraftPRCalls) != 0 {
		t.Errorf("no PR may be created, got %v", client.createDraftPRCalls)
	}
}

// TestFinalizeStageOutcome_MalformedPRCreate_CommitsAndPushesWork is the #2091
// R4 guard: a FABRIK_PR_CREATE parse failure no longer strands the stage's work
// locally — it is committed (including leftovers) and pushed before the pause.
func TestFinalizeStageOutcome_MalformedPRCreate_CommitsAndPushesWork(t *testing.T) {
	client, repoDir := runMalformedPRCreateImplement(t, 91, false)
	assertMalformedPRCreatePaused(t, client)

	out, err := exec.Command("git", "-C", repoDir, "ls-remote", "origin", "refs/heads/fabrik/issue-91").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("branch not on remote: %s %v", out, err)
	}
	sha := strings.Fields(string(out))[0]
	// The pushed tip must contain both the worker's commit and the leftover file.
	if err := exec.Command("git", "-C", repoDir, "fetch", "origin", "refs/heads/fabrik/issue-91:refs/tmp/check").Run(); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	for _, f := range []string{"committed.txt", "leftover.txt"} {
		if out, err := exec.Command("git", "-C", repoDir, "cat-file", "-e", sha+":"+f).CombinedOutput(); err != nil {
			t.Errorf("%s missing from pushed tip %s: %s", f, sha, out)
		}
	}
}

// TestFinalizeStageOutcome_MalformedPRCreate_PushFailureStillPauses covers
// FR-010: a failing push must not suppress the parse-error comment or pause.
func TestFinalizeStageOutcome_MalformedPRCreate_PushFailureStillPauses(t *testing.T) {
	client, _ := runMalformedPRCreateImplement(t, 92, true)
	assertMalformedPRCreatePaused(t, client)
}
