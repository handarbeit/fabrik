package github

// TestRecordings_AreScrubbed is #1453 AC6's "passes on the committed set"
// half: it re-runs wirescrub.Findings over every file actually committed
// under github/testdata/recordings/, independent of what each fixture's own
// provenance.scrubbed field claims. docs/ publishes live to
// fabrik.handarbeit.io from this repo, so this is the last line of defense
// against a real token, PAT, or private email ever reaching git history —
// see AC6, and TestFindings_SeededSecretsDetected in
// internal/wirescrub/scrub_test.go for the matching seeded-fixture proof
// that Findings itself works.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/handarbeit/fabrik/internal/wirescrub"
)

func TestRecordings_AreScrubbed(t *testing.T) {
	dir := filepath.Join("testdata", "recordings")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		t.Skipf("%s does not exist yet — no recordings committed", dir)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		checked++
		if findings := wirescrub.Findings(data); len(findings) > 0 {
			t.Errorf("%s contains secret-shaped content: %v", path, findings)
		}
	}
	if checked == 0 {
		t.Skip("no recording fixtures committed yet")
	}
}
