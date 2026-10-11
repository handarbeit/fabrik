package itemstate

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// A change to only OutputTitle counts as a changed check run (upsertCheckRunByID
// compares the whole struct), so it is observable CI progress like the other
// output fields.
func TestUpsertCheckRunByID_OutputTitleChangeIsAChange(t *testing.T) {
	runs := []gh.CheckRun{{ID: 1, Name: "build", Status: "completed", Conclusion: "failure"}}
	got, changed := upsertCheckRunByID(runs, gh.CheckRun{ID: 1, Name: "build", Status: "completed", Conclusion: "failure", OutputTitle: "2 tests failed"})
	if !changed {
		t.Fatal("OutputTitle-only change should report changed")
	}
	if got[0].OutputTitle != "2 tests failed" {
		t.Errorf("OutputTitle = %q", got[0].OutputTitle)
	}
	if _, changed := upsertCheckRunByID(got, got[0]); changed {
		t.Error("identical run should not report changed")
	}
}
