//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSmokeSingleRepoDispatch is the minimal proof-of-life test: file an issue,
// verify a worker dispatches and Specify completes. Does NOT wait for the full
// pipeline. Always include this in any release-validation run; it's cheap and
// catches general pipeline breakage.
//
// Wall-clock: ~3-5 min. Cost: ~$0.10-0.20.
func TestSmokeSingleRepoDispatch(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e smoke dispatch (%s)", time.Now().UTC().Format("15:04:05")),
		`## Goal

Verify Fabrik dispatches a worker on a trivial issue.

## Trivial change

This is a dispatch smoke test. The test framework will close this issue before any actual work happens. Specify should just acknowledge the issue and signal complete; no Research/Plan/Implement is expected.

If you (the Specify agent) are reading this, the simplest spec is: "This is a smoke-test issue. No implementation required. Emit FABRIK_NO_WORK_NEEDED to short-circuit." That keeps cost minimal.`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify", env.RepoAlpha, num)

	// Within 5 minutes Fabrik should have advanced past Specify.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Specify:complete", 5*time.Minute)
	t.Logf("Specify complete on %s#%d — dispatch path verified", env.RepoAlpha, num)

	// Don't wait for full pipeline; the t.Cleanup from FileIssue will close it.
}

// TestSpecKitSpecWritten verifies that when the Specify stage completes,
// a spec file exists at specs/<N>-<slug>/spec.md on the issue branch, contains
// a "# Feature Specification:" H1, and includes all mandatory Spec Kit sections.
// It also verifies that "## Open Questions" is stripped before the file is committed.
//
// Wall-clock: ~5-10 min. Cost: ~$0.15-0.30.
func TestSpecKitSpecWritten(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("15:04:05")
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e spec-kit spec file verify (%s)", stamp),
		`feat: add a hello world utility function

Write a simple Go helper function that returns "hello world".
This is a minimal e2e verification issue for the Spec Kit spec-file commit feature.

Scope: Single file change, no cross-repo work needed.`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify", env.RepoAlpha, num)

	// Wait up to 10 minutes for Specify to complete.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "stage:Specify:complete", 10*time.Minute)
	t.Logf("Specify complete on %s#%d — verifying spec file on branch", env.RepoAlpha, num)

	branchRef := fmt.Sprintf("fabrik/issue-%d", num)

	// List specs/ directory on the issue branch.
	specsOut, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/contents/specs", env.RepoAlpha),
		"-f", fmt.Sprintf("ref=%s", branchRef),
		"--jq", "[.[] | .name]")
	if err != nil {
		t.Fatalf("specs/ directory not found on branch %s: %v\n%s", branchRef, err, specsOut)
	}
	var dirs []string
	if parseErr := json.Unmarshal([]byte(strings.TrimSpace(specsOut)), &dirs); parseErr != nil {
		t.Fatalf("parse specs dir listing: %v\n%s", parseErr, specsOut)
	}

	prefix := fmt.Sprintf("%d-", num)
	var specDir string
	for _, d := range dirs {
		if strings.HasPrefix(d, prefix) {
			specDir = d
			break
		}
	}
	if specDir == "" {
		t.Fatalf("no specs/%d-* directory on branch %s (dirs: %v)", num, branchRef, dirs)
	}

	// Fetch the spec file content, decoding the GitHub base64 encoding.
	specPath := fmt.Sprintf("specs/%s/spec.md", specDir)
	specContent, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/contents/%s", env.RepoAlpha, specPath),
		"-f", fmt.Sprintf("ref=%s", branchRef),
		"--jq", `.content | gsub("\n"; "") | @base64d`)
	if err != nil {
		t.Fatalf("read %s on branch %s: %v\n%s", specPath, branchRef, err, specContent)
	}

	// Verify mandatory Spec Kit sections are present.
	for _, section := range []string{
		"# Feature Specification:",
		"## User Scenarios & Testing",
		"## Requirements",
		"## Success Criteria",
	} {
		if !strings.Contains(specContent, section) {
			t.Errorf("spec file %s missing required section %q", specPath, section)
		}
	}

	// Verify Open Questions is not committed to the spec file.
	if strings.Contains(specContent, "## Open Questions") {
		t.Errorf("spec file %s must not contain '## Open Questions'", specPath)
	}

	t.Logf("spec file %s verified on branch %s", specPath, branchRef)
}

// TestSmokeSingleRepoFullPipeline runs the full single-repo end-to-end flow:
// file an issue describing a trivial code change, expect Fabrik to take it
// from Specify all the way to Done with a merged PR.
//
// This is the "is the entire pipeline working end-to-end" check, complementary
// to TestCrossRepoSpawn (which exercises the cross-repo path). Useful when
// debugging "Implement onwards is broken" classes of issue.
//
// Wall-clock: ~20-40 min. Cost: ~$0.50-1.50.
func TestSmokeSingleRepoFullPipeline(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	stamp := time.Now().UTC().Format("20060102-150405")
	marker := fmt.Sprintf("smoke-full-pipeline-%s", stamp)
	num := FileIssue(t, env, env.RepoAlpha,
		fmt.Sprintf("e2e smoke full-pipeline (%s)", stamp),
		`## Goal

End-to-end single-repo pipeline smoke. Verify Fabrik can take an issue from Specify all the way to Done with a merged PR.

## Trivial change

Append a single comment line to `+"`README.md`"+` at the very end of the file:

`+"```"+`
<!-- `+marker+` -->
`+"```"+`

That's the entire change. One file, one line.

## Scope

Single repo only — no cross-repo work. The Plan stage should NOT decompose.`,
		"fabrik:yolo",
	)
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)
	SetIssueStatus(t, env, itemID, "Specify")
	t.Logf("filed %s#%d at Status=Specify, marker=%s", env.RepoAlpha, num, marker)

	// Full pipeline can take up to ~30 min depending on Claude latency.
	WaitForIssueClosed(t, env, env.RepoAlpha, num, 45*time.Minute)
	t.Logf("%s#%d closed — full single-repo pipeline verified", env.RepoAlpha, num)
}
