package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// statusLineSeq renders the ordered status-line writes the mock recorded for
// one project item: the text of each set, "<cleared>" for a clear.
func statusLineSeq(c *mockGitHubClient, itemID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, w := range c.statusLineWrites {
		if w.itemID != itemID {
			continue
		}
		if w.cleared {
			out = append(out, "<cleared>")
		} else {
			out = append(out, w.text)
		}
	}
	return out
}

func enableStatusLine(t *testing.T, eng *Engine, client *mockGitHubClient) {
	t.Helper()
	client.mu.Lock()
	client.textField = &gh.TextField{ID: "FIELD_TXT", Name: "Fabrik"}
	client.mu.Unlock()
	eng.cfg.StatusLineField = "Fabrik"
	eng.resolveStatusLineField("PVT_1")
}

func runSeamTrain(t *testing.T, eng *Engine, n int) {
	t.Helper()
	batch := makeSeamBatch(n)
	state := &mergeTrainWorkerState{assembling: true, projectID: "PVT_1"}
	eng.mergeTrainInFlight.Store(mergeTrainKey("owner/repo", "main"), state)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	captureStdout(func() { eng.runMergeTrainWorker(ctx, state, "owner", "repo", "main", batch) })
}

func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A green batch: queued → trial CI → landing → cleared at Done, each line once.
func TestStatusLine_Train_GreenBatchSequence(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	enableStatusLine(t, eng, client)

	runSeamTrain(t, eng, 3)

	want := []string{"queued · batch of 3", "trial · CI running", "landing", "<cleared>"}
	for i := 1; i <= 3; i++ {
		if got := statusLineSeq(client, fmt.Sprintf("item-%d", i)); !equalSeq(got, want) {
			t.Errorf("item-%d lines = %q, want %q", i, got, want)
		}
	}
}

// A red batch is bisected: the isolating steps are shown as "bisecting · step i
// of n" and the sub-trials do not overwrite that line with their own CI lines.
func TestStatusLine_Train_BisectSteps(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(present map[int]bool) bool { return present[2] })
	enableStatusLine(t, eng, client)

	runSeamTrain(t, eng, 4)

	var sawBisect bool
	for i := 1; i <= 4; i++ {
		seq := statusLineSeq(client, fmt.Sprintf("item-%d", i))
		if len(seq) == 0 || seq[0] != "queued · batch of 4" {
			t.Errorf("item-%d lines = %q, want to start with the queued line", i, seq)
		}
		for j, l := range seq {
			if strings.HasPrefix(l, "bisecting · step ") {
				sawBisect = true
				if !strings.Contains(l, " of ") {
					t.Errorf("item-%d bisect line %q lacks a ceiling", i, l)
				}
				// A sub-trial's own CI line must not directly overwrite a bisect line.
				if j+1 < len(seq) && seq[j+1] == "trial · CI running" && !strings.HasPrefix(seq[j], "bisecting") {
					t.Errorf("item-%d: unexpected trial line after bisect: %q", i, seq)
				}
			}
		}
	}
	if !sawBisect {
		t.Fatalf("no member showed a bisecting line: %q", statusLineSeq(client, "item-1"))
	}
	// Unchanged values are never rewritten: no member sees two equal lines in a row.
	for i := 1; i <= 4; i++ {
		seq := statusLineSeq(client, fmt.Sprintf("item-%d", i))
		for j := 1; j < len(seq); j++ {
			if seq[j] == seq[j-1] {
				t.Errorf("item-%d wrote %q twice in a row: %q", i, seq[j], seq)
			}
		}
	}
}

// With the feature on but no such field on the board, a full train run writes
// nothing and logs one startup line (and never per transition).
func TestStatusLine_Train_MissingFieldWritesNothing(t *testing.T) {
	skipIfNoGit(t)
	_, _, _, wm := setupTrainRepo(t)
	eng, client, _ := seamTrainEngine(t, wm, func(map[int]bool) bool { return false })
	eng.cfg.StatusLineField = "Fabrik"
	eng.resolveStatusLineField("PVT_1") // mock has no text field

	runSeamTrain(t, eng, 2)

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.statusLineWrites) != 0 {
		t.Fatalf("writes = %+v, want none", client.statusLineWrites)
	}
}

func TestStatusLine_Train_OverlapDeferredLine(t *testing.T) {
	eng, _, client := newOverlapEngine(t, map[int][]string{
		1: {"hot.go"},
		2: {"hot.go"},
		3: {"c.go"},
	})
	enableStatusLine(t, eng, client)
	members := admissionMembers(1, 2, 3)
	for i := range members {
		members[i].item.ItemID = fmt.Sprintf("item-%d", members[i].item.Number)
	}
	captureStdout(func() { eng.admitByOverlap(overlapTestKey, "o", "r", members) })

	if got := statusLineSeq(client, "item-2"); !equalSeq(got, []string{"deferred: overlaps #1"}) {
		t.Errorf("deferred member lines = %q", got)
	}
	for _, id := range []string{"item-1", "item-3"} {
		if got := statusLineSeq(client, id); len(got) != 0 {
			t.Errorf("%s (admitted) lines = %q, want none from the overlap filter", id, got)
		}
	}
}
