package itemstate

import (
	"testing"

	gh "github.com/handarbeit/fabrik/github"
)

// R4 (#2345): an incremental apply never replaces a known item ID with an
// empty or different one; only the authoritative listing may re-identify.
func TestApplyProjectItem_ItemIDNeverEmptiedOrReplaced(t *testing.T) {
	tests := []struct {
		name     string
		known    string
		incoming string
		auth     bool
		want     string
	}{
		{"empty kept", "PVTI_own", "", false, "PVTI_own"},
		{"foreign kept", "PVTI_own", "PVTI_foreign", false, "PVTI_own"},
		{"adopted when unknown", "", "PVTI_own", false, "PVTI_own"},
		{"authoritative replaces known", "PVTI_old", "PVTI_new", true, "PVTI_new"},
		{"authoritative never empties", "PVTI_own", "", true, "PVTI_own"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			item := &ItemState{Repo: "o/r", Number: 1, ItemID: tc.known}
			pi := gh.ProjectItem{Repo: "o/r", Number: 1, ItemID: tc.incoming}
			if tc.auth {
				applyProjectItemAuthoritative(item, pi)
			} else {
				applyProjectItem(item, pi)
			}
			if item.ItemID != tc.want {
				t.Fatalf("ItemID = %q, want %q", item.ItemID, tc.want)
			}
		})
	}
}

func TestStore_IssueOpenedKeepsKnownItemID(t *testing.T) {
	s := NewStore(nil)
	s.Reset([]gh.ProjectItem{{Repo: "o/r", Number: 1, ItemID: "PVTI_own", Status: "Validate"}})
	s.Apply(IssueOpened{Item: gh.ProjectItem{Repo: "o/r", Number: 1, ItemID: "PVTI_foreign"}})
	snap, err := s.Get("o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.State().ItemID; got != "PVTI_own" {
		t.Fatalf("ItemID = %q, want PVTI_own", got)
	}
}
