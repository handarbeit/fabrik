package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

var csT0 = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

func csStopComment(at time.Time) gh.Comment {
	return gh.Comment{Author: "bot", Body: "🏭 **Fabrik — paused by a daemon clean stop**\n\nstage Implement was interrupted.\n\n" + pauseCauseDaemonStopMarker, CreatedAt: at}
}

func csEvidence() cleanStopEvidence {
	return cleanStopEvidence{
		Labels:            []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:pause:daemon-stop"},
		Comments:          []gh.Comment{csStopComment(csT0)},
		PausedLabeledAt:   csT0,
		AwaitingLabeledAt: csT0,
	}
}

func TestCleanStopPauseVerdict(t *testing.T) {
	later := csT0.Add(5 * time.Second)
	cases := []struct {
		name   string
		mutate func(*cleanStopEvidence)
		resume bool
	}{
		{"newest marker, no later events", func(*cleanStopEvidence) {}, true},
		{"tie with the comment resumes", func(ev *cleanStopEvidence) { ev.PausedUnlabeledAt = csT0 }, true},
		{"no cause label (pre-ADR-2487 pause)", func(ev *cleanStopEvidence) {
			ev.Labels = []string{"fabrik:paused", "fabrik:awaiting-input"}
		}, true},
		{"human comment after the marker", func(ev *cleanStopEvidence) {
			ev.Comments = append(ev.Comments, gh.Comment{Author: "human", Body: "any news?", CreatedAt: later})
		}, true},
		{"later fabrik:paused labeled", func(ev *cleanStopEvidence) { ev.PausedLabeledAt = later }, false},
		{"later fabrik:paused unlabeled", func(ev *cleanStopEvidence) { ev.PausedUnlabeledAt = later }, false},
		{"later awaiting-input labeled", func(ev *cleanStopEvidence) { ev.AwaitingLabeledAt = later }, false},
		{"later awaiting-input unlabeled", func(ev *cleanStopEvidence) { ev.AwaitingUnlabeledAt = later }, false},
		{"no paused labeled event", func(ev *cleanStopEvidence) { ev.PausedLabeledAt = time.Time{} }, false},
		{"not paused", func(ev *cleanStopEvidence) { ev.Labels = nil }, false},
		{"other cause label", func(ev *cleanStopEvidence) {
			ev.Labels = []string{"fabrik:paused", "fabrik:pause:ci-timeout"}
		}, false},
		{"no marker", func(ev *cleanStopEvidence) {
			ev.Comments = []gh.Comment{{Body: "🏭 **Fabrik — paused**\n\nstuff", CreatedAt: csT0}}
		}, false},
		{"no comments", func(ev *cleanStopEvidence) { ev.Comments = nil }, false},
		{"newer stage comment", func(ev *cleanStopEvidence) {
			ev.Comments = append(ev.Comments, gh.Comment{Body: "🏭 **Fabrik — stage: Plan**\n\nquestion?", CreatedAt: later})
		}, false},
		{"newer engine pause of another cause", func(ev *cleanStopEvidence) {
			ev.Comments = append(ev.Comments, gh.Comment{Body: "🏭 **Fabrik — paused**\n\nTo resume, remove the fabrik:paused label.", CreatedAt: later})
		}, false},
		{"old marker under a later bare re-pause", func(ev *cleanStopEvidence) {
			ev.PausedUnlabeledAt = later
			ev.PausedLabeledAt = later.Add(time.Second)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := csEvidence()
			tc.mutate(&ev)
			reason, ok := cleanStopPauseVerdict(ev)
			if ok != tc.resume {
				t.Errorf("resume = %v (reason %q), want %v", ok, reason, tc.resume)
			}
			if !ok && reason == "" {
				t.Error("a refusal must carry a reason")
			}
		})
	}
}

// csHarness wires a mock client modelling issues paused by a clean stop.
type csHarness struct {
	mu       sync.Mutex
	removed  []string // "N:label" in order
	comments []string // "N:body"
	order    []string // interleaved "rm:N:label" / "comment:N"
}

func csClient(h *csHarness, ev cleanStopEvidence) *mockGitHubClient {
	return &mockGitHubClient{
		fetchLabelsFn:        func(_, _ string, _ int) ([]string, error) { return ev.Labels, nil },
		fetchIssueCommentsFn: func(_, _ string, _ int) ([]gh.Comment, error) { return ev.Comments, nil },
		fetchLabelAppliedAtFn: func(_, _ string, _ int, l string) (time.Time, error) {
			if l == "fabrik:paused" {
				return ev.PausedLabeledAt, nil
			}
			return ev.AwaitingLabeledAt, nil
		},
		fetchLabelRemovedAtFn: func(_, _ string, _ int, l string) (time.Time, error) {
			if l == "fabrik:paused" {
				return ev.PausedUnlabeledAt, nil
			}
			return ev.AwaitingUnlabeledAt, nil
		},
		removeLabelFromIssueFn: func(_, _ string, n int, l string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.removed = append(h.removed, itemKey(n, l))
			h.order = append(h.order, "rm:"+itemKey(n, l))
			return nil
		},
		addCommentFn: func(_, _ string, n int, body string) (int, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.comments = append(h.comments, itemKey(n, body))
			h.order = append(h.order, "comment:"+itemKey(n, ""))
			return 1, nil
		},
	}
}

func itemKey(n int, s string) string { return fmt.Sprintf("%d:%s", n, s) }

func csBoard(nums ...int) *gh.ProjectBoard {
	b := &gh.ProjectBoard{}
	for _, n := range nums {
		b.Items = append(b.Items, gh.ProjectItem{
			Number: n, Repo: "owner/repo", Status: "Implement",
			Labels: []string{"fabrik:paused", "fabrik:awaiting-input", "fabrik:pause:daemon-stop"},
		})
	}
	return b
}

func csEngine(t *testing.T, client *mockGitHubClient, enabled bool) *Engine {
	t.Helper()
	eng := testEngine(t, client, &mockClaudeInvoker{})
	eng.cfg.ResumeAfterCleanStop = enabled
	return eng
}

func TestResumeCleanStopPauses_LiftsInOrderAndCommentsOnce(t *testing.T) {
	h := &csHarness{}
	eng := csEngine(t, csClient(h, csEvidence()), true)
	board := csBoard(1)
	eng.resumeCleanStopPauses(board)
	eng.resumeCleanStopPauses(board) // a second poll must not resume it again

	want := []string{"rm:1:fabrik:paused", "rm:1:fabrik:awaiting-input", "comment:1:"}
	got := append([]string(nil), h.order...)
	// The cause label removal sits between awaiting-input and the comment; drop it for the order check.
	var filtered []string
	for _, o := range got {
		if strings.Contains(o, "fabrik:pause:daemon-stop") {
			continue
		}
		filtered = append(filtered, o)
	}
	if strings.Join(filtered, ",") != strings.Join(want, ",") {
		t.Fatalf("write order = %v, want %v", filtered, want)
	}
	if len(h.comments) != 1 || !strings.Contains(h.comments[0], cleanStopResumeCommentPrefix) {
		t.Errorf("comments = %v, want one resume comment", h.comments)
	}
}

func TestResumeCleanStopPauses_OptOutAndSeamWriteNothing(t *testing.T) {
	h := &csHarness{}
	eng := csEngine(t, csClient(h, csEvidence()), false)
	eng.resumeCleanStopPauses(csBoard(1))

	eng2 := csEngine(t, csClient(h, csEvidence()), true)
	eng2.SetCleanStopResumeDisabledForTest(true)
	eng2.resumeCleanStopPauses(csBoard(1))

	if len(h.order) != 0 {
		t.Errorf("writes with the resume off: %v", h.order)
	}
}

func TestResumeCleanStopPauses_SkipsClosedAndPRCards(t *testing.T) {
	h := &csHarness{}
	eng := csEngine(t, csClient(h, csEvidence()), true)
	b := csBoard(1, 2)
	b.Items[0].IsClosed = true
	b.Items[1].IsPR = true
	eng.resumeCleanStopPauses(b)
	if len(h.order) != 0 {
		t.Errorf("writes for closed/PR items: %v", h.order)
	}
}

func TestResumeCleanStopPauses_HumanRepauseStaysPaused(t *testing.T) {
	h := &csHarness{}
	ev := csEvidence()
	ev.PausedUnlabeledAt = csT0.Add(time.Minute)
	ev.PausedLabeledAt = csT0.Add(time.Minute)
	eng := csEngine(t, csClient(h, ev), true)
	eng.resumeCleanStopPauses(csBoard(1))
	if len(h.order) != 0 {
		t.Errorf("a human re-pause was lifted: %v", h.order)
	}
}

func TestResumeCleanStopPauses_ReadErrorsWriteNothingThenGiveUp(t *testing.T) {
	for _, which := range []string{"labels", "comments", "events"} {
		t.Run(which, func(t *testing.T) {
			h := &csHarness{}
			client := csClient(h, csEvidence())
			boom := errors.New("boom")
			switch which {
			case "labels":
				client.fetchLabelsFn = func(_, _ string, _ int) ([]string, error) { return nil, boom }
			case "comments":
				client.fetchIssueCommentsFn = func(_, _ string, _ int) ([]gh.Comment, error) { return nil, boom }
			case "events":
				client.fetchLabelRemovedAtFn = func(_, _ string, _ int, _ string) (time.Time, error) { return time.Time{}, boom }
			}
			eng := csEngine(t, client, true)
			board := csBoard(1)
			for i := 0; i < 6; i++ {
				eng.resumeCleanStopPauses(board)
			}
			if len(h.order) != 0 {
				t.Errorf("writes after read errors: %v", h.order)
			}
			if len(eng.cleanStopResume.candidates) != 0 {
				t.Errorf("candidate not dropped after %d attempts", cleanStopResumeMaxAttempts)
			}
		})
	}
}

func TestResumeCleanStopPauses_FailedRemovalPostsNoCommentAndRetries(t *testing.T) {
	h := &csHarness{}
	client := csClient(h, csEvidence())
	calls := 0
	client.removeLabelFromIssueFn = func(_, _ string, n int, l string) error {
		calls++
		if l == "fabrik:paused" && calls == 1 {
			return errors.New("github 502")
		}
		h.removed = append(h.removed, itemKey(n, l))
		return nil
	}
	eng := csEngine(t, client, true)
	board := csBoard(1)
	eng.resumeCleanStopPauses(board)
	if len(h.comments) != 0 {
		t.Fatalf("comment posted although the pause was not lifted: %v", h.comments)
	}
	eng.resumeCleanStopPauses(board)
	if len(h.comments) != 1 {
		t.Errorf("comments after the retry = %v, want 1", h.comments)
	}
}

func TestResumeCleanStopPauses_CapsEvaluationsPerPoll(t *testing.T) {
	h := &csHarness{}
	eng := csEngine(t, csClient(h, csEvidence()), true)
	var nums []int
	for i := 1; i <= 14; i++ {
		nums = append(nums, i)
	}
	board := csBoard(nums...)
	eng.resumeCleanStopPauses(board)
	if len(h.comments) != cleanStopResumeMaxPerPoll {
		t.Errorf("first poll resumed %d, want %d", len(h.comments), cleanStopResumeMaxPerPoll)
	}
	eng.resumeCleanStopPauses(board)
	if len(h.comments) != 14 {
		t.Errorf("after the second poll resumed %d, want 14", len(h.comments))
	}
}

// A pause that appears after the first populated board is never examined.
func TestResumeCleanStopPauses_CandidateSetIsFrozenAtFirstBoard(t *testing.T) {
	h := &csHarness{}
	eng := csEngine(t, csClient(h, csEvidence()), true)
	eng.resumeCleanStopPauses(csBoard(1))
	before := len(h.comments)
	eng.resumeCleanStopPauses(csBoard(1, 2))
	if len(h.comments) != before {
		t.Errorf("a pause seen after the first board was resumed: %v", h.comments)
	}
}

// FR-012: the resume comment is not steering and not a pause comment.
func TestCleanStopResumeBody_IsNotSteeringOrPauseWording(t *testing.T) {
	if !strings.HasPrefix(cleanStopResumeBody, fabrikCommentPrefix) {
		t.Error("resume comment lacks the 🏭 **Fabrik prefix")
	}
	if removePausedRE.MatchString(cleanStopResumeBody) || commentResumeRE.MatchString(cleanStopResumeBody) {
		t.Error("resume comment reads as a pause comment")
	}
	if strings.Contains(cleanStopResumeBody, pauseCauseDaemonStopMarker) {
		t.Error("resume comment carries the daemon-stop marker")
	}
	// Under the walk that decides a later pause, it is skipped, not a marker.
	if _, ok := newestPauseMarkerComment([]gh.Comment{{Body: cleanStopResumeBody, CreatedAt: csT0}}, pauseCauseDaemonStopMarker); ok {
		t.Error("resume comment matched as a clean-stop pause")
	}
}

// The literal-header form the compliance scan requires at the call site must
// stay identical to the constant the tests and docs reference.
func TestCleanStopResumeBody_MatchesPostedLiteral(t *testing.T) {
	if got := fmt.Sprintf("🏭 **Fabrik — resumed after restart**\n\n%s", cleanStopResumeDetail); got != cleanStopResumeBody {
		t.Errorf("posted literal %q != cleanStopResumeBody %q", got, cleanStopResumeBody)
	}
}
