//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// TestSmokeSingleRepo files a trivial issue against fabrik-test-alpha, places
// it at Status=Specify with yolo, and asserts Fabrik dispatches a worker
// within a reasonable window. It does NOT wait for the full pipeline to
// complete — that would take 30+ minutes. The point is to verify the test bed
// is alive and the dispatch path works.
//
// This is the minimal proof-of-life scenario. Always include it in any
// release-validation run.
func TestSmokeSingleRepo(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	issueNum := FileIssue(t, env, env.RepoAlpha,
		"e2e smoke: minimal single-repo dispatch",
		"## Goal\n\nVerify Fabrik dispatches a worker on a trivial issue.\n\n"+
			"## Trivial change\n\nUpdate `README.md` to add a single line at the end:\n\n"+
			"`<!-- e2e smoke test ran at "+time.Now().UTC().Format(time.RFC3339)+" -->`\n\n"+
			"That's the entire scope. Implement, open PR, merge. This is purely a "+
			"smoke test for the e2e harness — it is not testing any specific feature.",
		"fabrik:yolo",
	)

	itemID := AddIssueToProject(t, env, env.RepoAlpha, issueNum)
	SetIssueStatus(t, env, itemID, "Specify")

	t.Logf("filed %s#%d at Status=Specify", env.RepoAlpha, issueNum)

	// Within 3 minutes Fabrik should have advanced past Specify into Research.
	// (Specify on a trivial issue typically takes ~30-90 seconds.)
	WaitForIssueLabel(t, env, env.RepoAlpha, issueNum, "stage:Specify:complete", 3*time.Minute)
	t.Logf("Specify complete on %s#%d — dispatch path verified", env.RepoAlpha, issueNum)

	// We stop here. The full pipeline would take 30+ minutes and cost ~$1-2;
	// not what a smoke test is for. If you want full-pipeline coverage, use
	// TestCrossRepoSpawn (when it exists post-#803-fix).
}
