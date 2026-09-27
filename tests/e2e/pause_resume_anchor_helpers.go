//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Helpers for TestPauseLiftedOnlyByPostPauseHumanComment (#1876, ADR-1813).

// pausedAwaitingInputLabel is the second half of the engine's pause pair. It
// always co-occurs with fabrik:paused and selects which refusal log line
// itemNeedsWork emits (the awaiting-input branch is checked first).
const pausedAwaitingInputLabel = "fabrik:awaiting-input"

// nudgeCommentPrefix opens a comment the engine's findNewComments always skips
// (bodies starting "🏭 **Fabrik"). Posting one changes the item's comments, so
// the poll re-evaluates the item (cycleSet), but it can never resume a pause.
const nudgeCommentPrefix = "🏭 **Fabrik — e2e nudge**"

// pauseRefusalLogNeedle returns the substring of the engine log line emitted by
// itemNeedsWork (engine/item.go) when an item carrying BOTH fabrik:paused and
// fabrik:awaiting-input has exactly one human comment that predates the pause:
//
//	<RFC3339> [#N skip] awaiting-input: 1 human comment(s) predate the pause — still waiting
//
// Copied from the format string at engine/item.go's awaiting-input branch, with
// the issue number prefix so it is scoped to one issue. It is deliberately NOT
// the "resume refused (ADR-1813)" text: those processItem lines are unreachable
// on the normal dispatch path, because itemNeedsWork has already returned false.
func pauseRefusalLogNeedle(issue int) string {
	return fmt.Sprintf("[#%d skip] awaiting-input: 1 human comment(s) predate the pause — still waiting", issue)
}

// labelEvent is one labeled/unlabeled entry from an issue's events log.
type labelEvent struct {
	Event string // "labeled" or "unlabeled"
	Label string
	At    time.Time
}

// parseLabelEvents extracts the labeled/unlabeled events from an issue-events
// payload: one or more concatenated JSON arrays, as `gh api --paginate` emits.
// Events of any other kind are ignored.
func parseLabelEvents(body []byte) ([]labelEvent, error) {
	var out []labelEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var page []struct {
			Event     string `json:"event"`
			CreatedAt string `json:"created_at"`
			Label     *struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parsing issue events: %w", err)
		}
		for _, ev := range page {
			if (ev.Event != "labeled" && ev.Event != "unlabeled") || ev.Label == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, ev.CreatedAt)
			if err != nil {
				return nil, fmt.Errorf("%s event created_at %q: %w", ev.Event, ev.CreatedAt, err)
			}
			out = append(out, labelEvent{Event: ev.Event, Label: ev.Label.Name, At: at})
		}
	}
	return out, nil
}

// countLabelEvents returns how many times label was added and removed.
func countLabelEvents(events []labelEvent, label string) (labeled, unlabeled int) {
	for _, ev := range events {
		if ev.Label != label {
			continue
		}
		switch ev.Event {
		case "labeled":
			labeled++
		case "unlabeled":
			unlabeled++
		}
	}
	return labeled, unlabeled
}

// commentReactions is the reaction tally over the comments whose body contains
// a given substring.
type commentReactions struct {
	Matches int `json:"matches"`
	Eyes    int `json:"eyes"`
	Rocket  int `json:"rocket"`
}

// parseCommentReactions decodes the jq summary produced by commentReactionsJQ.
func parseCommentReactions(out string) (commentReactions, error) {
	var r commentReactions
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil {
		return commentReactions{}, fmt.Errorf("parsing reaction summary %q: %w", out, err)
	}
	return r, nil
}

// commentReactionsJQ builds the jq filter that tallies 👀/🚀 across the issue
// comments whose body contains substring. Callers pass an alphanumeric nonce;
// backslashes and quotes are escaped regardless.
func commentReactionsJQ(substring string) string {
	esc := strings.ReplaceAll(substring, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	return fmt.Sprintf(`[.[] | select(.body | contains("%s"))] | {matches: length, eyes: (map(.reactions.eyes) | add // 0), rocket: (map(.reactions.rocket) | add // 0)}`, esc)
}

// tryIssueLabelEvents reads the issue's labeled/unlabeled events over REST
// (issues/N/events, no GraphQL cost). The events log is durable: it survives a
// label being removed and re-added between polls, which current-label reads miss.
func tryIssueLabelEvents(env *Env, repo string, issueNumber int) ([]labelEvent, error) {
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/issues/%d/events?per_page=100", repo, issueNumber))
	if err != nil {
		return nil, fmt.Errorf("read events for %s#%d: %w\n%s", repo, issueNumber, err, out)
	}
	return parseLabelEvents([]byte(out))
}

// tryCommentReactions is a single, non-blocking read of the reaction tally for
// the issue comments containing substring.
func tryCommentReactions(env *Env, repo string, issueNumber int, substring string) (commentReactions, error) {
	owner, name, ok := splitRepo(repo)
	if !ok {
		return commentReactions{}, fmt.Errorf("bad repo: %q", repo)
	}
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100", owner, name, issueNumber),
		"--jq", commentReactionsJQ(substring))
	if err != nil {
		return commentReactions{}, fmt.Errorf("read comments for %s#%d: %w\n%s", repo, issueNumber, err, out)
	}
	return parseCommentReactions(out)
}

// requireCommentReactions is tryCommentReactions that fails the test on error.
func requireCommentReactions(t *testing.T, env *Env, repo string, issueNumber int, substring string) commentReactions {
	t.Helper()
	r, err := tryCommentReactions(env, repo, issueNumber, substring)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return r
}

// waitForCommentReaction polls until a comment containing substring has at
// least one reaction of the given kind ("eyes" or "rocket"), or fails at
// timeout. Transient read errors are logged and retried.
func waitForCommentReaction(t *testing.T, env *Env, repo string, issueNumber int, substring, kind string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := tryCommentReactions(env, repo, issueNumber, substring)
		if err != nil {
			t.Logf("waitForCommentReaction: transient error on %s#%d: %v (will retry)", repo, issueNumber, err)
		} else if (kind == "eyes" && r.Eyes > 0) || (kind == "rocket" && r.Rocket > 0) {
			return
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out after %s waiting for %q reaction on the comment containing %q on %s#%d",
		timeout, kind, substring, repo, issueNumber)
}

// ensureEngineLabelExists creates an engine-owned label on repo if it is
// missing, so `gh issue edit --add-label` cannot fail on a fresh bed. Like
// ensurePausedLabelExists it registers NO delete cleanup: the label is shared
// engine state and deleting it repo-wide would strip it from unrelated issues.
func ensureEngineLabelExists(t *testing.T, env *Env, repo, label string) {
	t.Helper()
	exists := func() bool {
		_, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/labels/%s", repo, strings.ReplaceAll(label, ":", "%3A")))
		return err == nil
	}
	if exists() {
		return
	}
	out, err := ghOutput(env, "label", "create", label, "-R", repo, "--color", "e99695")
	if err != nil && !exists() {
		t.Fatalf("ensure label %q exists on %s: %v\n%s", label, repo, err, out)
	}
}
