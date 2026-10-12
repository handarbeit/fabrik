package pruefer

import (
	"os"
	"strings"
	"testing"
)

// stagedForPrompt stages a one-entry diff for prompt tests.
func stagedForPrompt(t *testing.T) *StagedDiff {
	t.Helper()
	s, cleanup, err := stageDiff([]diffFileBlock{{Path: "main.go", Raw: "diff --git a/main.go b/main.go\n+++ b/main.go\n@@ -1 +1 @@\n+x\n"}}, "", 958)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return s
}

// TestBuildReviewPrompt_StagedDiffReplacesImpossibleGitDiff is the regression
// test for the review hole: CloneForReview fetches only
// refs/pull/<N>/head at depth 1, so the base branch is never a ref in the
// review clone and `git diff <base>...HEAD` cannot resolve. The prompt used
// to instruct exactly that. It failed on every review, and the model fell
// back to reading whole files — on concept-maps#958 (29 files, +3175) it
// reviewed one file and said so: "I could not run `git diff main...HEAD`
// because `main` doesn't resolve in this checkout, so I did not read the
// rest of the diff."
func TestBuildReviewPrompt_StagedDiffReplacesImpossibleGitDiff(t *testing.T) {
	staged := stagedForPrompt(t)
	got := buildReviewPrompt(ReviewRequest{
		Owner: "verveguy", Repo: "concept-maps", PRNumber: 958,
		BaseBranch: "main", Staged: staged,
	})

	if !strings.Contains(got, staged.Dir) || !strings.Contains(got, staged.IndexPath) {
		t.Errorf("prompt does not name the staging dir and index:\n%s", got)
	}
	// The impossible command must not be presented as something to run.
	if strings.Contains(got, "compare against it (e.g. `git diff main...HEAD`)") {
		t.Errorf("prompt still instructs the diff that cannot resolve:\n%s", got)
	}
	// And the model must be told *why*, so it doesn't burn turns discovering it.
	if !strings.Contains(got, "no git history or base ref") {
		t.Errorf("prompt does not warn that the base ref is absent:\n%s", got)
	}
}

// TestBuildReviewPrompt_StagedRequiresOpeningEveryEntry pins #2190 R1/R2 in
// the Go-owned halves: open every entry (including under subagents) and report
// coverage in the marker block.
func TestBuildReviewPrompt_StagedRequiresOpeningEveryEntry(t *testing.T) {
	got := buildReviewPrompt(ReviewRequest{Owner: "o", Repo: "r", PRNumber: 1, Staged: stagedForPrompt(t)})
	for _, want := range []string{
		"Open every entry in full",
		"every index entry must be opened by some reader",
		"PRUEFER_COVERAGE_BEGIN",
		"PRUEFER_COVERAGE_END",
		"Output has three parts",
		"report back the IDs it opened in full",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// TestBuildReviewPrompt_NoStagedDegrades: if nothing could be staged, the
// review still runs on the older wording rather than failing, and the prompt
// asks for no coverage block it has no entries for.
func TestBuildReviewPrompt_NoStagedDegrades(t *testing.T) {
	got := buildReviewPrompt(ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, BaseBranch: "main",
	})
	if !strings.Contains(got, "The PR targets \"main\"") {
		t.Errorf("degraded prompt lost its base-branch context:\n%s", got)
	}
	if strings.Contains(got, ".diff") || strings.Contains(got, "PRUEFER_COVERAGE_BEGIN") {
		t.Errorf("degraded prompt names staged entries or a coverage block it does not have:\n%s", got)
	}
	if !strings.Contains(got, "Output has two parts") {
		t.Errorf("degraded prompt should keep the two-part contract:\n%s", got)
	}
}

// TestStageDiff_OutsideCloneAndCleanedUp: the staged diff must not land inside
// the reviewed tree, where it would appear in git status and in any Grep/Glob
// sweep of the repo, and cleanup removes it (twice is fine).
func TestStageDiff_OutsideCloneAndCleanedUp(t *testing.T) {
	const raw = "diff --git a/x b/x\n+++ b/x\n@@ -0,0 +1 @@\n+one\n"
	s, cleanup, err := stageDiff([]diffFileBlock{{Path: "x", Raw: raw}}, "", 42)
	if err != nil {
		t.Fatalf("stageDiff: %v", err)
	}
	got, err := os.ReadFile(s.EntryPath(s.Entries[0]))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(got) != raw {
		t.Errorf("entry content = %q, want %q", got, raw)
	}
	if _, err := os.Stat(s.IndexPath); err != nil {
		t.Errorf("index missing: %v", err)
	}

	cleanup()
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Errorf("staging dir still present after cleanup: %v", err)
	}
	cleanup() // must be safe to call twice
}

// TestReviewPR_EmptyDiffStagesNothing is the regression test for the defect
// Pruefer found reviewing the original diff-file change: when FetchPRDiff
// returns 406 too_large, review.go's files-API fallback leaves diff == "".
// Staging that would give the prompt an empty index it calls "the
// authoritative statement of what changed" while telling the reviewer not to
// fall back to git — strictly worse than nothing, and it hits exactly the
// largest PRs.
func TestReviewPR_EmptyDiffStagesNothing(t *testing.T) {
	got := buildReviewPrompt(ReviewRequest{
		Owner: "o", Repo: "r", PRNumber: 1, BaseBranch: "main",
	})
	if strings.Contains(got, "authoritative statement of what changed") {
		t.Errorf("prompt claims an authoritative diff with nothing staged:\n%s", got)
	}
	if !strings.Contains(got, "no staged diff is available") {
		t.Errorf("prompt did not degrade to the base-branch wording:\n%s", got)
	}
}

func TestStageDiff_RejectsNothingToStage(t *testing.T) {
	if _, _, err := stageDiff(nil, "", 1); err == nil {
		t.Error("expected an error staging an empty diff")
	}
}
