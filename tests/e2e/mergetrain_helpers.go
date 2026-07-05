//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Merge-train e2e helpers (ADR-059). These build member PRs directly via the
// GitHub API and place issues straight into the Queued column, so a train
// scenario does not have to run the full 45-minute Specify→Validate pipeline for
// every member. The train is column-driven (ADR-059 D1: "the train's input set is
// every item in Queued"), so an externally-placed Queued item with a linked PR is
// a valid member — which is exactly what these helpers construct.

// requireTrainBed skips the test unless the test board has a "Queued" column —
// the one-time operator setup the merge train needs (ADR-059 D1). Keeps train
// scenarios from failing on a bed that predates the train rollout.
func requireTrainBed(t *testing.T, env *Env) {
	t.Helper()
	// Retry on transient errors (e.g. GraphQL rate-limit exhaustion — gh project runs
	// on GraphQL). A persistent read failure FAILS the test rather than silently
	// skipping, so a rate-limited run does not masquerade as "bed not set up".
	var out string
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		out, err = ghOutput(env, "project", "field-list", fmt.Sprint(env.ProjectNumber),
			"--owner", env.ProjectOwner, "--format", "json",
			"--jq", `.fields[] | select(.name=="Status") | .options[]?.name`)
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				if strings.TrimSpace(line) == "Queued" {
					return
				}
			}
			t.Skipf("test board %s/#%d has no Queued column — merge-train bed not set up (see tests/e2e/README.md)",
				env.ProjectOwner, env.ProjectNumber)
		}
		t.Logf("requireTrainBed: transient board-read error (attempt %d/6): %v\n%s", attempt+1, err, out)
		time.Sleep(20 * time.Second)
	}
	t.Fatalf("could not read board columns after 6 attempts (last: %v) — GraphQL rate limit or API issue, not a skip condition", err)
}

// defaultBranchSHA returns the head commit SHA of the repo's default branch.
func defaultBranchSHA(t *testing.T, env *Env, repo, baseBranch string) string {
	t.Helper()
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, baseBranch),
		"--jq", ".object.sha")
	if err != nil {
		t.Fatalf("resolve %s default branch (%s) sha: %v\n%s", repo, baseBranch, err, out)
	}
	sha := lastNonEmpty(out)
	if sha == "" {
		t.Fatalf("empty sha for %s/%s", repo, baseBranch)
	}
	return sha
}

// CreateMemberPR builds a real member PR on repo: it branches off baseBranch,
// writes content to path on that branch, and opens a PR whose body contains
// "Closes #issueNum" (so Fabrik discovers the issue↔PR linkage). Returns the PR
// number. Registers cleanup to delete the branch at test end.
//
// path/content let the caller shape the batch: distinct paths → a clean batch;
// the same path with divergent content → a textual conflict for the bisection /
// conflict-resolution scenarios.
func CreateMemberPR(t *testing.T, env *Env, repo, baseBranch, branch, path, content, issueTitle string, issueNum int) int {
	t.Helper()
	baseSHA := defaultBranchSHA(t, env, repo, baseBranch)

	// Create the branch ref off the base head.
	if out, err := ghOutput(env, "api", "--method", "POST",
		fmt.Sprintf("repos/%s/git/refs", repo),
		"-f", "ref=refs/heads/"+branch,
		"-f", "sha="+baseSHA); err != nil {
		t.Fatalf("create branch %s on %s: %v\n%s", branch, repo, err, out)
	}
	t.Cleanup(func() {
		_, _ = ghOutput(env, "api", "--method", "DELETE",
			fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, branch))
	})

	// Write the file on the new branch (single commit).
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	if out, err := ghOutput(env, "api", "--method", "PUT",
		fmt.Sprintf("repos/%s/contents/%s", repo, path),
		"-f", fmt.Sprintf("message=e2e merge-train member for #%d", issueNum),
		"-f", "content="+enc,
		"-f", "branch="+branch); err != nil {
		t.Fatalf("write %s on %s@%s: %v\n%s", path, repo, branch, err, out)
	}

	// Open the PR with the Closes #N linkage.
	body := fmt.Sprintf("e2e merge-train member.\n\nCloses #%d\n", issueNum)
	out, err := ghOutput(env, "pr", "create", "-R", repo,
		"--base", baseBranch, "--head", branch,
		"--title", issueTitle, "--body", body)
	if err != nil {
		t.Fatalf("create member PR for #%d on %s: %v\n%s", issueNum, repo, err, out)
	}
	prNum := parseIssueNumberFromURL(lastNonEmpty(out))
	if prNum == 0 {
		t.Fatalf("could not parse member PR number from %q", out)
	}
	t.Logf("created member PR #%d (issue #%d, branch %s, path %s)", prNum, issueNum, branch, path)
	return prNum
}

// QueueMember files an issue, adds it to the project, creates its member PR, and
// places the issue directly in the Queued column. Returns (issueNum, prNum). The
// caller controls path/content to make the batch clean or conflicting.
func QueueMember(t *testing.T, env *Env, repo, baseBranch, marker, path, content string) (int, int) {
	t.Helper()
	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e merge-train member %s (%s)", marker, stamp)
	num := FileIssue(t, env, repo, title,
		fmt.Sprintf("e2e merge-train member. marker=%s", marker))
	itemID := AddIssueToProject(t, env, repo, num)
	// The engine resolves a member's linked PR strictly by the fabrik/issue-<N>
	// branch convention (github.Client.FetchLinkedPR queries pulls?head=fabrik/issue-N),
	// NOT by the "Closes #N" body — so the member PR MUST live on that branch or the
	// train cannot find it and ejects the member. (The Closes #N body still drives
	// GitHub's issue auto-close on merge; both are set.)
	// Make the file path unique per run. A landed batch MERGES its member files into
	// main, so a fixed path collides with the existing file on the next run (GitHub's
	// contents API requires the existing blob sha to update, which we don't supply).
	// The fresh issue number guarantees uniqueness; the directory is preserved so the
	// bisection poison-guard (which scans e2e/train/entries/) still sees the file.
	uPath := uniqueMemberPath(path, num)
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	prNum := CreateMemberPR(t, env, repo, baseBranch, branch, uPath, content, title, num)
	// Confirm the PR is resolvable by that branch (mirrors the engine's resolver)
	// BEFORE placing the item in Queued, so the train's first poll can fetch it.
	LinkedPRNumber(t, env, repo, num)
	// Placing directly in Queued: the train is column-driven, so this is a valid
	// member without running the full pipeline.
	SetIssueStatus(t, env, itemID, "Queued")
	t.Logf("queued member: issue #%d, PR #%d, at Status=Queued", num, prNum)
	return num, prNum
}

// WaitForIntegrationPR polls the repo for the merge-train integration PR (head
// branch carries the "merge-train-" prefix), up to timeout. Returns the number of
// the most recently created one.
func WaitForIntegrationPR(t *testing.T, env *Env, repo string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := ghOutput(env, "pr", "list", "-R", repo, "--state", "all",
			"--json", "number,headRefName,createdAt",
			"--jq", `[.[] | select(.headRefName | startswith("fabrik/merge-train/"))] | sort_by(.createdAt) | last | .number`)
		if err == nil {
			if n := parseFirstInt(lastNonEmpty(out)); n > 0 {
				return n
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no merge-train integration PR appeared on %s within %s", repo, timeout)
		}
		time.Sleep(10 * time.Second)
	}
}

// projectStatus returns the current board Status column of an issue, or "" if the
// item is not found on the board.
func projectStatus(t *testing.T, env *Env, repo string, issueNumber int) string {
	t.Helper()
	out, err := ghOutput(env, "project", "item-list", fmt.Sprint(env.ProjectNumber),
		"--owner", env.ProjectOwner, "--format", "json", "--limit", "200",
		"--jq", fmt.Sprintf(`.items[] | select(.content.number==%d and (.content.repository|ascii_downcase)==("%s"|ascii_downcase)) | .status`, issueNumber, repo))
	if err != nil {
		t.Logf("projectStatus: gh error for %s#%d: %v", repo, issueNumber, err)
		return ""
	}
	return strings.TrimSpace(lastNonEmpty(out))
}

// WaitForIssueComment polls the issue's comments until one contains substring, or
// timeout expires. Used to assert engine-posted lifecycle comments (e.g. the
// merge-train ejection notice) that are posted on every code path.
func WaitForIssueComment(t *testing.T, env *Env, repo string, issueNumber int, substring string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := ghOutput(env, "issue", "view", fmt.Sprint(issueNumber), "-R", repo,
			"--json", "comments", "--jq", ".comments[].body")
		if err == nil && strings.Contains(out, substring) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for comment containing %q on %s#%d", substring, repo, issueNumber)
		}
		time.Sleep(10 * time.Second)
	}
}

// uniqueMemberPath inserts "-<num>" before the file extension so each run's member
// file is unique (landed files persist on main). "e2e/train/entries/clean1.txt" +
// 42 → "e2e/train/entries/clean1-42.txt".
func uniqueMemberPath(path string, num int) string {
	slash := strings.LastIndex(path, "/")
	dot := strings.LastIndex(path, ".")
	if dot <= slash { // no extension in the basename
		return fmt.Sprintf("%s-%d", path, num)
	}
	return fmt.Sprintf("%s-%d%s", path[:dot], num, path[dot:])
}

// parseFirstInt extracts a leading integer from s (jq may emit "null").
func parseFirstInt(s string) int {
	s = strings.TrimSpace(s)
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0
	}
	return n
}

// waitForPRClosed polls until the PR is CLOSED or MERGED (both are terminal for
// a member PR the train has landed), up to timeout.
func waitForPRClosed(t *testing.T, env *Env, repo string, prNumber int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := ghOutput(env, "pr", "view", fmt.Sprint(prNumber), "-R", repo,
			"--json", "state", "--jq", ".state")
		if err == nil {
			switch strings.TrimSpace(out) {
			case "CLOSED", "MERGED":
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("member PR #%d on %s not closed within %s (last state: %q, err: %v)", prNumber, repo, timeout, strings.TrimSpace(out), err)
		}
		time.Sleep(10 * time.Second)
	}
}

// assertPRMerged fails unless the PR is in the MERGED state.
func assertPRMerged(t *testing.T, env *Env, repo string, prNumber int) {
	t.Helper()
	out, err := ghOutput(env, "pr", "view", fmt.Sprint(prNumber), "-R", repo,
		"--json", "state", "--jq", ".state")
	if err != nil {
		t.Fatalf("could not read state of integration PR #%d: %v\n%s", prNumber, err, out)
	}
	if got := strings.TrimSpace(out); got != "MERGED" {
		t.Fatalf("integration PR #%d state = %q, want MERGED (batch did not land atomically)", prNumber, got)
	}
}

// assertNoStaleTrainArtifacts fails if the repo still has open merge-train
// integration PRs after a scenario — a guard against the reconstruction bugs
// (permanent stall / orphaned remnants) surviving cleanup.
func assertNoStaleTrainArtifacts(t *testing.T, env *Env, repo string) {
	t.Helper()
	out, err := ghOutput(env, "pr", "list", "-R", repo, "--state", "open",
		"--json", "headRefName", "--jq", `[.[] | select(.headRefName | startswith("fabrik/merge-train/"))] | length`)
	if err != nil {
		t.Logf("warn: could not check for stale train PRs on %s: %v", repo, err)
		return
	}
	if n := parseFirstInt(lastNonEmpty(out)); n > 0 {
		t.Errorf("found %d open merge-train integration PR(s) still on %s after scenario", n, repo)
	}
}
