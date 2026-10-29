package simgh

import (
	"errors"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// FetchCommitTime answers the seeded committer date per SHA, and a SHA nobody
// seeded is a wrapped gh.ErrNotFound — never a time invented from the clock — so
// the review-wait gate's fallback to the label time is reachable (#2467).
func TestFetchCommitTime_SeededPerSHAAndUnknownIsNotFound(t *testing.T) {
	s, _ := newSim(t)
	at := time.Date(2026, 10, 24, 16, 45, 40, 0, time.UTC)
	s.SeedRepo("acme/widgets").SeedCommitTime("acme/widgets", "sha-a", at)
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	got, err := s.FetchCommitTime("acme", "widgets", "sha-a")
	if err != nil || !got.Equal(at) {
		t.Fatalf("FetchCommitTime(sha-a) = %v, %v; want %v", got, err, at)
	}
	if _, err := s.FetchCommitTime("acme", "widgets", "sha-b"); !errors.Is(err, gh.ErrNotFound) {
		t.Errorf("FetchCommitTime(sha-b) err = %v; want gh.ErrNotFound", err)
	}
}

func TestFetchCommitTime_SurvivesSnapshotRestore(t *testing.T) {
	s, clk := newSim(t)
	at := time.Date(2026, 10, 24, 16, 45, 40, 0, time.UTC)
	s.SeedRepo("acme/widgets").SeedCommitTime("acme/widgets", "sha-a", at)
	snap, err := s.Snapshot(t.TempDir())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	restored, err := Restore(snap, t.TempDir(), WithClock(clk))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, err := restored.FetchCommitTime("acme", "widgets", "sha-a"); err != nil || !got.Equal(at) {
		t.Fatalf("restored FetchCommitTime = %v, %v; want %v", got, err, at)
	}
}
