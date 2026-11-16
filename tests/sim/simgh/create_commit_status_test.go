package simgh

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// CreateCommitStatus keeps only the latest status per context, as GitHub's
// combined-status endpoint does (#2471).
func TestCreateCommitStatusReplacesSameContext(t *testing.T) {
	s, _ := seedBasicBoard(t)
	sha := seedPRForState(t, s, false)

	if err := s.CreateCommitStatus("acme", "widgets", sha, "pending", gh.ReviewStatusContext, "waiting"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCommitStatus("acme", "widgets", sha, "success", "other/ctx", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCommitStatus("acme", "widgets", sha, "success", gh.ReviewStatusContext, "clean"); err != nil {
		t.Fatal(err)
	}
	got, err := s.FetchCombinedStatus("acme", "widgets", sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("statuses = %+v, want 2 contexts", got)
	}
	for _, st := range got {
		if st.Context == gh.ReviewStatusContext && st.State != "success" {
			t.Errorf("fabrik/review = %q, want success (latest wins)", st.State)
		}
	}
}
