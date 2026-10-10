package simgh

import (
	"testing"
	"time"
)

func TestDateFieldRoundTripAndDiscount(t *testing.T) {
	clk := &steppedClock{now: time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)}
	s := New(t.TempDir(), WithClock(clk), WithLastActivityField("Last activity"), WithLastRunField("Last run"))
	s.SeedRepo("o/r")
	s.SeedProject("o", 1, "Board", []string{"Backlog", "Queued"})
	s.SeedDateField("o", 1, "Last activity")
	s.SeedTextField("o", 1, "Last run")
	s.SeedIssue("o/r", IssueSeed{Number: 7, Title: "t", Body: "b"})
	s.SeedProjectItem("o", 1, "o/r", 7, false, "Queued")
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}

	proj := s.defaultProject
	field, err := s.FetchDateField(proj.id, "Last activity")
	if err != nil || field == nil {
		t.Fatalf("FetchDateField = %v, %v", field, err)
	}
	if f, _ := s.FetchDateField(proj.id, "Last run"); f != nil {
		t.Fatalf("a text field must not read as a date field, got %+v", f)
	}
	if f, _ := s.FetchTextField(proj.id, "Last activity"); f != nil {
		t.Fatalf("a date field must not read as a text field, got %+v", f)
	}
	var itemID string
	for id := range proj.items {
		itemID = id
	}

	clk.now = clk.now.Add(time.Minute)
	if err := s.UpdateProjectItemDateField(proj.id, itemID, field.ID, "2026-10-10"); err != nil {
		t.Fatal(err)
	}
	if got := s.DateFieldValue("o/r", 7, "Last activity"); got != "2026-10-10" {
		t.Errorf("value = %q", got)
	}
	probe, _, err := s.ProbeProjectBoard("o", "r", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if !probe[0].EffectiveUpdatedAt.Before(clk.now) {
		t.Errorf("EffectiveUpdatedAt = %v, want before the date write at %v (discounted)", probe[0].EffectiveUpdatedAt, clk.now)
	}

	if err := s.UpdateProjectItemDateField(proj.id, itemID, field.ID, "10/10/2026"); err == nil {
		t.Error("want an error for a non-YYYY-MM-DD date")
	}
	if err := s.UpdateProjectItemDateField(proj.id, itemID, "bogus", "2026-10-10"); err == nil {
		t.Error("want an error for a foreign field ID")
	}
}
