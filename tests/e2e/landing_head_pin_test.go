//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ghapi "github.com/handarbeit/fabrik/github"
)

// TestLandingHeadPinRefusedOnMovedHead is the live-e2e half of #2342 (ADR 2342): it
// proves that real GitHub refuses a merge, an auto-merge enablement and an enqueue
// whose pinned head is no longer the PR head, and that the client classifies each
// refusal as gh.ErrHeadMoved.
//
// The sim twins (tests/sim/landing_head_pin_test.go) prove the engine flow with an
// injected push between the gate and the merge request, but the sim models GitHub's
// refusal from its documentation, not from its wire: the error text of the two
// GraphQL mutations is undocumented. This test closes that gap. It cannot drive the
// race inside one engine poll (the gates and the merge run within a single poll), so
// it reproduces the precondition directly: capture a head A, push a second commit B,
// then issue each pinned request with A through the same client methods the engine
// uses.
//
// Each refusal's raw text is logged so it can be captured as a wire-contract
// fixture (github/testdata). The auto-merge and enqueue legs accept the refusals a
// repository without those features gives ("auto-merge not enabled", not queued): a
// refusal is classified from the live head, so a moved head is ErrHeadMoved either
// way, and the leg only fails if the request SUCCEEDS.
//
// No Claude, no engine involvement beyond the bed being up. The PR is closed unmerged
// on teardown; nothing lands on the base branch.
func TestLandingHeadPinRefusedOnMovedHead(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	repo := env.RepoAlpha
	const base = "main"
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		t.Fatalf("repo %q is not owner/name", repo)
	}

	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e landing head pin (%s)", stamp)
	// The issue is deliberately NOT added to the project board: the engine must not
	// act on it; only the PR and its branch matter here.
	issue := FileIssue(t, env, repo, title, "e2e landing head pin scenario (#2342). Not on the board.")
	branch := fmt.Sprintf("fabrik/issue-%d", issue)
	path := uniqueMemberPath("e2e/landing-head-pin/a.md", issue)
	pr := createMemberPRBody(t, env, repo, base, branch, path, "# head A\n", title,
		"e2e landing head pin scenario PR. No closing keyword; closed unmerged on teardown.", issue, false)
	t.Cleanup(func() { _, _ = ghOutput(env, "pr", "close", fmt.Sprint(pr), "-R", repo) })
	AwaitPRForBranchVisible(t, env, repo, issue, awaitSeedTimeout)

	headA, err := prHeadSHA(env, repo, pr)
	if err != nil || headA == "" {
		t.Fatalf("reading head A of PR #%d: %v", pr, err)
	}
	// Push head B.
	pathB := uniqueMemberPath("e2e/landing-head-pin/b.md", issue)
	if out, err := ghOutput(env, "api", "--method", "PUT", fmt.Sprintf("repos/%s/contents/%s", repo, pathB),
		"-f", "message=e2e: move the PR head after it was judged",
		"-f", "content=IyBoZWFkIEIK", // "# head B\n"
		"-f", "branch="+branch); err != nil {
		t.Fatalf("pushing head B: %v\n%s", err, out)
	}
	// The pinned merge's own pre-check reads the PR head; wait until GitHub reports B
	// so the legs below start from the moved state rather than racing the push.
	deadline := time.Now().Add(2 * time.Minute)
	var headB string
	for time.Now().Before(deadline) {
		headB, _ = prHeadSHA(env, repo, pr)
		if headB != "" && headB != headA {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if headB == "" || headB == headA {
		Inconclusive(t, "GitHub did not report the pushed head within 2m (A=%s, B=%q)", headA, headB)
	}
	t.Logf("PR #%d head moved %s -> %s", pr, headA[:8], headB[:8])

	client := ghapi.NewClient(env.GHToken)
	assertRefused := func(leg string, err error, accept ...error) {
		t.Helper()
		t.Logf("%s refusal (raw): %v", leg, err)
		if err == nil {
			t.Fatalf("%s at the stale head %s SUCCEEDED — the engine's pin would not protect anything", leg, headA[:8])
		}
		if errors.Is(err, ghapi.ErrHeadMoved) {
			return
		}
		for _, a := range accept {
			if errors.Is(err, a) {
				return
			}
		}
		t.Errorf("%s at the stale head: err = %v, want errors.Is(ErrHeadMoved)", leg, err)
	}

	assertRefused("merge", client.MergePRAtHeadSHA(owner, name, pr, headA))
	assertRefused("enable auto-merge", client.EnablePullRequestAutoMerge(owner, name, pr, "MERGE", headA), ghapi.ErrAutoMergeNotEnabled)
	assertRefused("enqueue", client.EnqueuePullRequest(owner, name, pr, headA))

	// Nothing merged or queued.
	if state, err := restPRState(env, repo, pr); err != nil || state == "MERGED" {
		t.Errorf("PR #%d state = %q (err %v) — a stale-head request landed it", pr, state, err)
	}
}
