package engine

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// TestSettle_MergeObs pins the typed observation (#2302 R1): only the two
// unknown returns are MergeObsUnknown; a read error, "CI checks pending" and
// every other computed return never are.
func TestSettle_MergeObs(t *testing.T) {
	failed := gh.CheckRun{Name: "ci", Status: "completed", Conclusion: "failure"}
	pending := gh.CheckRun{Name: "ci", Status: "in_progress"}
	cases := []struct {
		name      string
		mergeable *bool
		state     string
		fieldsErr error
		runs      []gh.CheckRun
		wantObs   MergeObservation
		wantValue string
		wantReas  string
	}{
		{name: "mergeable null", mergeable: nil, state: "", wantObs: MergeObsUnknown, wantValue: "mergeable=null"},
		{name: "mergeable null with state", mergeable: nil, state: "unknown", wantObs: MergeObsUnknown, wantValue: "mergeable=null"},
		{name: "state unknown", mergeable: boolPtr(true), state: "unknown", wantObs: MergeObsUnknown, wantValue: "mergeable_state=unknown"},
		{name: "read error", fieldsErr: errors.New("boom"), wantObs: MergeObsNone},
		{name: "clean", mergeable: boolPtr(true), state: "clean", wantObs: MergeObsComputed},
		{name: "dirty", mergeable: boolPtr(false), state: "dirty", wantObs: MergeObsComputed},
		{name: "ci pending", mergeable: boolPtr(true), state: "blocked", runs: []gh.CheckRun{pending}, wantObs: MergeObsComputed, wantReas: "CI checks pending"},
		{name: "ci failed", mergeable: boolPtr(true), state: "blocked", runs: []gh.CheckRun{failed}, wantObs: MergeObsComputed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) {
					return &gh.PRDetails{Number: 5, State: "open", HeadSHA: "sha1"}, nil
				},
				fetchPRMergeableFieldsFn: func(owner, repo string, n int) (*bool, string, error) {
					return tc.mergeable, tc.state, tc.fieldsErr
				},
				fetchCheckRunsFn: func(owner, repo, sha string) ([]gh.CheckRun, error) { return tc.runs, nil },
			}
			eng := testEngineForMerge(t, client)
			r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"})
			if r.MergeObs != tc.wantObs {
				t.Fatalf("MergeObs = %v, want %v (reason %q)", r.MergeObs, tc.wantObs, r.Reason)
			}
			if r.MergeUnknownValue != tc.wantValue {
				t.Errorf("MergeUnknownValue = %q, want %q", r.MergeUnknownValue, tc.wantValue)
			}
			if tc.wantReas != "" && r.Reason != tc.wantReas {
				t.Errorf("Reason = %q, want %q", r.Reason, tc.wantReas)
			}
		})
	}
}

func TestSettle_MergeObs_NoPRAndTerminalAreNone(t *testing.T) {
	client := &mockGitHubClient{
		fetchLinkedPRFn: func(owner, repo string, n int) (*gh.PRDetails, error) { return nil, nil },
	}
	eng := testEngineForMerge(t, client)
	if r := eng.settlePRMergeState(settleItem(1), &stages.Stage{Name: "Validate"}); r.MergeObs != MergeObsNone {
		t.Errorf("no PR: MergeObs = %v, want none", r.MergeObs)
	}
}
