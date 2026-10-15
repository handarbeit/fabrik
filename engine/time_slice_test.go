package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/claudeerr"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Tests for the time-slice exemption (revised FR-001) and the longest-waiting-
// first dispatch order (FR-006) of #2232 / ADR 2232.

func TestIsTimeSlice(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		turnLimited bool
		want        bool
	}{
		{name: "turn cap", err: &claudeerr.TurnLimitError{}, turnLimited: true, want: true},
		{name: "max_wall_time kill", err: &claudeerr.KilledError{Cause: claudeerr.KillWallTime, Err: errors.New("killed")}, want: true},
		{name: "wrapped wall-time kill", err: fmt.Errorf("outer: %w", &claudeerr.KilledError{Cause: claudeerr.KillWallTime, Err: errors.New("killed")}), want: true},
		{name: "inactivity kill is a hang, not a slice", err: &claudeerr.KilledError{Cause: claudeerr.KillInactivity, Err: errors.New("killed")}, want: false},
		{name: "fast error exit", err: errors.New("claude exited with error: exit status 1"), want: false},
		{name: "clean exit without marker", err: nil, want: false},
	}
	for _, tc := range cases {
		if got := isTimeSlice(tc.err, tc.turnLimited); got != tc.want {
			t.Errorf("%s: isTimeSlice = %v, want %v", tc.name, got, tc.want)
		}
	}
	defer SetTimeSliceBackoffDisabledForTest(true)()
	if isTimeSlice(&claudeerr.TurnLimitError{}, true) {
		t.Error("the test seam must neutralise the exemption")
	}
}

// A time slice records an already-due cooldown (admitted on the very next poll);
// any other incomplete exit records now + backoff.
func TestRecordPostRunRetryCooldown_ZeroIsAlreadyDue(t *testing.T) {
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	item := gh.ProjectItem{Number: 91, Title: "x", Status: "Research"}
	eng.store.Apply(itemstate.IssueOpened{Item: item})
	eng.recordPostRunRetryCooldown(item, 0)
	snap, _ := eng.store.Peek(cooldownTestRepo, 91)
	if got := snap.CooldownAt("retry-backoff"); !got.Equal(start) {
		t.Fatalf("Until = %v, want the current instant %v", got, start)
	}
	if !snap.HasExpiredCooldown(start) {
		t.Fatal("an already-due cooldown must read as expired so the next poll admits the item")
	}
}

func TestStageAttemptedSliceMarksBackoffExempt(t *testing.T) {
	eng, _ := cooldownAdmissionEngine(t, &mockGitHubClient{})
	item := gh.ProjectItem{Number: 92, Title: "x", Status: "Research"}
	eng.store.Apply(itemstate.IssueOpened{Item: item})
	eng.store.Apply(itemstate.StageAttempted{Repo: cooldownTestRepo, Number: 92, StageName: "Research", At: time.Now(), Slice: true})
	if snap, _ := eng.store.Peek(cooldownTestRepo, 92); !snap.BackoffExempt("Research") {
		t.Fatal("a slice must mark the stage backoff-exempt")
	}
	// The next, non-slice attempt clears the exemption.
	eng.store.Apply(itemstate.StageAttempted{Repo: cooldownTestRepo, Number: 92, StageName: "Research", At: time.Now()})
	if snap, _ := eng.store.Peek(cooldownTestRepo, 92); snap.BackoffExempt("Research") {
		t.Fatal("a later non-slice attempt must restore the backoff")
	}
}

func TestOrderDispatchCandidates(t *testing.T) {
	eng, start := cooldownAdmissionEngine(t, &mockGitHubClient{})
	mk := func(n int) gh.ProjectItem {
		return gh.ProjectItem{Number: n, Title: "x", Status: "Research"}
	}
	items := []gh.ProjectItem{mk(1), mk(2), mk(3), mk(4)}
	for _, it := range items {
		eng.store.Apply(itemstate.IssueOpened{Item: it})
	}
	// #1 just ran, #3 ran long ago, #2 and #4 never ran.
	eng.store.Apply(itemstate.StageAttempted{Repo: cooldownTestRepo, Number: 1, StageName: "Research", At: start})
	eng.store.Apply(itemstate.StageAttempted{Repo: cooldownTestRepo, Number: 3, StageName: "Research", At: start.Add(-time.Hour)})

	got := eng.orderDispatchCandidates(items)
	want := []int{2, 4, 3, 1}
	for i, n := range want {
		if got[i].Number != n {
			t.Fatalf("order = %v, want %v", dispatchNumbers(got), want)
		}
	}
	if items[0].Number != 1 || items[3].Number != 4 {
		t.Fatal("the input slice must not be reordered")
	}
	defer SetDispatchOrderDisabledForTest(true)()
	if got := eng.orderDispatchCandidates(items); got[0].Number != 1 {
		t.Fatalf("with the seam disabled the board order must be kept, got %v", dispatchNumbers(got))
	}
}

func dispatchNumbers(items []gh.ProjectItem) []int {
	out := make([]int, len(items))
	for i, it := range items {
		out[i] = it.Number
	}
	return out
}
