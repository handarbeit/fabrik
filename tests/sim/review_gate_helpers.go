package sim

import (
	"fmt"
	"testing"

	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// review_authority/expected_reviewers sim helpers (ADR-1250, ADR-1283,
// #1258/#1298's issue chain). This file is the structural analogue of
// tests/e2e/review_authority_helpers.go's seedReviewGateItem/
// seedReviewGateItemDraft: it files an issue directly onto a gate-ready
// column with stage:<column>:complete already set, seeds a member PR linked
// to it, and returns both numbers — exercising the catch-up loop's
// checkReviewGate/handleReviewGate logic without ever invoking the scripted
// Claude for the gated stage, at zero Claude-call cost, exactly as the live
// helper does at zero GitHub-API-round-trip cost.
//
// Divergence from the live helper (recorded once here per README.md's
// vocabulary-mapping convention): no repo/baseBranch parameters — env
// already identifies a single managed repo, and every scenario in this
// package that uses this helper targets the repo's default branch. A
// scenario needing a non-default base can seed its own branch/PR directly
// instead of using this helper.

// seedReviewGateItem files an issue, seeds a member PR directly via
// SeedPR/SeedCommitFrom (no Claude cost), pre-applies stage:<column>:complete,
// and places the item at column. extraLabels are applied at file time (e.g.
// "review-authority:authoritative", "fabrik:yolo").
//
// The member PR is opened ready (non-draft). Unlike the live bed, simgh
// never synthesizes an incidental bot review — there is no equivalent of
// #1312's "a real reviewer might review it before the assertion runs" risk
// here, so draft vs non-draft carries no sim-side determinism concern. The
// draft variant (seedReviewGateItemDraft) is kept anyway for structural
// parity with the live helper, so a reader of both beds recognizes the same
// shape.
func seedReviewGateItem(t *testing.T, env *Env, column, marker string, extraLabels ...string) (issueNum, prNum int) {
	t.Helper()
	return seedReviewGateItemImpl(t, env, column, marker, false, extraLabels...)
}

// seedReviewGateItemDraft is seedReviewGateItem, but opens the member PR as
// a draft.
func seedReviewGateItemDraft(t *testing.T, env *Env, column, marker string, extraLabels ...string) (issueNum, prNum int) {
	t.Helper()
	return seedReviewGateItemImpl(t, env, column, marker, true, extraLabels...)
}

func seedReviewGateItemImpl(t *testing.T, env *Env, column, marker string, draft bool, extraLabels ...string) (issueNum, prNum int) {
	t.Helper()

	labels := append([]string{"stage:" + column + ":complete"}, extraLabels...)
	title := fmt.Sprintf("sim review-gate %s", marker)
	num := FileIssue(t, env, title, fmt.Sprintf("sim review_gate test. marker=%s", marker), column, labels...)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	path := fmt.Sprintf("sim/review-gate/%s-%d.md", marker, num)
	content := fmt.Sprintf("# sim review-gate marker\n\nmarker=%s\n", marker)
	env.Sim.Sim().SeedCommitFrom(env.OwnerRepo, branch, "", map[string]string{path: content}, "sim: "+title)
	env.Sim.Sim().SeedPR(env.OwnerRepo, simgh.PRSeed{
		Title:       title,
		Head:        branch,
		Draft:       draft,
		IssueNumber: num,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seedReviewGateItem: %v", err)
	}

	pr, err := env.Sim.FetchLinkedPR(env.Owner, env.Repo, num)
	if err != nil {
		t.Fatalf("seedReviewGateItem: FetchLinkedPR: %v", err)
	}
	if pr == nil || pr.Number == 0 {
		t.Fatalf("seedReviewGateItem: no linked PR for #%d", num)
	}

	t.Logf("seeded review-gate item: issue #%d, PR #%d, stage:%s:complete, Status=%s (marker=%s, draft=%v)",
		num, pr.Number, column, column, marker, draft)
	return num, pr.Number
}
