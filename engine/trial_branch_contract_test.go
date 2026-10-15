package engine

import (
	"fmt"
	"reflect"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/pruefer"
)

// TestTrialBranchContract_PrueferRecognisesEngineTrialBranches (#2230) pins the
// wire format between the two components. Pruefer keeps its own copy of the
// trial branch grammar and of the `Closes #N` member lines (it does not import
// engine); this builds branch names with the engine's own namer and prefix and
// requires Pruefer to recognise them, and renders an integration PR body with
// the engine's own builder and requires Pruefer to recover the members. A
// naming or body change here fails CI instead of silently disabling the skip.
func TestTrialBranchContract_PrueferRecognisesEngineTrialBranches(t *testing.T) {
	for _, base := range []string{"main", "release/1.x", "release-2026-10"} {
		namer := &trialNamer{base: fmt.Sprintf("merge-train-%s-%d", sanitizeBranchName(base), 1760000000)}
		for i := 0; i < 3; i++ { // base name, then -t1, -t2
			name := namer.next()
			branch := trainBranchPrefix + name
			if !trialBelongsToBase(branch, base) {
				t.Fatalf("engine does not consider %q a trial of %q — test premise broken", branch, base)
			}
			if !pruefer.IsTrialBranch(branch) {
				t.Errorf("pruefer does not recognise the engine trial branch %q", branch)
			}
		}
	}
	for _, notTrial := range []string{"fabrik/issue-12", "main", trainBranchPrefix + "not-a-trial"} {
		if pruefer.IsTrialBranch(notTrial) {
			t.Errorf("pruefer recognises %q as a trial branch", notTrial)
		}
	}
}

func TestTrialBranchContract_PrueferReadsEngineMemberLines(t *testing.T) {
	survivors := []trainMember{
		{item: gh.ProjectItem{Number: 11, Title: "first"}},
		{item: gh.ProjectItem{Number: 12, Title: "second"}},
	}
	body := buildIntegrationPRBody(survivors)
	if got := pruefer.TrialMemberIssues(body); !reflect.DeepEqual(got, []int{11, 12}) {
		t.Errorf("pruefer read members %v from the engine's integration PR body %q, want [11 12]", got, body)
	}
}
