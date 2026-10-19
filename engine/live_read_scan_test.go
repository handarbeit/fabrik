package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestLiveReadsAreOnlyMadeThroughTheHelpers (#2350, ADR 2350): a gate that reads
// GitHub live must apply the result to the Store, which is what the helpers in
// live_read.go do. A raw call to one of the live-read client methods anywhere else
// in the engine is a gate that reads, decides and throws the result away — the
// shape the issue removes. Each file that still makes such a call is listed with
// the reason it is not a gate decision on the item's own state.
func TestLiveReadsAreOnlyMadeThroughTheHelpers(t *testing.T) {
	raw := regexp.MustCompile(`\be\.client\.(FetchLabels|FetchItemDetails)\(|\.FetchPRMerged\(|\be\.readClient\.(FetchPRReviews|FetchPRReviewRequests)\(`)

	exceptions := map[string]string{
		"live_read.go":              "the helpers themselves",
		"interfaces.go":             "the client interface declaration",
		"merge_train.go":            "reads each batch member's labels at formation; the train deliberately avoids Store-sourced member state (ADR 1208, 1773)",
		"merge_train_catchup.go":    "singleton catch-up ownership check on a batch member (ADR 2044)",
		"overseer_actions.go":       "overseer action guards read the target's live labels for a refusal; they apply nothing by design (ADR 1966-c)",
		"overseer_advance.go":       "overseer advance gates read live to refuse; a refused action writes nothing (ADR 2126)",
		"pause_guard.go":            "merge-train member pause check (refuseIfMembersPaused); members are batch state, not the poll's item (ADR 2341)",
		"pr_settle.go":              "settlePRMergeState's terminal confirmation on the settle result's own PR; the result carries it to the caller",
		"prcreate.go":               "linkage heal reads the PR it is repairing before the item is tracked as linked",
		"spawn.go":                  "spawn recovery reads of a parent/child item that may not be in the Store (ADR 1419, 1583)",
		"reviews.go":                "feedback-body and timeout-message review reads (resolveReviewsForFeedback, resolvedReviewData, pauseForReviewTimeout): they build comments or wording, not a gate verdict",
		"feedback_dispatch_view.go": "the dispatcher-view read is deliberately the cache-served view (readClient), compared against the gate's live read (ADR 2160)",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := exceptions[name]; ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if raw.MatchString(line) {
				t.Errorf("%s:%d reads GitHub live without the live-read helpers: %s\n\tuse liveLabels/liveItem/livePRMerged/liveReviews (engine/live_read.go), or add the file to the exception list with a reason",
					name, i+1, trimmed)
			}
		}
	}
}
