package engine

import "testing"

// TestSpawnChildLabelLength guards against the spawnChildLabel format
// exceeding GitHub's 50-character REST API limit for label names, mirroring
// TestBotRepromptedLabelLength's precedent (engine/reviews_test.go). Checked
// at generous magnitudes — a 4-digit block index and a 10-digit issue
// number — since GitHub issue numbers are unbounded in principle and a
// Plan could in theory declare thousands of spawn blocks.
func TestSpawnChildLabelLength(t *testing.T) {
	cases := []struct {
		blockIndex, childNumber int
	}{
		{1, 1},
		{9999, 9999999999},
	}
	for _, c := range cases {
		label := spawnChildLabel(c.blockIndex, c.childNumber)
		if len(label) > 50 {
			t.Errorf("spawnChildLabel(%d, %d) = %q is %d chars (max 50)", c.blockIndex, c.childNumber, label, len(label))
		}
	}
}

// TestParseSpawnChildLabels_RoundTrip verifies parseSpawnChildLabels recovers
// exactly the blockIndex -> childNumber mapping spawnChildLabel encodes, and
// ignores unrelated labels.
func TestParseSpawnChildLabels_RoundTrip(t *testing.T) {
	labels := []string{
		"stage:Plan:complete",
		spawnChildLabel(1, 101),
		spawnChildLabel(2, 205),
		"fabrik:yolo",
	}
	got := parseSpawnChildLabels(labels)
	want := map[int]int{1: 101, 2: 205}
	if len(got) != len(want) {
		t.Fatalf("parseSpawnChildLabels(%v) = %v, want %v", labels, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseSpawnChildLabels[%d] = %d, want %d", k, got[k], v)
		}
	}
}

// TestParseSpawnChildLabels_Empty verifies an item with no spawn markers
// yields an empty map, not a nil-vs-empty distinction bugs could hide in.
func TestParseSpawnChildLabels_Empty(t *testing.T) {
	got := parseSpawnChildLabels([]string{"stage:Plan:complete", "fabrik:cruise"})
	if len(got) != 0 {
		t.Errorf("expected no markers parsed, got %v", got)
	}
}
