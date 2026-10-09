package simgh

import "testing"

// A PR outlives its head branch on real GitHub: deleting the branch closes an
// open PR, which keeps listing with its last head SHA. The engine deletes
// merge-train trial branches with its own `git push --delete`, so the model
// must not turn a vanished head into a ListPRs error that breaks every later
// read in the repo.
//
// Neutralisation: make resolvePRHead return the (empty) live SHA when the
// branch is gone, or drop stateWithHeadGone, and this test fails on the SHA or
// state assertion.
func TestListPRsSurvivesDeletedHeadBranch(t *testing.T) {
	s, _ := seedBasicBoard(t)
	seedCleanDivergence(t, s)
	s.SeedPR(repoName, PRSeed{Number: 42, Head: headBranch, Base: "main", Title: "to be orphaned"})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding PR: %v", err)
	}

	before, err := s.ListPRs("acme", "widgets")
	if err != nil {
		t.Fatalf("ListPRs before delete: %v", err)
	}
	if len(before) != 1 || before[0].HeadSHA == "" || before[0].State != "open" {
		t.Fatalf("PR before delete = %+v, want one open PR with a head SHA", before)
	}
	lastSHA := before[0].HeadSHA

	r, err := s.repoByKey(repoKey("acme", "widgets"))
	if err != nil {
		t.Fatalf("repoByKey: %v", err)
	}
	r.gitMu.Lock()
	_, err = runGit(r.bareDir, "update-ref", "-d", "refs/heads/"+headBranch)
	r.gitMu.Unlock()
	if err != nil {
		t.Fatalf("deleting head branch: %v", err)
	}

	after, err := s.ListPRs("acme", "widgets")
	if err != nil {
		t.Fatalf("ListPRs after head branch delete: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("ListPRs after delete = %+v, want the PR still listed", after)
	}
	if after[0].HeadSHA != lastSHA {
		t.Errorf("HeadSHA = %q, want the last-known %q", after[0].HeadSHA, lastSHA)
	}
	if after[0].State != "closed" || after[0].Merged {
		t.Errorf("State = %q merged=%v, want closed and not merged", after[0].State, after[0].Merged)
	}
}
