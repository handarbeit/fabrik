//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

// Issue-event-log helpers for TestYoloRemovedMidValidateBlocksMerge (#1769) and
// TestCommentReentryShowsReworking (#1802).
//
// Neither fix logs anything the bed log could prove (refreshAutonomyLabels has no
// success log; the rework swap has none), and the windows they concern are either
// minutes long but racy to sample (S1) or sub-second between two label writes (S2).
// The issue events log is durable and totally ordered by its monotonic event id,
// so both scenarios assert on ORDER in that log — never on sleeps, and never on
// created_at (1s resolution).
//
// The parsing and ordering logic here is pure (unit-tested in
// label_events_test.go); only fetchIssueEvents touches the network.

// reworkingLabelPrefix mirrors the engine's unexported constant of the same name
// (engine/comments.go): the rework marker for a stage is reworkingLabelPrefix +
// the stage name. The engine's constant cannot be imported from this package, so
// the literal is carried here and pinned by TestReworkingLabelPrefixLiteral.
const reworkingLabelPrefix = "fabrik:reworking:"

// Event kinds as GitHub reports them in the issue events log.
const (
	eventLabeled   = "labeled"
	eventUnlabeled = "unlabeled"
)

// issueEvent is one entry of an issue's events log, reduced to what the ordering
// checks need. Label is empty for events that carry no label.
type issueEvent struct {
	ID    int64
	Kind  string
	Label string
}

// parseIssueEvents parses an issue-events payload (one or more concatenated JSON
// arrays, as `gh api --paginate` emits), preserving list order.
func parseIssueEvents(body []byte) ([]issueEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var out []issueEvent
	for {
		var page []struct {
			ID    int64  `json:"id"`
			Event string `json:"event"`
			Label *struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if err := dec.Decode(&page); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parsing issue events: %w", err)
		}
		for _, ev := range page {
			e := issueEvent{ID: ev.ID, Kind: ev.Event}
			if ev.Label != nil {
				e.Label = ev.Label.Name
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// maxEventID returns the highest event id, or 0 for an empty log.
func maxEventID(events []issueEvent) int64 {
	var max int64
	for _, e := range events {
		if e.ID > max {
			max = e.ID
		}
	}
	return max
}

// firstEvent returns the lowest-id event of the given kind and label with id
// greater than afterID.
func firstEvent(events []issueEvent, kind, label string, afterID int64) (issueEvent, bool) {
	var best issueEvent
	found := false
	for _, e := range events {
		if e.Kind != kind || e.Label != label || e.ID <= afterID {
			continue
		}
		if !found || e.ID < best.ID {
			best, found = e, true
		}
	}
	return best, found
}

// checkYoloRemovedMidValidate proves the S1 timing window from the events log:
//
//	labeled stage:Validate:in_progress  <  unlabeled fabrik:yolo  <  labeled fabrik:awaiting-ci
//
// fabrik:awaiting-ci is applied by handleStageComplete the instant Validate
// finishes, so a yolo removal that lands between the first two events happened
// while Validate was running — the window #1769's live re-read exists for. Any
// other ordering means the run cannot demonstrate the property, so the error is
// prefixed INCONCLUSIVE and the caller must fail (never skip) on it.
func checkYoloRemovedMidValidate(events []issueEvent) error {
	const yolo = "fabrik:yolo"
	inProgress, ok := firstEvent(events, eventLabeled, "stage:Validate:in_progress", 0)
	if !ok {
		return fmt.Errorf("INCONCLUSIVE: no `labeled stage:Validate:in_progress` event — Validate never started")
	}
	removed, ok := firstEvent(events, eventUnlabeled, yolo, 0)
	if !ok {
		return fmt.Errorf("INCONCLUSIVE: no `unlabeled %s` event — the removal never landed", yolo)
	}
	awaitingCI, ok := firstEvent(events, eventLabeled, "fabrik:awaiting-ci", 0)
	if !ok {
		return fmt.Errorf("INCONCLUSIVE: no `labeled fabrik:awaiting-ci` event — Validate never signalled completion")
	}
	if removed.ID < inProgress.ID {
		return fmt.Errorf("INCONCLUSIVE: %s was removed (event %d) before Validate started (stage:Validate:in_progress, event %d) — not a mid-stage removal",
			yolo, removed.ID, inProgress.ID)
	}
	if removed.ID > awaitingCI.ID {
		return fmt.Errorf("INCONCLUSIVE: %s was removed (event %d) after Validate completed (fabrik:awaiting-ci, event %d) — the removal missed the in-progress window",
			yolo, removed.ID, awaitingCI.ID)
	}
	return nil
}

// checkReworkSequence proves the S2 property from the events log: after sinceID
// (the highest event id captured just before the re-entry comment was posted, so
// the genuine earlier `labeled stage:<stage>:complete` is ignored), the events
// occur in this order:
//
//	1. labeled   fabrik:reworking:<stage>   marker added first (ADR-1802)
//	2. unlabeled stage:<stage>:complete     completion cleared while the marker is on
//	3. labeled   stage:<stage>:complete     completion restored
//	4. unlabeled fabrik:reworking:<stage>   marker removed last
//
// Steps 2..3 bracket the window in which the board would have shown the item as
// not-complete; step 1 before 2 and 3 before 4 mean that window is always covered
// by the marker. Pre-#1802 the engine never removed the complete label, so step 2
// has no event and the check fails there.
func checkReworkSequence(events []issueEvent, sinceID int64, stage string) error {
	marker := reworkingLabelPrefix + stage
	complete := "stage:" + stage + ":complete"
	steps := []struct {
		kind, label, desc string
	}{
		{eventLabeled, marker, "marker added"},
		{eventUnlabeled, complete, "completion cleared"},
		{eventLabeled, complete, "completion restored"},
		{eventUnlabeled, marker, "marker removed"},
	}
	after := sinceID
	for i, s := range steps {
		ev, ok := firstEvent(events, s.kind, s.label, after)
		if !ok {
			return fmt.Errorf("rework sequence step %d (%s): no `%s %s` event after event %d — expected order: labeled %s, unlabeled %s, labeled %s, unlabeled %s",
				i+1, s.desc, s.kind, s.label, after, marker, complete, complete, marker)
		}
		after = ev.ID
	}
	return nil
}

// fetchIssueEvents reads the issue's full events log (paginated).
func fetchIssueEvents(env *Env, repo string, issueNumber int) ([]issueEvent, error) {
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/issues/%d/events?per_page=100", repo, issueNumber))
	if err != nil {
		return nil, fmt.Errorf("read events for %s#%d: %w\n%s", repo, issueNumber, err, out)
	}
	return parseIssueEvents([]byte(out))
}

// mustFetchIssueEvents is the fatal counterpart to fetchIssueEvents.
func mustFetchIssueEvents(t *testing.T, env *Env, repo string, issueNumber int) []issueEvent {
	t.Helper()
	events, err := fetchIssueEvents(env, repo, issueNumber)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
