package engine

import (
	"errors"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

func TestClearOrphanAwaitingInput(t *testing.T) {
	cases := []struct {
		name       string
		live       []string
		fetchErr   error
		removeErr  error
		disabled   bool
		wantRemove bool
	}{
		{name: "bare label is stripped", live: []string{"fabrik:awaiting-input"}, wantRemove: true},
		{name: "pair is a real pause and is kept", live: []string{"fabrik:paused", "fabrik:awaiting-input"}},
		{name: "no label is a no-op", live: []string{"stage:Plan:complete"}},
		{name: "fetch error leaves it alone", fetchErr: errors.New("boom")},
		{name: "ErrNotFound on removal is success", live: []string{"fabrik:awaiting-input"}, removeErr: gh.ErrNotFound, wantRemove: true},
		{name: "seam disabled", live: []string{"fabrik:awaiting-input"}, disabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockGitHubClient{
				fetchLabelsFn: func(owner, repo string, n int) ([]string, error) { return tc.live, tc.fetchErr },
			}
			if tc.removeErr != nil {
				client.removeLabelFromIssueFn = func(owner, repo string, n int, l string) error { return tc.removeErr }
			}
			eng := testEngine(t, client, &mockClaudeInvoker{})
			eng.SetOrphanAwaitingInputClearDisabledForTest(tc.disabled)

			// The snapshot deliberately disagrees with the live labels: the
			// decision must read the live set.
			eng.clearOrphanAwaitingInput(gh.ProjectItem{Number: 7, Labels: []string{"fabrik:paused", "fabrik:awaiting-input"}})

			removed := len(client.removeLabelCalls) > 0
			if removed != tc.wantRemove {
				t.Fatalf("removed = %v, want %v (calls=%v)", removed, tc.wantRemove, client.removeLabelCalls)
			}
			if removed && client.removeLabelCalls[0].labelName != "fabrik:awaiting-input" {
				t.Errorf("removed %q, want fabrik:awaiting-input", client.removeLabelCalls[0].labelName)
			}
		})
	}
}
