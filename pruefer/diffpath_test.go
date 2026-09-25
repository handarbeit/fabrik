package pruefer

import (
	"os"
	"strings"
	"testing"
)

// TestBuildReviewPrompt_DiffPathReplacesImpossibleGitDiff is the regression
// test for the review hole: CloneForReview fetches only
// refs/pull/<N>/head at depth 1, so the base branch is never a ref in the
// review clone and `git diff <base>...HEAD` cannot resolve. The prompt used
// to instruct exactly that. It failed on every review, and the model fell
// back to reading whole files — on concept-maps#958 (29 files, +3175) it
// reviewed one file and said so: "I could not run `git diff main...HEAD`
// because `main` doesn't resolve in this checkout, so I did not read the
// rest of the diff."
func TestBuildReviewPrompt_DiffPathReplacesImpossibleGitDiff(t *testing.T) {
	got := buildReviewPrompt(ReviewRequest{
		Owner: "verveguy", Repo: "concept-maps", PRNumber: 958,
		BaseBranch: "main", DiffPath: "/tmp/pruefer-pr-958-x.diff",
	})

	if !strings.Contains(got, "/tmp/pruefer-pr-958-x.diff") {
		t.Errorf("prompt does not name the diff file:\n%s", got)
	}
	// The impossible command must not be presented as something to run.
	if strings.Contains(got, "compare against it (e.g. `git diff main...HEAD`)") {
		t.Errorf("prompt still instructs the diff that cannot resolve:\n%s", got)
	}
	// And the model must be told *why*, so it doesn't burn turns discovering it.
	if !strings.Contains(got, "not a ref here") {
		t.Errorf("prompt does not warn that the base ref is absent:\n%s", got)
	}
}

// TestBuildReviewPrompt_NoDiffPathDegrades: if the diff file could not be
// written, the review still runs on the older wording rather than failing.
func TestBuildReviewPrompt_NoDiffPathDegrades(t *testing.T) {
	got := buildReviewPrompt(ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, BaseBranch: "main",
	})
	if !strings.Contains(got, "The PR's base branch is \"main\"") {
		t.Errorf("degraded prompt lost its base-branch context:\n%s", got)
	}
	if strings.Contains(got, ".diff") {
		t.Errorf("degraded prompt names a diff file it does not have:\n%s", got)
	}
}

// TestWriteDiffFile_OutsideCloneAndCleanedUp: the diff must not land inside
// the reviewed tree, where it would appear in git status and in any
// Grep/Glob sweep of the repo.
func TestWriteDiffFile_OutsideCloneAndCleanedUp(t *testing.T) {
	const body = "diff --git a/x b/x\n+one\n"
	path, cleanup, err := writeDiffFile(body, 42)
	if err != nil {
		t.Fatalf("writeDiffFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != body {
		t.Errorf("diff file content = %q, want %q", got, body)
	}
	if !strings.HasSuffix(path, ".diff") {
		t.Errorf("diff file %q lacks a .diff suffix", path)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("diff file still present after cleanup: %v", err)
	}
	cleanup() // must be safe to call twice
}
