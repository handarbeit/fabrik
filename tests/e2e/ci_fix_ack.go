//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Helpers for TestCIFixReinvoke's run-ID acknowledgement nonce (#1991, ADR 1991).
//
// The bed's ci-fix-sentinel job, for a PR whose body carries ackSentinelMarker,
// resolves a nonce T = the ID of the EARLIEST ci.yml pull_request run on the PR's
// head branch, and passes only if the repo root holds a file ackFile containing
// the line "ack:<T>" and the current run is not run T itself. Run T is therefore
// red by construction (its own ID cannot be in a file committed before it
// existed), and T is a future GitHub-assigned ID — not derivable from the
// worktree, git history or ci.yml — so the Implement agent cannot pre-empt it.
// When the sentinel fails it emits an Actions annotation naming T; that
// annotation is the only way T reaches the reinvoked agent, via the engine's
// CI-fix prompt (engine/ci.go ciFailureDetail).
const (
	// bedCIWorkflowFile is the bed's CI workflow file; its runs define T.
	bedCIWorkflowFile = "ci.yml"
	// ackSentinelMarker is the PR-body marker selecting the ack branch of the
	// sentinel job (the sibling -required and -unfixable markers are other tests').
	ackSentinelMarker = "ci-fix-sentinel-ack"
	// ackFile is the repo-root file the CI-fix commit must create.
	ackFile = "CI_FIX_ACK"
	// sentinelCheckName is the sentinel's job / check-run name.
	sentinelCheckName = "ci-fix-sentinel"
)

// ackLine is the line the ack file must contain for nonce T.
func ackLine(nonce int64) string { return fmt.Sprintf("ack:%d", nonce) }

// parseMinRunID parses the output of `--jq '[…id] | min'`: a decimal ID, or
// "null"/"" when there is no run yet.
func parseMinRunID(out string) (int64, error) {
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return 0, fmt.Errorf("no workflow run yet")
	}
	id, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse run id %q: %w", out, err)
	}
	return id, nil
}

// promptMissing returns what the CI-fix prompt log line lacks of the failure
// context it must carry: the failing check's name and the nonce's ack line. An
// empty result means the prompt provably carried the CI failure (R1) — "the
// engine retried" alone is not enough.
func promptMissing(line string, nonce int64) []string {
	var missing []string
	for _, want := range []string{sentinelCheckName, ackLine(nonce)} {
		if !strings.Contains(line, want) {
			missing = append(missing, want)
		}
	}
	return missing
}

// tryEarliestCIRunID returns the ID of the earliest bed-CI pull_request run on
// branch — the nonce T. Errors are transient (callers retry).
func tryEarliestCIRunID(env *Env, repo, branch string) (int64, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return 0, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/actions/workflows/%s/runs?branch=%s&event=pull_request&per_page=100",
			owner, name, bedCIWorkflowFile, branch),
		"--jq", "[.workflow_runs[].id] | min")
	if err != nil {
		return 0, fmt.Errorf("list ci runs: %w: %s", err, strings.TrimSpace(out))
	}
	return parseMinRunID(out)
}

// trySentinelJob returns the sentinel job's ID (== its check-run ID) and
// conclusion ("" while not completed) within workflow run runID.
func trySentinelJob(env *Env, repo string, runID int64) (jobID int64, conclusion string, err error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return 0, "", fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/actions/runs/%d/jobs", owner, name, runID),
		"--jq", fmt.Sprintf(`.jobs[] | select(.name==%q) | "\(.id)\t\(.conclusion // "")"`, sentinelCheckName))
	if err != nil {
		return 0, "", fmt.Errorf("list jobs of run %d: %w: %s", runID, err, strings.TrimSpace(out))
	}
	fields := strings.Split(strings.TrimSpace(out), "\t")
	if len(fields) != 2 {
		return 0, "", fmt.Errorf("no %s job in run %d yet (got %q)", sentinelCheckName, runID, strings.TrimSpace(out))
	}
	jobID, err = strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse job id %q: %w", fields[0], err)
	}
	return jobID, fields[1], nil
}

// trySentinelAnnotations returns every annotation (title and message) on the
// check run as one string.
func trySentinelAnnotations(env *Env, repo string, checkRunID int64) (string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/check-runs/%d/annotations?per_page=50", owner, name, checkRunID),
		"--jq", `.[] | "\(.title // ""): \(.message)"`)
	if err != nil {
		return "", fmt.Errorf("annotations of check run %d: %w: %s", checkRunID, err, strings.TrimSpace(out))
	}
	return out, nil
}

// tryFileAtRef returns the content of path at ref (branch or SHA); an error
// when the file does not exist there.
func tryFileAtRef(env *Env, repo, path, ref string) (string, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return "", fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api", "-H", "Accept: application/vnd.github.raw",
		fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, name, path, ref))
	if err != nil {
		return "", fmt.Errorf("read %s@%s: %w: %s", path, ref, err, strings.TrimSpace(out))
	}
	return out, nil
}

// assertSentinelAckWorkflow fails (never skips — a bed without the mechanism
// would otherwise silently reintroduce #916's un-runnable test) when the bed's
// CI workflow on main lacks the ack branch of the sentinel job. See
// "Bed workflow for TestCIFixReinvoke" in tests/e2e/README.md.
func assertSentinelAckWorkflow(t *testing.T, env *Env, repo string) {
	t.Helper()
	wf, err := tryFileAtRef(env, repo, ".github/workflows/"+bedCIWorkflowFile, "main")
	if err != nil {
		t.Fatalf("cannot read the bed's CI workflow to confirm the %s mechanism: %v", ackSentinelMarker, err)
	}
	if !strings.Contains(wf, ackSentinelMarker) {
		t.Fatalf("%s's .github/workflows/%s has no %q branch — apply the workflow change in "+
			"tests/e2e/README.md (\"Bed workflow for TestCIFixReinvoke\") before running this test",
			repo, bedCIWorkflowFile, ackSentinelMarker)
	}
}

// waitForFirstSentinelRunRed resolves the nonce T and waits for run T's sentinel
// job to conclude. It fails fast — naming the pre-emption — if that first run is
// green (the agent satisfied the sentinel before CI ran: the #916 failure mode),
// and fails if the failure annotation does not carry ack:<T> (the bed mechanism
// is not delivering the nonce). Returns T.
func waitForFirstSentinelRunRed(t *testing.T, env *Env, repo, branch string, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		nonce, err := tryEarliestCIRunID(env, repo, branch)
		if err != nil {
			lastErr = err
			pollSleep(pollBase())
			continue
		}
		jobID, conclusion, err := trySentinelJob(env, repo, nonce)
		if err != nil {
			lastErr = err
			pollSleep(pollBase())
			continue
		}
		switch conclusion {
		case "":
			lastErr = fmt.Errorf("run %d's %s job still running", nonce, sentinelCheckName)
			pollSleep(pollBase())
			continue
		case "failure":
			ann, err := trySentinelAnnotations(env, repo, jobID)
			if err != nil {
				lastErr = err
				pollSleep(pollBase())
				continue
			}
			if !strings.Contains(ann, ackLine(nonce)) {
				t.Fatalf("first CI run %d on %s failed, but its %s annotations do not carry %q — the bed's "+
					"sentinel is not delivering the nonce (annotations: %q)", nonce, branch, sentinelCheckName, ackLine(nonce), ann)
			}
			return nonce
		default:
			t.Fatalf("the FIRST CI run (%d) on %s concluded %q, not failure — the Implement agent pre-empted the "+
				"sentinel (or the bed's ack mechanism is inactive), so the CI-fix reinvoke can never fire; failing fast "+
				"instead of waiting out the pipeline (#916 failure mode)", nonce, branch, conclusion)
		}
	}
	t.Fatalf("timed out waiting for the first CI run's %s job on %s to fail (last: %v)", sentinelCheckName, branch, lastErr)
	return 0
}
