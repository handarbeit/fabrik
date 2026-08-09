package simgh

import (
	"testing"
	"time"
)

// advancingClock moves forward on every read, the way the default realClock
// does. The rest of the suite uses the fixed fakeClock, which makes two
// s.now() reads in one mutation indistinguishable from one — so a coherence
// bug in timestamp handling is invisible to every other test in the package.
type advancingClock struct {
	t    time.Time
	step time.Duration
}

func newAdvancingClock() *advancingClock {
	return &advancingClock{
		t:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		step: time.Second,
	}
}

func (c *advancingClock) Now() time.Time {
	c.t = c.t.Add(c.step)
	return c.t
}

// simWithAdvancingClock is newSim with a clock that ticks on every read.
func simWithAdvancingClock(t *testing.T) *Sim {
	t.Helper()
	skipIfNoGit(t)
	return New(t.TempDir(), WithClock(newAdvancingClock()))
}

// TestOneMutationStampsOneTimestamp pins that a single mutation reads the clock
// once and stamps every field it touches with that one value.
//
// This is a coherence property, not a cosmetic one. A mutation that reads the
// clock per field lets an item's timestamp and its owning project's — or a
// label's applied-at and its issue's updated-at — disagree for what GitHub
// reports as one event. The engine reads exactly these pairs against each
// other: FetchProjectUpdatedAt gates whether a poll bothers to look at the
// board at all, and FetchLabelAppliedAt is the timing anchor for the review
// gate, the CI settle scan, and the Done-archive scan.
//
// The whole test hangs on the clock: under the fixed fakeClock every ordering
// produces equal timestamps and the assertions below hold vacuously, which is
// exactly why it uses an advancing one.
func TestOneMutationStampsOneTimestamp(t *testing.T) {
	t.Run("card and project agree on a status move", func(t *testing.T) {
		s := simWithAdvancingClock(t)
		seedBoardForTimestamps(t, s)

		projectID, itemID := mustLookupItem(t, s)
		field, err := s.FetchStatusField(projectID)
		if err != nil {
			t.Fatalf("FetchStatusField: %v", err)
		}
		if err := s.UpdateProjectItemStatus(projectID, itemID, field.FieldID, field.Options["Review"]); err != nil {
			t.Fatalf("UpdateProjectItemStatus: %v", err)
		}

		// The card is the only thing this mutation touched, and the issue was
		// stamped strictly earlier at seed time, so the board projection's
		// laterOf(issue, card) resolves to the card's own timestamp.
		item, err := s.FetchProjectItem("acme", "widgets", 7)
		if err != nil {
			t.Fatalf("FetchProjectItem: %v", err)
		}
		projectAt, err := s.FetchProjectUpdatedAt(projectID)
		if err != nil {
			t.Fatalf("FetchProjectUpdatedAt: %v", err)
		}
		if !item.UpdatedAt.Equal(projectAt) {
			t.Fatalf("one status move stamped the card at %v but its project at %v; "+
				"they must share a single clock read", item.UpdatedAt, projectAt)
		}
	})

	t.Run("label applied-at and issue updated-at agree", func(t *testing.T) {
		s := simWithAdvancingClock(t)
		seedBoardForTimestamps(t, s)

		if err := s.AddLabelToIssue("acme", "widgets", 7, "fabrik:awaiting-review"); err != nil {
			t.Fatalf("AddLabelToIssue: %v", err)
		}

		appliedAt, err := s.FetchLabelAppliedAt("acme", "widgets", 7, "fabrik:awaiting-review")
		if err != nil {
			t.Fatalf("FetchLabelAppliedAt: %v", err)
		}
		// Only the issue moved; the card was stamped earlier at seed time, so
		// the projection's laterOf resolves to the issue's updated-at.
		item, err := s.FetchProjectItem("acme", "widgets", 7)
		if err != nil {
			t.Fatalf("FetchProjectItem: %v", err)
		}
		if !appliedAt.Equal(item.UpdatedAt) {
			t.Fatalf("one label application stamped applied-at %v but the issue %v; "+
				"they must share a single clock read", appliedAt, item.UpdatedAt)
		}
	})

	t.Run("comment created-at and its issue's updated-at agree", func(t *testing.T) {
		s := simWithAdvancingClock(t)
		seedBoardForTimestamps(t, s)

		if _, err := s.AddComment("acme", "widgets", 7, "hello"); err != nil {
			t.Fatalf("AddComment: %v", err)
		}

		item, err := s.FetchProjectItem("acme", "widgets", 7)
		if err != nil {
			t.Fatalf("FetchProjectItem: %v", err)
		}
		// Comments arrive on the deep read, not the shallow board projection.
		if err := s.FetchItemDetails(item); err != nil {
			t.Fatalf("FetchItemDetails: %v", err)
		}
		if len(item.Comments) != 1 {
			t.Fatalf("comments = %d, want 1", len(item.Comments))
		}
		if !item.Comments[0].CreatedAt.Equal(item.UpdatedAt) {
			t.Fatalf("one comment stamped created-at %v but its issue %v; "+
				"they must share a single clock read", item.Comments[0].CreatedAt, item.UpdatedAt)
		}
	})
}

// seedBoardForTimestamps is seedBasicBoard's body without the fakeClock, so
// the caller can supply an advancing one.
func seedBoardForTimestamps(t *testing.T, s *Sim) {
	t.Helper()
	s.SeedRepo("acme/widgets").
		SeedProject("acme", 2, "Engineering", []string{"Backlog", "Implement", "Review", "Done"}).
		SeedIssue("acme/widgets", IssueSeed{
			Number: 7,
			Title:  "Add a thing",
			Body:   "spec body",
			Author: "human",
			Status: "Implement",
		})
	if err := s.Err(); err != nil {
		t.Fatalf("seeding: %v", err)
	}
}

// mustLookupItem resolves issue 7's project and card IDs.
func mustLookupItem(t *testing.T, s *Sim) (projectID, itemID string) {
	t.Helper()
	board, err := s.FetchProjectBoard("acme", "widgets", 2, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	itemID, _, err = s.LookupIssueProjectItem(board.ProjectID, "acme/widgets", 7)
	if err != nil {
		t.Fatalf("LookupIssueProjectItem: %v", err)
	}
	return board.ProjectID, itemID
}
