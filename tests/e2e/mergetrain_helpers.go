//go:build e2e

package e2e

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

// PrepareMemberExactPath is QueueMember minus two things: it does NOT uniquify the
// path (uniqueMemberPath) and it does NOT place the issue in Queued. It files the
// issue, adds it to the project, creates the member PR on fabrik/issue-<N> at
// exactly path, and confirms the PR is resolvable — leaving the caller to move
// itemID to Queued via SetIssueStatus.
//
// Both differences exist for the conflict scenario: two members must write the SAME
// path with divergent content to conflict textually (QueueMember's per-issue
// suffix would make every member's path distinct), and all members must enter Queued
// back to back so a train worker cannot form a partial batch while slow gh calls
// for a later member are still in flight. The caller owns path uniqueness across
// runs (a landed batch merges its files into main).
func PrepareMemberExactPath(t *testing.T, env *Env, repo, baseBranch, marker, path, content string) (issueNum, prNum int, itemID string) {
	t.Helper()
	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e merge-train member %s (%s)", marker, stamp)
	issueNum = FileIssue(t, env, repo, title,
		fmt.Sprintf("e2e merge-train member. marker=%s", marker))
	itemID = AddIssueToProject(t, env, repo, issueNum)
	branch := fmt.Sprintf("fabrik/issue-%d", issueNum)
	prNum = CreateMemberPR(t, env, repo, baseBranch, branch, path, content, title, issueNum)
	LinkedPRNumber(t, env, repo, issueNum)
	t.Logf("prepared member %s: issue #%d, PR #%d, path %s (not yet Queued)", marker, issueNum, prNum, path)
	return issueNum, prNum, itemID
}

// readLogLinesFrom returns every line of the test bed's fabrik.log from offset to
// EOF, for callers that must reason about line ORDER within a window (which
// CountLogLines/WaitForLogLine cannot express). Call it only once the terminal
// state being analysed has already been observed through GitHub state, so the
// window is known to be complete.
func readLogLinesFrom(t *testing.T, env *Env, offset int64) []string {
	t.Helper()
	f, err := os.Open(env.LogPath)
	if err != nil {
		t.Fatalf("open %s: %v", env.LogPath, err)
	}
	defer f.Close()
	if _, err := f.Seek(offset, 0); err != nil {
		t.Fatalf("seek %s to %d: %v", env.LogPath, offset, err)
	}
	var lines []string
	scanner := bufio.NewScanner(f)
	// Same buffer sizing as CountLogLines: engine lines can exceed the 64KB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning %s from offset %d: %v", env.LogPath, offset, err)
	}
	return lines
}

// waitForLogLineOrFail polls the bed log from offset until a line containing want
// appears (returned), or fails the test if a line containing any failOn substring
// appears first (each failOn entry maps a substring to the diagnostic to print),
// or timeout expires. Unlike WaitForLogLine it lets a scenario turn a known
// early-exit cause (e.g. the admission gate deferring a member) into a named
// failure instead of a bare timeout. want is checked before failOn within a line.
func waitForLogLineOrFail(t *testing.T, env *Env, want string, failOn map[string]string, offset int64, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if line, err := tryLogLineContaining(env, want, offset); err == nil && line != "" {
			return line
		} else if err != nil {
			t.Logf("waitForLogLineOrFail: transient log read error: %v (will retry)", err)
		}
		for sub, why := range failOn {
			if line, err := tryLogLineContaining(env, sub, offset); err == nil && line != "" {
				t.Fatalf("saw %q while waiting for %q: %s\nlog line: %s", sub, want, why, strings.TrimSpace(line))
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for log line containing %q (scanned from offset %d)", timeout, want, offset)
		}
		time.Sleep(5 * time.Second)
	}
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
		// REST, via tryPRComments: GitHub numbers issues and PRs in one space
		// and serves both from repos/{o}/{r}/issues/{n}/comments, so this is
		// the same endpoint for an issue as for a PR despite the helper's
		// name. Verified against live issues (not PRs) with comments: bodies
		// identical to `gh issue view --json comments`.
		//
		// This loop is why it matters: 10s interval for up to 25 minutes in
		// mergetrain_bisect_test.go is ~150 GraphQL points per use on the old
		// path (found in review — it was missed in the first pass, not
		// deliberately left).
		bodies, err := tryPRComments(env, repo, issueNumber)
		if err == nil {
			for _, b := range bodies {
				if strings.Contains(b, substring) {
					return
				}
			}
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
		out, err := restPRState(env, repo, prNumber)
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

// landedPRPattern matches the engine-posted "landed" comment every landing
// path posts on the member's OWN PR (engine/merge_train.go: landSingleton's
// "Landed one-at-a-time via singleton PR #%d.", landMergeTrainBatch's "Landed
// via batch PR #%d.", and finishSingletonFastPathLanding's "Landed via
// singleton fast path PR #%d."), capturing the PR number the change actually
// landed through.
//
// For the first two that number is a DISTINCT integration/singleton PR. For
// the fast path (#1644) it is the member's own PR, because that path lands the
// member PR directly rather than minting a landing PR — so callers comparing
// the captured number against the member PR must treat equality as legitimate
// there. See waitForLandingPRNumber's contract below.
var landedPRPattern = regexp.MustCompile(`Landed (?:via batch|one-at-a-time via singleton|via singleton fast path) PR #(\d+)\.`)

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
// waitForLandingPRNumber returns just the landing PR number, for callers that
// do not need to distinguish which landing path posted the comment. Callers
// that assert on the landing PR being DISTINCT from the member's own PR must
// use waitForLandingPRDetail instead: that invariant does not hold for the
// singleton fast path (#1644), where the member's own PR legitimately is the
// landing PR.
func waitForLandingPRNumber(t *testing.T, env *Env, repo string, memberPRNum int, timeout time.Duration) int {
	t.Helper()
	n, _ := waitForLandingPRDetail(t, env, repo, memberPRNum, timeout)
	return n
}

// waitForLandingPRDetail is waitForLandingPRNumber plus the path indicator:
// viaFastPath reports whether the comment found was the singleton fast path's
// (#1644), in which case the returned PR number IS memberPRNum by design.
func waitForLandingPRDetail(t *testing.T, env *Env, repo string, memberPRNum int, timeout time.Duration) (landingPR int, viaFastPath bool) {
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
			fast := false
			for _, b := range bodies {
				if m := landedPRPattern.FindStringSubmatch(b); m != nil {
					if n, aerr := strconv.Atoi(m[1]); aerr == nil && n > 0 {
						found = n
						fast = strings.Contains(m[0], "via singleton fast path")
					}
				}
			}
			if found > 0 {
				return found, fast
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
	out, err := restPRState(env, repo, prNumber)
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

// ── Batch-cap scenario helpers (#1850, ADR-1833) ───────────────────────────
//
// TestMergeTrainQueuedDeeperThanBatchCap needs a batch to form ONLY once all of
// its members are Queued: the engine has no batching dwell, so a poll that lands
// while members are still being queued would dispatch a worker on a partial
// batch. fabrik:paused is the one exclusion a test controls
// (groupQueuedItemsByRepoUnordered drops paused items), so members are queued
// paused and then unpaused together.

// pausedLabel is the engine's pause label. It is created by the engine in normal
// operation; ensurePausedLabelExists only guards a bed where it does not yet exist.
const pausedLabel = "fabrik:paused"

// ensurePausedLabelExists creates fabrik:paused on repo if it is missing, so that
// gh issue create --label does not fail. Unlike ensureLabelExists it registers NO
// delete cleanup: the label is engine-owned and shared by every scenario, and
// deleting it repo-wide would strip it from unrelated issues. Never swap this
// for ensureLabelExists.
func ensurePausedLabelExists(t *testing.T, env *Env, repo string) {
	t.Helper()
	exists := func() bool {
		_, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/labels/%s", repo, "fabrik%3Apaused"))
		return err == nil
	}
	if exists() {
		return
	}
	out, err := ghOutput(env, "label", "create", pausedLabel, "-R", repo, "--color", "e99695")
	if err != nil && !exists() {
		t.Fatalf("ensure label %q exists on %s: %v\n%s", pausedLabel, repo, err, out)
	}
}

// QueueMemberPaused is QueueMember for the default base, except the issue is
// created already carrying fabrik:paused — so it is never visible to the engine
// without the label — and stays paused after being placed in Queued. The caller
// releases the whole batch with removePausedConcurrently. QueueMember itself is
// untouched for its existing callers.
func QueueMemberPaused(t *testing.T, env *Env, repo, baseBranch, marker, path, content string) (int, int) {
	t.Helper()
	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e merge-train member %s (%s)", marker, stamp)
	num := FileIssue(t, env, repo, title,
		fmt.Sprintf("e2e merge-train member. marker=%s", marker), pausedLabel)
	itemID := AddIssueToProject(t, env, repo, num)
	uPath := uniqueMemberPath(path, num)
	branch := fmt.Sprintf("fabrik/issue-%d", num)
	prNum := CreateMemberPR(t, env, repo, baseBranch, branch, uPath, content, title, num)
	LinkedPRNumber(t, env, repo, num)
	SetIssueStatus(t, env, itemID, "Queued")
	t.Logf("queued PAUSED member: issue #%d, PR #%d, at Status=Queued", num, prNum)
	return num, prNum
}

// removePausedConcurrently removes fabrik:paused from every issue at once (REST,
// one goroutine per issue) so the engine's view of the batch flips from "none
// visible" to "all visible" inside roughly one API round-trip, rather than
// across the seconds a sequential loop would take. Goroutines never call
// t.Fatal; every failure is returned.
func removePausedConcurrently(env *Env, repo string, nums []int) []error {
	errs := make([]error, len(nums))
	var wg sync.WaitGroup
	for i, n := range nums {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			out, err := ghOutput(env, "api", "-X", "DELETE",
				fmt.Sprintf("repos/%s/issues/%d/labels/%s", repo, n, "fabrik%3Apaused"))
			if err != nil {
				errs[i] = fmt.Errorf("remove %s from %s#%d: %v: %s", pausedLabel, repo, n, err, strings.TrimSpace(out))
			}
		}(i, n)
	}
	wg.Wait()
	var failed []error
	for _, e := range errs {
		if e != nil {
			failed = append(failed, e)
		}
	}
	return failed
}

// repauseOnFailure registers a cleanup that, only when the test failed, re-applies
// fabrik:paused to the issue if it is still open — so a failed run cannot leave
// un-paused Queued members to join a later scenario's batch. Call it right after
// the member is queued: cleanups run LIFO, so it runs before FileIssue's own
// close-the-issue cleanup for the same member. Best-effort by design.
func repauseOnFailure(t *testing.T, env *Env, repo string, num int) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if state, err := tryIssueState(env, repo, num); err == nil && state != "OPEN" {
			return
		}
		_, _ = ghOutput(env, "issue", "edit", fmt.Sprint(num), "-R", repo, "--add-label", pausedLabel)
	})
}

// staleQueuedMembers returns open, non-paused issues on repo that already sit in
// Queued. Any such item would join this scenario's (repo, base) partition and
// change the batch composition, so the caller fails loudly rather than run.
func staleQueuedMembers(env *Env, repo string) ([]int, error) {
	items, err := fetchBoardItems(env)
	if err != nil {
		return nil, err
	}
	var stale []int
	for _, it := range items {
		if !strings.EqualFold(it.Repo, repo) || strings.TrimSpace(it.Status) != "Queued" {
			continue
		}
		state, err := tryIssueState(env, repo, it.Number)
		if err != nil {
			return nil, fmt.Errorf("read state of %s#%d: %w", repo, it.Number, err)
		}
		if state != "OPEN" {
			continue
		}
		labels, err := tryIssueLabels(env, repo, it.Number)
		if err != nil {
			return nil, fmt.Errorf("read labels of %s#%d: %w", repo, it.Number, err)
		}
		paused := false
		for _, l := range labels {
			if l == pausedLabel {
				paused = true
			}
		}
		if !paused {
			stale = append(stale, it.Number)
		}
	}
	sort.Ints(stale)
	return stale, nil
}

// configuredMaxBatchSize best-effort reads the bed's configured max_batch_size:
// FABRIK_MAX_BATCH_SIZE from the bed .env wins (it takes precedence over the
// config file, cmd/root.go), else a top-level max_batch_size line in
// .fabrik/config.yaml. Returns 0 when neither is set (engine default applies).
// This is only a pre-flight; the authoritative check is the engine's own
// "batch capped … max_batch_size=N" log line.
func configuredMaxBatchSize(env *Env) int {
	if v, err := readEnvFileValue(filepath.Join(env.FabrikTestDir, ".env"), "FABRIK_MAX_BATCH_SIZE"); err == nil && strings.TrimSpace(v) != "" {
		if n, aerr := strconv.Atoi(strings.TrimSpace(v)); aerr == nil {
			return n
		}
		return -1 // set but unparseable: not the default of 5
	}
	data, err := os.ReadFile(filepath.Join(env.FabrikTestDir, ".fabrik", "config.yaml"))
	if err != nil {
		return 0
	}
	if m := regexp.MustCompile(`(?m)^max_batch_size:\s*(\d+)\s*(?:#.*)?$`).FindSubmatch(data); m != nil {
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	return 0
}

// logLinesSince returns every line of the bed log from offset to EOF.
func logLinesSince(t *testing.T, env *Env, offset int64) []string {
	t.Helper()
	lines, err := allLogLinesContaining(env, "", offset)
	if err != nil {
		t.Fatalf("read %s from offset %d: %v", env.LogPath, offset, err)
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r\n")
	}
	return lines
}

// waitForLogMatch polls the log from offset until some line satisfies match, and
// returns that line. WaitForLogLine can only substring-match; the scenario needs
// to match on a parsed field (the repo and trainKey a line names).
func waitForLogMatch(t *testing.T, env *Env, offset int64, timeout time.Duration, what string, match func(line string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, l := range logLinesSince(t, env, offset) {
			if match(l) {
				return l
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for log line: %s (scanned from offset %d)", timeout, what, offset)
		}
		time.Sleep(10 * time.Second)
	}
}

// The parsers below match the engine's own format strings, copied verbatim:
//
//	engine/poll.go        "batch capped for %s: %d Queued item(s) exceed max_batch_size=%d — landing first %d by entry order"
//	engine/poll.go        "batch snapshot for %s: %d item(s) — #A \"title\", …"
//	engine/merge_train.go "opened draft CI PR #%d for %s/%s (%d survivor(s))"
//	engine/merge_train.go "merged integration PR #%d for %s"          (%s = owner/repo)
//	engine/merge_train.go "landing complete for %s (integration PR #%d, %d members)"   (%s = trainKey)
//
// The strings have no compile-time link to the engine, so a wording change there
// makes these return ok=false and the scenario fails loudly on "line not found"
// rather than passing vacuously. mergetrain_batchcap_parse_test.go pins them.
// trainKey is the bare owner/repo for the default-base partition; a non-default
// base is owner/repo:branch — the trailing ": " / " (" in each pattern keeps a
// key from matching a longer one.

var snapshotMemberRE = regexp.MustCompile(`(?:^|, )#(\d+) "`)

// parseBatchSnapshot parses a "batch snapshot for <trainKey>: N item(s) — …"
// line, returning the member issue numbers in listed order.
func parseBatchSnapshot(line, trainKey string) ([]int, bool) {
	head := "batch snapshot for " + trainKey + ": "
	i := strings.Index(line, head)
	if i < 0 {
		return nil, false
	}
	rest := line[i+len(head):]
	j := strings.Index(rest, " item(s) — ")
	if j < 0 {
		return nil, false
	}
	want, err := strconv.Atoi(rest[:j])
	if err != nil {
		return nil, false
	}
	var members []int
	for _, m := range snapshotMemberRE.FindAllStringSubmatch(rest[j+len(" item(s) — "):], -1) {
		n, _ := strconv.Atoi(m[1])
		members = append(members, n)
	}
	if len(members) != want {
		return nil, false
	}
	return members, true
}

// parseBatchCapped parses a "batch capped for <trainKey>: Q Queued item(s) exceed
// max_batch_size=M — landing first M by entry order" line.
func parseBatchCapped(line, trainKey string) (queued, maxBatch int, ok bool) {
	re := regexp.MustCompile(`batch capped for ` + regexp.QuoteMeta(trainKey) +
		`: (\d+) Queued item\(s\) exceed max_batch_size=(\d+) — landing first (\d+) by entry order`)
	m := re.FindStringSubmatch(line)
	if m == nil || m[2] != m[3] {
		return 0, 0, false
	}
	queued, _ = strconv.Atoi(m[1])
	maxBatch, _ = strconv.Atoi(m[2])
	return queued, maxBatch, true
}

// parseOpenedDraftCI parses "opened draft CI PR #N for <owner/repo> (K survivor(s))".
func parseOpenedDraftCI(line, repo string) (pr, survivors int, ok bool) {
	re := regexp.MustCompile(`opened draft CI PR #(\d+) for ` + regexp.QuoteMeta(repo) + ` \((\d+) survivor\(s\)\)`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	pr, _ = strconv.Atoi(m[1])
	survivors, _ = strconv.Atoi(m[2])
	return pr, survivors, true
}

// parseMergedIntegration parses "merged integration PR #N for <owner/repo>".
func parseMergedIntegration(line, repo string) (pr int, ok bool) {
	re := regexp.MustCompile(`merged integration PR #(\d+) for ` + regexp.QuoteMeta(repo) + `\s*$`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	pr, _ = strconv.Atoi(m[1])
	return pr, true
}

// parseLandingComplete parses "landing complete for <trainKey> (integration PR #N, K members)".
func parseLandingComplete(line, trainKey string) (pr, members int, ok bool) {
	re := regexp.MustCompile(`landing complete for ` + regexp.QuoteMeta(trainKey) + ` \(integration PR #(\d+), (\d+) members\)`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		return 0, 0, false
	}
	pr, _ = strconv.Atoi(m[1])
	members, _ = strconv.Atoi(m[2])
	return pr, members, true
}

// sameMemberSet reports whether a and b hold the same numbers, ignoring order.
func sameMemberSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]int(nil), a...)
	y := append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// trainPR is one merge-train trial/integration PR, reduced to what the
// batch-cap scenario asserts on.
type trainPR struct {
	Number  int
	Merged  bool
	State   string // REST state: "open" or "closed"
	Members []int  // issue numbers from the body's "Closes #N" lines, ascending
}

var closesLineRE = regexp.MustCompile(`(?m)^Closes #(\d+)\s*$`)

// listTrainPRsSince lists PRs on repo whose head is under fabrik/merge-train/,
// created at or after since, and whose body's "Closes #N" lines include at
// least one of members — so PRs from any other scenario are excluded by
// membership, not by timing luck. Ascending by PR number. REST, not GraphQL.
func listTrainPRsSince(env *Env, repo string, since time.Time, members []int) ([]trainPR, error) {
	out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/pulls?state=all&per_page=100", repo),
		"--jq", `[.[] | select(.head.ref | startswith("fabrik/merge-train/")) | {number, state, merged: (.merged_at != null), created_at, body: (.body // "")}]`)
	if err != nil {
		return nil, fmt.Errorf("list train PRs on %s: %v: %s", repo, err, strings.TrimSpace(out))
	}
	var raw []struct {
		Number    int    `json:"number"`
		State     string `json:"state"`
		Merged    bool   `json:"merged"`
		CreatedAt string `json:"created_at"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); err != nil {
		return nil, fmt.Errorf("parse train PR list: %w", err)
	}
	ours := map[int]bool{}
	for _, m := range members {
		ours[m] = true
	}
	var prs []trainPR
	for _, r := range raw {
		created, perr := time.Parse(time.RFC3339, r.CreatedAt)
		if perr != nil || created.Before(since) {
			continue
		}
		var closes []int
		mine := false
		for _, m := range closesLineRE.FindAllStringSubmatch(r.Body, -1) {
			n, _ := strconv.Atoi(m[1])
			closes = append(closes, n)
			if ours[n] {
				mine = true
			}
		}
		if !mine {
			continue
		}
		sort.Ints(closes)
		prs = append(prs, trainPR{Number: r.Number, Merged: r.Merged, State: r.State, Members: closes})
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	return prs, nil
}
