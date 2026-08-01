//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// review_authority e2e helpers (ADR-1250, issue #1258). Authoritative-mode
// coverage is applied per issue via the "review-authority:authoritative"
// label (engine support: #1261, decoupled from this issue) rather than a
// bed-local stage/column variant — review_authority is a property of a
// stage's config, not a distinct kind of stage, so it does not belong on
// the board as a column name. Scenarios opt in per item by passing the
// label as an extraLabel to seedReviewGateItem, running against the bed's
// existing Review column with its default (advisory) stage config. See
// adrs/1258-e2e-review-authority-coverage.md for the full rationale,
// including why the earlier column/stage-variant design was rejected.
//
// Bed prerequisite: the "review-authority:authoritative" label must already
// exist in the target repo. FileIssue passes extraLabels straight through to
// `gh issue create --label`, which fails hard (not gracefully) if the label
// is absent — see tests/e2e/README.md's prerequisite for this test file,
// which mirrors the existing "Gate labels seeded" precedent for
// TestPausedMergedPRRecovery.

// seedReviewGateItem files an issue, adds it to the project, builds a member
// PR directly via the GitHub API (CreateMemberPR — no Claude cost), seeds
// stage:<column>:complete, and places the item at column. This exercises the
// catch-up loop's gate/settle logic (checkReviewGate via handleReviewGate)
// without ever invoking Claude for the gated stage — the same zero-cost
// construction precedent as TestPausedMergedPRRecovery's R5. extraLabels are
// applied at file time (e.g. "fabrik:yolo" for the composition scenario).
//
// The member PR is opened ready (non-draft), so the bed's claude-review.yml
// bot will typically review it within ~60-100s. Scenarios that submit their
// own deliberate verdict must not treat the bot's incidental review as proof
// the gate engaged — see the file-level "Determinism" note below and #1312.
func seedReviewGateItem(t *testing.T, env *Env, repo, baseBranch, column, marker string, extraLabels ...string) (issueNum, prNum int, itemID string) {
	t.Helper()
	return seedReviewGateItemImpl(t, env, repo, baseBranch, column, marker, false, extraLabels...)
}

// seedReviewGateItemDraft is seedReviewGateItem, but opens the member PR as a
// draft (via CreateMemberPRDraft) so the bed's claude-review.yml bot never
// reviews it — its job is guarded by
// `if: github.event.pull_request.draft == false` and only triggers on
// opened/ready_for_review. Use this when the property under test is "nothing
// has reviewed this PR" (e.g. expected_reviewers's declared-but-unrequested
// and undeclared-nothing-requested scenarios): with a real (non-draft) PR, an
// incidental bot review can satisfy checkReviewGate's
// `len(outstanding) == 0 && hasReviews` clearing branch before the scenario's
// own assertions run, independent of any reviewer the test explicitly
// requested or declared. Never mark the returned PR ready during such a
// scenario, or the bot becomes eligible to review it again. See #1312.
func seedReviewGateItemDraft(t *testing.T, env *Env, repo, baseBranch, column, marker string, extraLabels ...string) (issueNum, prNum int, itemID string) {
	t.Helper()
	return seedReviewGateItemImpl(t, env, repo, baseBranch, column, marker, true, extraLabels...)
}

func seedReviewGateItemImpl(t *testing.T, env *Env, repo, baseBranch, column, marker string, draft bool, extraLabels ...string) (issueNum, prNum int, itemID string) {
	t.Helper()
	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e review-authority %s (%s)", marker, stamp)
	num := FileIssue(t, env, repo, title,
		fmt.Sprintf("e2e review_authority gate/settle test. marker=%s", marker), extraLabels...)
	itemID = AddIssueToProject(t, env, repo, num)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	path := fmt.Sprintf("e2e/review-authority/%s-%d.md", marker, num)
	content := fmt.Sprintf("# e2e review-authority marker\n\nmarker=%s\n", marker)
	if draft {
		prNum = CreateMemberPRDraft(t, env, repo, baseBranch, branch, path, content, title, num)
	} else {
		prNum = CreateMemberPR(t, env, repo, baseBranch, branch, path, content, title, num)
	}
	// Confirm the PR is resolvable by the fabrik/issue-<N> branch convention
	// (mirrors the engine's resolver) before seeding the completion label.
	LinkedPRNumber(t, env, repo, num)

	AddLabel(t, env, repo, num, "stage:"+column+":complete")
	SetIssueStatus(t, env, itemID, column)
	t.Logf("seeded review-gate item: issue #%d, PR #%d, stage:%s:complete, Status=%s (marker=%s, draft=%v)",
		num, prNum, column, column, marker, draft)
	return num, prNum, itemID
}
