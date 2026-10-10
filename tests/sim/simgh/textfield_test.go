package simgh

import (
	"testing"
	"time"
)

type steppedClock struct{ now time.Time }

func (c *steppedClock) Now() time.Time { return c.now }

func TestTextFieldRoundTripAndDiscount(t *testing.T) {
	clk := &steppedClock{now: time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)}
	s := New(t.TempDir(), WithClock(clk), WithStatusLineField("Fabrik"))
	s.SeedRepo("o/r")
	s.SeedProject("o", 1, "Board", []string{"Backlog", "Queued"})
	s.SeedTextField("o", 1, "Fabrik")
	s.SeedIssue("o/r", IssueSeed{Number: 7, Title: "t", Body: "b"})
	s.SeedProjectItem("o", 1, "o/r", 7, false, "Queued")
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}

	proj := s.defaultProject
	field, err := s.FetchTextField(proj.id, "Fabrik")
	if err != nil || field == nil {
		t.Fatalf("FetchTextField = %v, %v", field, err)
	}
	if f, _ := s.FetchTextField(proj.id, "Nope"); f != nil {
		t.Fatalf("missing field = %+v, want nil", f)
	}
	var itemID string
	for id := range proj.items {
		itemID = id
	}

	clk.now = clk.now.Add(time.Minute)
	if err := s.UpdateProjectItemTextField(proj.id, itemID, field.ID, "landing"); err != nil {
		t.Fatal(err)
	}
	if got := s.TextFieldValue("o/r", 7, "Fabrik"); got != "landing" {
		t.Errorf("value = %q", got)
	}
	probe, _, err := s.ProbeProjectBoard("o", "r", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	// The write bumped the card's updatedAt, but the probe discounts it: the
	// effective time is the issue's own (seeded a minute earlier).
	if !probe[0].EffectiveUpdatedAt.Before(clk.now) {
		t.Errorf("EffectiveUpdatedAt = %v, want before the display write at %v", probe[0].EffectiveUpdatedAt, clk.now)
	}

	if err := s.ClearProjectItemField(proj.id, itemID, field.ID); err != nil {
		t.Fatal(err)
	}
	if got := s.TextFieldValue("o/r", 7, "Fabrik"); got != "" {
		t.Errorf("value after clear = %q", got)
	}
	if err := s.UpdateProjectItemTextField(proj.id, itemID, "bogus", "x"); err == nil {
		t.Error("want an error for a foreign field ID")
	}
}
