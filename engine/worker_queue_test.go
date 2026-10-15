package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
)

// A queued worker (#2121) holds no slot, process or lock: the PID<=0 stale-worker
// detector must not clear it however long it has waited, or WorkerSlotAcquired
// would later hit a nil handle and the labels would be removed under a
// legitimately waiting reinvoke.
func TestDetectorSkipsQueuedWorkerPastTheStaleTimeout(t *testing.T) {
	client := &mockGitHubClient{}
	e := testEngine(t, client, &mockClaudeInvoker{})
	bootstrapItem(t, e, 12, []string{"fabrik:editing"})
	long := time.Now().Add(-time.Hour)
	e.store.Apply(itemstate.WorkerEntered{
		Repo: "owner/repo", Number: 12, StageName: "Implement", StartedAt: long,
		Kind: itemstate.WorkerKindReviewReinvoke, Queued: true, QueuedSince: long,
	})

	e.runWorkerDetectorScan()

	if w := getWorker(t, e, 12); w == nil {
		t.Fatal("detector cleared a queued worker")
	}
	if removed := removeLabelsCalled(client, 12); len(removed) > 0 {
		t.Errorf("detector removed labels for a queued worker: %v", removed)
	}

	// The same handle, once it holds a slot and has gone stale, is cleared as before.
	e.store.Apply(itemstate.WorkerSlotAcquired{Repo: "owner/repo", Number: 12, At: long})
	e.runWorkerDetectorScan()
	if w := getWorker(t, e, 12); w != nil {
		t.Errorf("a slot-holding PID-less worker past the stale timeout must still be cleared, got %+v", w)
	}
}

// workerEnteredSites lists the audited non-test call sites that apply
// itemstate.WorkerEntered (#2121, ADR 2121). WorkerEntered claims the item; a
// site that claims it before holding a worker slot must say so with Queued.
// Adding a site means deciding which it is.
var workerEnteredSites = map[string]string{
	"poll.go":     "dispatchCandidates: takes e.sem (non-blocking, ADR 2114) BEFORE WorkerEntered, so the worker holds a slot",
	"reinvoke.go": "dispatchReinvoke: claims the item on the poll goroutine with Queued: true; its goroutine applies WorkerSlotAcquired after e.sem",
}

func TestWorkerEnteredSitesAreAudited(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "itemstate.WorkerEntered{") {
				found = append(found, name)
			}
		}
	}
	sort.Strings(found)
	var unaudited []string
	for _, f := range found {
		if _, ok := workerEnteredSites[f]; !ok {
			unaudited = append(unaudited, f)
		}
	}
	if len(unaudited) > 0 {
		t.Errorf("itemstate.WorkerEntered applied in unaudited file(s) %v: record in workerEnteredSites whether the site holds a worker slot first or applies it Queued", unaudited)
	}
	seen := map[string]bool{}
	for _, f := range found {
		seen[f] = true
	}
	for f := range workerEnteredSites {
		if !seen[f] {
			t.Errorf("workerEnteredSites lists %s but it no longer applies WorkerEntered", f)
		}
	}
}
