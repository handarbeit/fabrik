//go:build e2e

package e2e

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
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

// requireTrainBed skips the test unless (a) the suite is running in train
// mode "on" and (b) the test board has a "Queued" column — the one-time
// operator setup the merge train needs (ADR-059 D1).
//
// The mode check comes first and matters on its own: these scenarios place
// issues directly in Queued (QueueMember), a pure GitHub/board operation
// that succeeds regardless of mode. But under merge_train: off, nothing
// ever drains Queued — HoldingStage items are never individually dispatched,
// and handleMergeTrainBatch (the batch drain) only runs when
// e.cfg.MergeTrain == "on" (engine/poll.go). Without this check, running
// these scenarios during an off-mode run would place members that are never
// landed, and every wait helper below would burn its full 10-50 min timeout
// as a false failure instead of skipping cleanly.
func requireTrainBed(t *testing.T, env *Env) {
	t.Helper()
	if resolveTrainMode(t, env) != "on" {
		t.Skip("merge-train scenario requires train mode \"on\" — skipping under train mode \"off\"")
	}
	// Retry on transient errors (e.g. GraphQL rate-limit exhaustion — gh project runs
	// on GraphQL). A persistent read failure FAILS the test rather than silently
	// skipping, so a rate-limited run does not masquerade as "bed not set up".
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		var sf statusField
		sf, err = fetchStatusField(env)
		if err == nil {
			for _, o := range sf.Options {
				if strings.TrimSpace(o.Name) == "Queued" {
					return
				}
			}
			t.Skipf("test board %s/#%d has no Queued column — merge-train bed not set up (see tests/e2e/README.md)",
				env.ProjectOwner, env.ProjectNumber)
		}
		t.Logf("requireTrainBed: transient board-read error (attempt %d/6): %v", attempt+1, err)
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
	return createMemberPR(t, env, repo, baseBranch, branch, path, content, issueTitle, issueNum, false)
}

// CreateMemberPRDraft is CreateMemberPR, but opens the PR as a draft. The bed's
// real reviewer (Pruefer, as of #1396 — see tests/e2e/README.md's "Reviewer
// topology"; claude-review.yml was deleted 2026-08-13) only lists open,
// non-draft PRs each poll (cmd/pruefer/README.md) — a draft PR that is never
// marked ready is therefore permanently invisible to it. Scenarios whose
// property under test is "nothing has reviewed this PR yet" (e.g.
// expected_reviewers's declared-but-unrequested
// and undeclared-nothing-requested cases) use this instead of CreateMemberPR to
// avoid racing that bot's incidental review against the engine's first gate
// evaluation (see #1312).
func CreateMemberPRDraft(t *testing.T, env *Env, repo, baseBranch, branch, path, content, issueTitle string, issueNum int) int {
	t.Helper()
	return createMemberPR(t, env, repo, baseBranch, branch, path, content, issueTitle, issueNum, true)
}

func createMemberPR(t *testing.T, env *Env, repo, baseBranch, branch, path, content, issueTitle string, issueNum int, draft bool) int {
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
	args := []string{"pr", "create", "-R", repo,
		"--base", baseBranch, "--head", branch,
		"--title", issueTitle, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	out, err := ghOutput(env, args...)
	if err != nil {
		t.Fatalf("create member PR for #%d on %s: %v\n%s", issueNum, repo, err, out)
	}
	prNum := parseIssueNumberFromURL(lastNonEmpty(out))
	if prNum == 0 {
		t.Fatalf("could not parse member PR number from %q", out)
	}
	t.Logf("created member PR #%d (issue #%d, branch %s, path %s, draft=%v)", prNum, issueNum, branch, path, draft)
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

// QueueMemberOnBase is QueueMember for a non-default baseBranch (#1648): it additionally
// applies the base:<branch> label to the issue itself, ensuring the label exists first
// (gh issue create --label requires a pre-existing label, unlike the engine's own
// AddLabel path). This label is what the engine's own partitioning
// (groupQueuedByRepoAndBase/baseBranchForItem) reads to resolve the member's base — the
// PR's own actual base ref (set by CreateMemberPR) is not itself sufficient, since
// itemHasBaseLabel/baseBranchForItem look at the issue's labels, not the PR. QueueMember
// itself is left untouched for its many existing main-only callers (R1/AC4: default-base
// behavior must stay byte-identical).
func QueueMemberOnBase(t *testing.T, env *Env, repo, baseBranch, marker, path, content string) (int, int) {
	t.Helper()
	baseLabel := "base:" + baseBranch
	ensureLabelExists(t, env, repo, baseLabel)

	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e merge-train member %s (%s)", marker, stamp)
	num := FileIssue(t, env, repo, title,
		fmt.Sprintf("e2e merge-train member. marker=%s", marker), baseLabel)
	itemID := AddIssueToProject(t, env, repo, num)
	uPath := uniqueMemberPath(path, num)
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	prNum := CreateMemberPR(t, env, repo, baseBranch, branch, uPath, content, title, num)
	LinkedPRNumber(t, env, repo, num)
	SetIssueStatus(t, env, itemID, "Queued")
	t.Logf("queued member on base %q: issue #%d (label %s), PR #%d, at Status=Queued", baseBranch, num, baseLabel, prNum)
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
	items, err := fetchBoardItems(env)
	if err != nil {
		t.Logf("projectStatus: board query error for %s#%d: %v", repo, issueNumber, err)
		return ""
	}
	it, ok := findBoardItem(items, repo, issueNumber)
	if !ok {
		return ""
	}
	return strings.TrimSpace(it.Status)
}

// WaitForMemberLanded polls until a landed member reaches the durable landing
// signal: board Status == "Done" OR the issue is CLOSED. The two are not
// equivalent — this repo's project has a native "archive item when issue
// closes" automation that runs near-instantly after a member's issue closes
// (via the integration PR's Closes #N), and an archived board item drops out
// of `gh project item-list` entirely (status reads "" thereafter). A poll keyed
// solely on Status=="Done" can therefore miss a member that closes-and-archives
// between two poll ticks, even though the train landed it correctly. Closed is
// itself a durable, un-missable terminal state, so racing both signals and
// accepting whichever is observed first eliminates the miss.
func WaitForMemberLanded(t *testing.T, env *Env, repo string, issueNumber int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastStatus, lastState string
	for time.Now().Before(deadline) {
		lastStatus = projectStatus(t, env, repo, issueNumber)
		if lastStatus == "Done" {
			return
		}
		if state, err := tryIssueState(env, repo, issueNumber); err == nil {
			lastState = state
			if state == "CLOSED" {
				return
			}
		} else {
			t.Logf("WaitForMemberLanded: transient gh error reading issue state for %s#%d: %v (will retry)", repo, issueNumber, err)
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("timed out waiting for %s#%d to land (last observed status %q, issue state %q)",
		repo, issueNumber, lastStatus, lastState)
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

// landedPRPattern matches the engine-posted "landed" comment both landing
// paths post on the member's OWN PR (engine/merge_train.go: landSingleton's
// "Landed one-at-a-time via singleton PR #%d." and landMergeTrainBatch's
// "Landed via batch PR #%d."), capturing the distinct integration/singleton
// PR number the change actually landed through.
var landedPRPattern = regexp.MustCompile(`Landed (?:via batch|one-at-a-time via singleton) PR #(\d+)\.`)

// waitForLandingPRNumber polls the member's own PR comments (memberPRNum) for
// the engine's "landed via ..." comment and returns the distinct
// integration/singleton PR number it cites. This is scoped to the specific
// member PR, so — unlike log scanning or WaitForIntegrationPR's repo-wide
// "most recently created" search — it stays correct under t.Parallel()
// execution where sibling merge-train scenarios land unrelated batches
// concurrently in the same repo.
//
// This does depend on the engine's landed-comment AddComment call succeeding
// (engine/merge_train.go:1817/:812 log a warn and move on if it fails — it is
// not retried). That is the same best-effort-comment dependency
// WaitForIssueComment/WaitForPRCommentContaining already carry for other
// merge-train e2e scenarios (e.g. the "merge-train — ejected" and "runaway
// guard tripped" comments), so this is not a new class of flakiness for the
// suite. On timeout, check the bed log around this member's landing for
// "warn: could not post landed comment on PR #<memberPRNum>" — if present,
// the failure is this known-benign comment-post gap, not a stuck landing.
//
// No test-only fallback covers both landing paths reliably, so none is
// implemented here — see #1275 (engine-side retry of this AddComment call,
// the actual fix) for why: closedByPullRequestsReferences on the member
// issue can't substitute because landSingleton's own landing-PR body says
// "Lands #%d", not "Closes #%d" (engine/merge_train.go:778), so it never
// registers as a closing PR reference for that path; and bed-log scanning
// isn't member-scoped for the batch path's "merged integration PR #%d for
// %s" (engine/merge_train.go:1786, repo-only — ambiguous under concurrent
// merge-train activity), even though landSingleton's own log line happens to
// be ("merged singleton landing PR #%d for #%d", engine/merge_train.go:796).
func waitForLandingPRNumber(t *testing.T, env *Env, repo string, memberPRNum int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		bodies, err := tryPRComments(env, repo, memberPRNum)
		if err == nil {
			// Comments come back oldest-first; take the LAST match rather than
			// the first so a restart-driven repost of the landed comment (or
			// any other duplicate) yields the most recent — and therefore
			// authoritative — landing PR number, not a stale one from an
			// earlier partial run.
			found := 0
			for _, b := range bodies {
				if m := landedPRPattern.FindStringSubmatch(b); m != nil {
					if n, aerr := strconv.Atoi(m[1]); aerr == nil && n > 0 {
						found = n
					}
				}
			}
			if found > 0 {
				return found
			}
		} else {
			t.Logf("waitForLandingPRNumber: transient error reading PR #%d comments on %s: %v (will retry)", memberPRNum, repo, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a \"landed via ...\" comment on member PR #%d on %s (last err: %v) — "+
				"if the bed log shows \"warn: could not post landed comment on PR #%d\" around this landing, "+
				"the engine's best-effort comment post failed transiently (not retried, tracked as #1275); this "+
				"is a known, non-regression false failure — re-run the test", memberPRNum, repo, err, memberPRNum)
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
		t.Fatalf("integration/singleton PR #%d state = %q, want MERGED (did not land atomically)", prNumber, got)
	}
}

// WaitForNoStaleTrainArtifacts polls until the repo has no open merge-train
// integration PRs, up to timeout — a guard against the reconstruction bugs
// (permanent stall / orphaned remnants) surviving cleanup. This is a
// point-in-time condition racing the engine's own poll-cycle cleanup: a
// batch's terminal event (e.g. the runaway guard firing) pauses/alerts
// synchronously, but the orphaned trial/integration PR is reclaimed by the
// normal train-reconcile logic on a subsequent poll cycle, so a bare
// single-shot check can observe a PR that is already correctly scheduled for
// (but hasn't yet completed) cleanup.
func WaitForNoStaleTrainArtifacts(t *testing.T, env *Env, repo string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastCount int
	var lastErr error
	var sawReading bool
	for {
		out, err := ghOutput(env, "pr", "list", "-R", repo, "--state", "open",
			"--json", "headRefName", "--jq", `[.[] | select(.headRefName | startswith("fabrik/merge-train/"))] | length`)
		lastErr = err
		if err == nil {
			sawReading = true
			lastCount = parseFirstInt(lastNonEmpty(out))
			if lastCount == 0 {
				return
			}
			t.Logf("WaitForNoStaleTrainArtifacts: %d open merge-train integration PR(s) still on %s (will retry)", lastCount, repo)
		} else {
			t.Logf("WaitForNoStaleTrainArtifacts: transient gh error checking for stale train PRs on %s: %v (will retry)", repo, err)
		}
		if time.Now().After(deadline) {
			if !sawReading {
				t.Fatalf("could not check for stale merge-train PRs on %s after %s — no successful reading (last err: %v)", repo, timeout, lastErr)
			}
			t.Fatalf("found %d open merge-train integration PR(s) still on %s after %s", lastCount, repo, timeout)
		}
		time.Sleep(10 * time.Second)
	}
}
