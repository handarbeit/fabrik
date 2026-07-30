//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// review_authority e2e helpers (ADR-1250, issue #1258). Authoritative-mode
// coverage needs a stage whose review_authority is "authoritative" without
// touching the bed's default stage config (Review/Validate stay advisory).
// This is a one-time, bed-local operator prerequisite: a "Review-Authoritative"
// board column plus a matching stage YAML (a copy of review.yaml with
// review_authority: authoritative added), mirroring the existing
// Queued/queued.yaml precedent used by the merge-train scenarios. See
// tests/e2e/README.md for the exact setup and adrs/1258-e2e-review-authority-coverage.md
// for why this mechanism — not a Validate variant — is what's reachable here.

// requireAuthoritativeBed skips the test unless the test board has a
// "Review-Authoritative" column — the one-time operator setup these
// scenarios need. Mirrors requireTrainBed's retry-on-transient-error
// behavior: a persistent read failure FAILS the test (not a silent skip),
// so a rate-limited run doesn't masquerade as "bed not set up".
func requireAuthoritativeBed(t *testing.T, env *Env) {
	t.Helper()
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		var sf statusField
		sf, err = fetchStatusField(env)
		if err == nil {
			for _, o := range sf.Options {
				if strings.TrimSpace(o.Name) == "Review-Authoritative" {
					return
				}
			}
			t.Skipf("test board %s/#%d has no Review-Authoritative column — "+
				"authoritative-mode e2e bed not set up (see tests/e2e/README.md)",
				env.ProjectOwner, env.ProjectNumber)
		}
		t.Logf("requireAuthoritativeBed: transient board-read error (attempt %d/6): %v", attempt+1, err)
		time.Sleep(20 * time.Second)
	}
	t.Fatalf("could not read board columns after 6 attempts (last: %v) — GraphQL rate limit or API issue, not a skip condition", err)
}

// seedReviewGateItem files an issue, adds it to the project, builds a member
// PR directly via the GitHub API (CreateMemberPR — no Claude cost), seeds
// stage:<column>:complete, and places the item at column. This exercises the
// catch-up loop's gate/settle logic (checkReviewGate via handleReviewGate)
// without ever invoking Claude for the gated stage — the same zero-cost
// construction precedent as TestPausedMergedPRRecovery's R5. extraLabels are
// applied at file time (e.g. "fabrik:yolo" for the composition scenario).
func seedReviewGateItem(t *testing.T, env *Env, repo, baseBranch, column, marker string, extraLabels ...string) (issueNum, prNum int, itemID string) {
	t.Helper()
	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e review-authority %s (%s)", marker, stamp)
	num := FileIssue(t, env, repo, title,
		fmt.Sprintf("e2e review_authority gate/settle test. marker=%s", marker), extraLabels...)
	itemID = AddIssueToProject(t, env, repo, num)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	path := fmt.Sprintf("e2e/review-authority/%s-%d.md", marker, num)
	content := fmt.Sprintf("# e2e review-authority marker\n\nmarker=%s\n", marker)
	prNum = CreateMemberPR(t, env, repo, baseBranch, branch, path, content, title, num)
	// Confirm the PR is resolvable by the fabrik/issue-<N> branch convention
	// (mirrors the engine's resolver) before seeding the completion label.
	LinkedPRNumber(t, env, repo, num)

	AddLabel(t, env, repo, num, "stage:"+column+":complete")
	SetIssueStatus(t, env, itemID, column)
	t.Logf("seeded review-gate item: issue #%d, PR #%d, stage:%s:complete, Status=%s (marker=%s)",
		num, prNum, column, column, marker)
	return num, prNum, itemID
}
