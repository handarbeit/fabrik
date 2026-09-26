//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestPauseRefusalLogNeedle(t *testing.T) {
	// Exact text of engine/item.go's awaiting-input refusal format with N=1,
	// prefixed with the engine's "[#N tag]" log scope.
	got := pauseRefusalLogNeedle(1876)
	want := "[#1876 skip] awaiting-input: 1 human comment(s) predate the pause — still waiting"
	if got != want {
		t.Fatalf("needle = %q, want %q", got, want)
	}
	// Scoped to one issue: another issue's line must not match.
	line := "2026-09-26T21:40:00Z [#18760 skip] awaiting-input: 1 human comment(s) predate the pause — still waiting\n"
	if strings.Contains(line, pauseRefusalLogNeedle(1876)) {
		t.Fatalf("needle for #1876 matched a line for #18760")
	}
	real := "2026-09-26T21:40:00Z [#1876 skip] awaiting-input: 1 human comment(s) predate the pause — still waiting\n"
	if !strings.Contains(real, pauseRefusalLogNeedle(1876)) {
		t.Fatalf("needle did not match the engine's log line format")
	}
	// The unreachable processItem wording must never be the needle.
	if strings.Contains(got, "resume refused") {
		t.Fatalf("needle must not use the processItem 'resume refused' text: %q", got)
	}
}

func TestParseLabelEvents(t *testing.T) {
	// Two concatenated pages as `gh api --paginate` emits, with non-label
	// events mixed in.
	body := `[
 {"event":"labeled","created_at":"2026-09-26T21:40:00Z","label":{"name":"fabrik:paused"}},
 {"event":"commented","created_at":"2026-09-26T21:40:01Z"},
 {"event":"labeled","created_at":"2026-09-26T21:40:00Z","label":{"name":"fabrik:awaiting-input"}}
][
 {"event":"unlabeled","created_at":"2026-09-26T21:50:00Z","label":{"name":"fabrik:paused"}},
 {"event":"labeled","created_at":"2026-09-26T21:41:00Z","label":{"name":"other"}}
]`
	events, err := parseLabelEvents([]byte(body))
	if err != nil {
		t.Fatalf("parseLabelEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4 (commented ignored): %+v", len(events), events)
	}
	if l, u := countLabelEvents(events, "fabrik:paused"); l != 1 || u != 1 {
		t.Errorf("fabrik:paused labeled/unlabeled = %d/%d, want 1/1", l, u)
	}
	if l, u := countLabelEvents(events, "fabrik:awaiting-input"); l != 1 || u != 0 {
		t.Errorf("fabrik:awaiting-input labeled/unlabeled = %d/%d, want 1/0", l, u)
	}
	if l, u := countLabelEvents(events, "absent"); l != 0 || u != 0 {
		t.Errorf("absent label labeled/unlabeled = %d/%d, want 0/0", l, u)
	}
}

func TestParseLabelEventsEmptyAndMalformed(t *testing.T) {
	for _, body := range []string{"", "[]", "[][]"} {
		events, err := parseLabelEvents([]byte(body))
		if err != nil || len(events) != 0 {
			t.Errorf("parseLabelEvents(%q) = %v, %v; want no events, nil", body, events, err)
		}
	}
	if _, err := parseLabelEvents([]byte(`[{"event":"labeled"`)); err == nil {
		t.Errorf("truncated JSON: want error")
	}
	bad := `[{"event":"labeled","created_at":"yesterday","label":{"name":"x"}}]`
	if _, err := parseLabelEvents([]byte(bad)); err == nil {
		t.Errorf("bad created_at: want error")
	}
}

func TestCountLabelEventsRelabelCycle(t *testing.T) {
	// The pre-#1813 failure shape: the pause lifted and re-applied repeatedly.
	body := `[
 {"event":"labeled","created_at":"2026-09-26T21:40:00Z","label":{"name":"fabrik:paused"}},
 {"event":"unlabeled","created_at":"2026-09-26T21:41:00Z","label":{"name":"fabrik:paused"}},
 {"event":"labeled","created_at":"2026-09-26T21:41:05Z","label":{"name":"fabrik:paused"}},
 {"event":"unlabeled","created_at":"2026-09-26T21:42:00Z","label":{"name":"fabrik:paused"}}
]`
	events, err := parseLabelEvents([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if l, u := countLabelEvents(events, "fabrik:paused"); l != 2 || u != 2 {
		t.Fatalf("labeled/unlabeled = %d/%d, want 2/2", l, u)
	}
}

func TestParseCommentReactions(t *testing.T) {
	r, err := parseCommentReactions(`{"matches":1,"eyes":2,"rocket":0}` + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if r != (commentReactions{Matches: 1, Eyes: 2, Rocket: 0}) {
		t.Fatalf("got %+v", r)
	}
	if _, err := parseCommentReactions("not json"); err == nil {
		t.Fatalf("want error on non-JSON")
	}
}

func TestCommentReactionsJQEscapes(t *testing.T) {
	f := commentReactionsJQ(`a"b\c`)
	if !strings.Contains(f, `contains("a\"b\\c")`) {
		t.Fatalf("substring not escaped in %q", f)
	}
}
