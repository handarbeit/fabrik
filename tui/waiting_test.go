package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func waitingModel(t *testing.T) Model {
	t.Helper()
	redirectHistory(t)
	m := New(30, ProjectInfo{}, "", nil, nil, 0, false)
	m.width = 140
	m.active.now = time.Now()
	return m
}

func send(m Model, ev Event) Model {
	next, _ := m.Update(ev)
	return next.(Model)
}

func TestWaitingRows_AwaitingCINamesThePR(t *testing.T) {
	m := waitingModel(t)
	m = send(m, WaitingItemsEvent{
		Items:     []WaitingItem{{Number: 2232, Title: "retry backoff", Stage: "Validate", Kind: WaitingExternal, Text: "CI on PR #2236 (5/7 checks, since 13:52)"}},
		WaitCount: 1,
	})
	view := m.active.View(m.width)
	for _, want := range []string{"⏳", "#2232", "Validate", "waiting for: CI on PR #2236 (5/7 checks, since 13:52)", "retry backoff"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
}

func TestWaitingRows_HumanRowUsesHandMarker(t *testing.T) {
	m := waitingModel(t)
	m = send(m, WaitingItemsEvent{
		Items:      []WaitingItem{{Number: 7, Stage: "Implement", Kind: WaitingHuman, Text: "human reply (paused)"}},
		HumanCount: 1,
	})
	view := m.active.View(m.width)
	if !strings.Contains(view, "✋#7") || strings.Contains(view, "⏳") {
		t.Errorf("view = %q", view)
	}
}

func TestWaitingRows_EachR1ExampleRenders(t *testing.T) {
	texts := []string{
		"CI on PR #2236 (5/7 checks, since 13:52)",
		"review from handarbeit-pruefer (since 13:50)",
		"retry at 14:03 (time slice)",
		"rebase (fabrik:rebase-needed)",
		"landing in merge train (Queued, position 2 of 3)",
		"human reply (paused)",
	}
	var items []WaitingItem
	for i, tx := range texts {
		kind := WaitingExternal
		if strings.HasPrefix(tx, "human") {
			kind = WaitingHuman
		}
		items = append(items, WaitingItem{Number: 10 + i, Stage: "Validate", Kind: kind, Text: tx})
	}
	m := waitingModel(t)
	m = send(m, WaitingItemsEvent{Items: items})
	view := m.active.View(m.width)
	for _, tx := range texts {
		if !strings.Contains(view, "waiting for: "+tx) {
			t.Errorf("view missing row for %q", tx)
		}
	}
}

func TestWaitingRows_OmittedLineAndHeight(t *testing.T) {
	m := waitingModel(t)
	items := make([]WaitingItem, 5)
	for i := range items {
		items[i] = WaitingItem{Number: i + 1, Stage: "Validate", Text: "CI"}
	}
	before := m.active.Height()
	m = send(m, WaitingItemsEvent{Items: items, Omitted: 3, WaitCount: 8})
	view := m.active.View(m.width)
	if !strings.Contains(view, "+3 more waiting") {
		t.Errorf("view missing +K more line:\n%s", view)
	}
	// 5 rows + the +K line + title + borders, over the empty pane's 4.
	if got, want := m.active.Height(), 5+1+1+2; got != want {
		t.Errorf("Height() = %d, want %d (was %d empty)", got, want, before)
	}
	if lines := strings.Count(view, "\n") + 1; lines > m.active.Height() {
		t.Errorf("rendered %d lines in a pane of height %d", lines, m.active.Height())
	}
	if !strings.Contains(view, "In Progress (8)") {
		t.Errorf("title should count the visible rows plus the omitted ones, matching the header:\n%s", view)
	}
}

func TestWaitingRows_SuppressedWhenAWorkerIsRunning(t *testing.T) {
	m := waitingModel(t)
	m = send(m, WaitingItemsEvent{Items: []WaitingItem{{Number: 5, Stage: "Validate", Text: "CI"}}})
	m = send(m, JobStartedEvent{IssueNumber: 5, Title: "t", StageName: "Validate", StartedAt: time.Now()})
	if rows := m.active.waitingRows(); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none while a worker runs", rows)
	}
	if strings.Contains(m.active.View(m.width), "waiting for:") {
		t.Error("waiting row rendered beside the running row")
	}
}

func TestWaitingRows_SnapshotReplacesAndClears(t *testing.T) {
	m := waitingModel(t)
	m = send(m, WaitingItemsEvent{Items: []WaitingItem{{Number: 1, Stage: "Validate", Text: "CI"}}})
	m = send(m, WaitingItemsEvent{})
	if len(m.active.waitingRows()) != 0 || m.active.Height() != 4 {
		t.Errorf("rows=%d height=%d after empty snapshot", len(m.active.waitingRows()), m.active.Height())
	}
}

func TestWaitingRows_NarrowWidthNeverOverflows(t *testing.T) {
	m := waitingModel(t)
	m = send(m, WaitingItemsEvent{Items: []WaitingItem{
		{Number: 2232, Title: strings.Repeat("long title ", 10), Stage: "Validate", Kind: WaitingExternal, Text: "CI on PR #2236 (5/7 checks, since 13:52)"},
		{Number: 2233, Stage: "Validate", Kind: WaitingHuman, Text: strings.Repeat("x", 100)},
	}})
	for _, w := range []int{30, 50, 80} {
		for _, line := range strings.Split(m.active.View(w), "\n") {
			if got := lipgloss.Width(line); got > w {
				t.Errorf("width %d: line is %d cells: %q", w, got, line)
			}
		}
	}
}

func TestHeader_NothingToDoBecomesWaitCount(t *testing.T) {
	cases := []struct {
		name         string
		wait, human  int
		status, want string
	}{
		{"waits", 2, 0, HeaderNothingToDo, "waiting on 2 item(s)"},
		{"waits and human", 1, 1, HeaderNothingToDo, "waiting on 2 item(s), 1 need you"},
		{"only human", 0, 1, HeaderNothingToDo, "waiting on 1 item(s), 1 need you"},
		{"no waits stays idle", 0, 0, HeaderNothingToDo, HeaderNothingToDo},
		{"other status untouched", 3, 0, "[poll] dispatched 1 item(s)", "[poll] dispatched 1 item(s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := waitingModel(t)
			m = send(m, WaitingItemsEvent{WaitCount: c.wait, HumanCount: c.human})
			m = send(m, LogEvent{Tag: "poll", Message: strings.TrimPrefix(c.status, "[poll] ") + "\n"})
			view := m.header.View(m.width)
			if !strings.Contains(view, c.want) {
				t.Errorf("header = %q, want %q", view, c.want)
			}
			if c.want != HeaderNothingToDo && strings.Contains(view, HeaderNothingToDo) {
				t.Errorf("header still reads idle: %q", view)
			}
		})
	}
}

func TestHeader_WaitCountSurvivesLaterPollLogLines(t *testing.T) {
	m := waitingModel(t)
	m = send(m, LogEvent{Tag: "poll", Message: "found 3 items on board\n"})
	m = send(m, WaitingItemsEvent{WaitCount: 2})
	m = send(m, LogEvent{Tag: "poll", Message: "nothing to do\n"})
	if v := m.header.View(m.width); !strings.Contains(v, "waiting on 2 item(s)") {
		t.Errorf("header = %q", v)
	}
	// The count is cleared by the next snapshot, not by a log line.
	m = send(m, WaitingItemsEvent{})
	if v := m.header.View(m.width); !strings.Contains(v, "nothing to do") {
		t.Errorf("header = %q", v)
	}
}
