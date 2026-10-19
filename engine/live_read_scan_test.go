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
// shape the issue removes. Only live_read.go is exempt as a file. Every other raw
// call is audited per call site: it must carry a `// live-read-audited: <reason>`
// comment on its own line or the line directly above, so a new raw read added to a
// file that already has an audited one (checkReviewGate, refuseIfPausedLive, ...)
// still fails the scan.
func TestLiveReadsAreOnlyMadeThroughTheHelpers(t *testing.T) {
	raw := regexp.MustCompile(`\be\.client\.(FetchLabels|FetchItemDetails)\(|\.FetchPRMerged\(|\be\.readClient\.(FetchPRReviews|FetchPRReviewRequests)\(`)
	const marker = "live-read-audited:"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "live_read.go" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !raw.MatchString(line) {
				continue
			}
			audited := strings.Contains(line, marker) || (i > 0 && strings.Contains(lines[i-1], marker))
			if !audited {
				t.Errorf("%s:%d reads GitHub live without the live-read helpers: %s\n\tuse liveLabels/liveItem/livePRMerged/liveReviews (engine/live_read.go), or mark the call `// %s <reason>`",
					name, i+1, trimmed, marker)
			}
		}
	}
}

// The scan above is only worth anything if its pattern still matches the calls it
// guards: a renamed client method must not turn it into a no-op.
func TestLiveReadScanStillSeesTheHelperCalls(t *testing.T) {
	data, err := os.ReadFile("live_read.go")
	if err != nil {
		t.Fatal(err)
	}
	raw := regexp.MustCompile(`\be\.client\.(FetchLabels|FetchItemDetails)\(|\.FetchPRMerged\(|\be\.readClient\.(FetchPRReviews|FetchPRReviewRequests)\(`)
	if n := len(raw.FindAllString(string(data), -1)); n < 5 {
		t.Fatalf("the scan pattern matches %d raw calls in live_read.go, want at least 5: it no longer sees the client methods", n)
	}
}
