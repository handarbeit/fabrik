package sim

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tests/sim/simgh"
)

// See README.md's vocabulary-mapping table (R5) for where these diverge from
// tests/e2e/harness.go's names, parameter order, and semantics — this file
// otherwise mirrors that file's vocabulary deliberately, so a scenario reads
// the same in both beds.

// FileIssue creates an issue in env's managed repo, places it on the project
// board at the given status column, and returns its issue number.
//
// Divergence from tests/e2e.FileIssue (R5): the live harness's FileIssue only
// creates the issue — AddIssueToProject and SetIssueStatus are separate
// calls, because that's three real GitHub API round-trips. simgh's SeedIssue
// does the equivalent of all three in one seeding call, so this signature
// folds status in directly rather than mirroring the three-call split; see
// README.md's table entry for this function.
func FileIssue(t *testing.T, env *Env, title, body, status string, labels ...string) int {
	t.Helper()
	env.issueSeqMu.Lock()
	env.issueSeqNext++
	num := env.issueSeqNext
	env.issueSeqMu.Unlock()

	env.Sim.Sim().SeedIssue(env.OwnerRepo, simgh.IssueSeed{
		Number: num,
		Title:  title,
		Body:   body,
		Status: status,
		Labels: append([]string(nil), labels...),
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("FileIssue: %v", err)
	}
	return num
}

// projectItem fetches the current board card for issueNumber, failing the
// test if it cannot be found — every assertion helper below needs this, and
// a missing item is always a scenario bug worth stopping on immediately
// rather than reporting a confusing downstream nil-pointer failure.
func projectItem(t *testing.T, env *Env, issueNumber int) gh.ProjectItem {
	t.Helper()
	item, err := env.Sim.FetchProjectItem(env.Owner, env.Repo, issueNumber)
	if err != nil {
		t.Fatalf("fetch #%d: %v", issueNumber, err)
	}
	if item == nil {
		t.Fatalf("#%d is not on the project board", issueNumber)
	}
	return *item
}

// IssueLabels returns issueNumber's current labels.
func IssueLabels(t *testing.T, env *Env, issueNumber int) []string {
	t.Helper()
	return projectItem(t, env, issueNumber).Labels
}

// hasLabel reports whether labels contains name.
func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if l == name {
			return true
		}
	}
	return false
}

// WaitForProjectStatus drives polls (via AdvanceUntil) until issueNumber's
// board column equals status, or maxPolls is exhausted.
func WaitForProjectStatus(t *testing.T, env *Env, issueNumber int, status string, maxPolls int) {
	t.Helper()
	AdvanceUntil(t, env, func(env *Env) bool {
		return projectItem(t, env, issueNumber).Status == status
	}, maxPolls)
}

// WaitForIssueLabel drives polls until issueNumber carries label, or
// maxPolls is exhausted.
func WaitForIssueLabel(t *testing.T, env *Env, issueNumber int, label string, maxPolls int) {
	t.Helper()
	AdvanceUntil(t, env, func(env *Env) bool {
		return hasLabel(projectItem(t, env, issueNumber).Labels, label)
	}, maxPolls)
}

// WaitForLabelAbsent drives polls until issueNumber no longer carries label,
// or maxPolls is exhausted. Returns immediately (no polls) if the label is
// already absent — mirrors WaitForIssueLabel's "already true" short-circuit.
func WaitForLabelAbsent(t *testing.T, env *Env, issueNumber int, label string, maxPolls int) {
	t.Helper()
	AdvanceUntil(t, env, func(env *Env) bool {
		return !hasLabel(projectItem(t, env, issueNumber).Labels, label)
	}, maxPolls)
}

// WaitForIssueClosed drives polls until issueNumber is closed, or maxPolls
// is exhausted. No direct tests/e2e equivalent (the live harness checks this
// inline in scenario bodies); named to match this file's WaitFor* family
// since AC1 requires driving to "a closed issue in the Done column."
func WaitForIssueClosed(t *testing.T, env *Env, issueNumber int, maxPolls int) {
	t.Helper()
	AdvanceUntil(t, env, func(env *Env) bool {
		return projectItem(t, env, issueNumber).IsClosed
	}, maxPolls)
}

