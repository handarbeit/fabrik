package boardcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// Identifiers carried by the testdata/webhooks fixtures (#2345).
const (
	fixtureOwnProject = "PVT_own"
	fixtureStatusFld  = "PVTSSF_status"
	fixtureOwnItemID  = "PVTI_own1"
	fixtureForeignID  = "PVTI_foreign1"
)

func loadWebhookFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", "projects_v2_item_"+name+".json"))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

// asOurItem rewrites a foreign fixture so the foreign event names OUR item ID —
// the case that would resolve through itemIDToKey and consume an echo entry if
// the filter ran late.
func asOurItem(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), fixtureForeignID, fixtureOwnItemID))
}

type filterEnv struct {
	c       *CacheImpl
	mc      *mockClient
	logs    *[]string
	changes *[]itemstate.Change
	mu      *sync.Mutex
}

func newFilterEnv(t *testing.T) *filterEnv {
	t.Helper()
	var mu sync.Mutex
	var logs []string
	var changes []itemstate.Change
	mc := &mockClient{itemDetailsResult: &gh.ProjectItem{Number: 1, Repo: "owner/repo", Title: "T"}}
	store := itemstate.NewStore(nil)
	c := NewCacheImpl(mc, store, func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	c.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_issue1", ItemID: fixtureOwnItemID, Number: 1, Repo: "owner/repo", Status: "Validate"},
	}, fixtureOwnProject)
	c.SetStatusFieldID(fixtureStatusFld)
	store.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		changes = append(changes, ch)
		mu.Unlock()
	}))
	return &filterEnv{c: c, mc: mc, logs: &logs, changes: &changes, mu: &mu}
}

func (e *filterEnv) ignoredLogs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, l := range *e.logs {
		if strings.Contains(l, "projects_v2_item") && strings.Contains(l, "ignored") {
			n++
		}
	}
	return n
}

func (e *filterEnv) assertUntouched(t *testing.T) {
	t.Helper()
	snap, err := e.c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("item missing after ignored event: %v", err)
	}
	s := snap.State()
	if s.ItemID != fixtureOwnItemID || s.Status != "Validate" {
		t.Fatalf("cached item changed: ItemID=%q Status=%q", s.ItemID, s.Status)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(*e.changes) != 0 {
		t.Fatalf("ignored event produced %d store change(s)", len(*e.changes))
	}
}

// FR-001/FR-005: foreign-project events change nothing, make no fetch and log
// exactly one line each — including variants that carry OUR item ID.
func TestProjectsV2ItemForeignProjectIsIgnored(t *testing.T) {
	for _, name := range []string{"created_foreign", "edited_status_foreign", "archived_foreign"} {
		for _, ours := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ourItemID=%v", name, ours), func(t *testing.T) {
				e := newFilterEnv(t)
				var echoes int
				e.c.SetMatchEchoFn(func(kind, action, id string) { echoes++ })
				payload := loadWebhookFixture(t, name)
				if ours {
					payload = asOurItem(payload)
				}
				e.c.ApplyDelta("projects_v2_item", payload)
				e.assertUntouched(t)
				if e.mc.fetchItemDetailsCount != 0 {
					t.Errorf("FetchItemDetails calls = %d, want 0", e.mc.fetchItemDetailsCount)
				}
				if echoes != 0 {
					t.Errorf("foreign event consumed an echo entry (%d match calls)", echoes)
				}
				if n := e.ignoredLogs(); n != 1 {
					t.Errorf("ignored-event log lines = %d, want 1", n)
				}
			})
		}
	}
}

// FR-002: a non-Status field edit on our own board changes nothing.
func TestProjectsV2ItemNonStatusEditIsIgnored(t *testing.T) {
	for _, name := range []string{"edited_priority_own", "edited_text_own"} {
		t.Run(name, func(t *testing.T) {
			e := newFilterEnv(t)
			var echoes int
			e.c.SetMatchEchoFn(func(kind, action, id string) { echoes++ })
			e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, name))
			e.assertUntouched(t)
			if echoes != 0 {
				t.Errorf("non-Status edit consumed an echo entry")
			}
			if n := e.ignoredLogs(); n != 1 {
				t.Errorf("ignored-event log lines = %d, want 1", n)
			}
		})
	}
}

// FR-006 / SC-003: our own Status edit, with correct IDs, still applies.
func TestProjectsV2ItemOwnStatusEditStillApplies(t *testing.T) {
	e := newFilterEnv(t)
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_own"))
	snap, err := e.c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.State().Status; got != "Implement" {
		t.Fatalf("Status = %q, want Implement", got)
	}
	if n := e.ignoredLogs(); n != 0 {
		t.Errorf("applied event logged %d ignored lines", n)
	}
}

// FR-003: events missing the identifying IDs are not applied as Status changes.
func TestProjectsV2ItemMissingIDsAreNotApplied(t *testing.T) {
	for _, name := range []string{"edited_no_ids", "edited_no_field_id", "created_no_ids"} {
		t.Run(name, func(t *testing.T) {
			e := newFilterEnv(t)
			e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, name))
			e.assertUntouched(t)
			if e.mc.fetchItemDetailsCount != 0 {
				t.Errorf("FetchItemDetails calls = %d, want 0", e.mc.fetchItemDetailsCount)
			}
			if n := e.ignoredLogs(); n != 1 {
				t.Errorf("ignored-event log lines = %d, want 1", n)
			}
		})
	}
}

// A top-level project_node_id is accepted as well as projects_v2_item's own.
func TestProjectsV2ItemTopLevelProjectNodeID(t *testing.T) {
	e := newFilterEnv(t)
	var m map[string]any
	if err := json.Unmarshal(loadWebhookFixture(t, "edited_status_own"), &m); err != nil {
		t.Fatal(err)
	}
	delete(m["projects_v2_item"].(map[string]any), "project_node_id")
	m["project_node_id"] = fixtureOwnProject
	b, _ := json.Marshal(m)
	e.c.ApplyDelta("projects_v2_item", b)
	snap, _ := e.c.store.Get("owner/repo", 1)
	if got := snap.State().Status; got != "Implement" {
		t.Fatalf("Status = %q, want Implement", got)
	}
}

// Unknown own project ID: nothing is trusted.
func TestProjectsV2ItemUnknownOwnProjectIsIgnored(t *testing.T) {
	e := newFilterEnv(t)
	e.c.mu.Lock()
	e.c.projectID = ""
	e.c.mu.Unlock()
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_own"))
	e.assertUntouched(t)
}

// Unknown Status field ID is resolved lazily from the fallback client; a failed
// resolution drops the event.
func TestProjectsV2ItemStatusFieldResolvedLazily(t *testing.T) {
	e := newFilterEnv(t)
	e.c.mu.Lock()
	e.c.statusFieldID = ""
	e.c.mu.Unlock()
	// mockClient reports field "SF1", which is not the fixture's field.
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_own"))
	e.assertUntouched(t)
	if got := e.c.StatusFieldID(); got != "SF1" {
		t.Fatalf("lazily resolved StatusFieldID = %q, want SF1", got)
	}

	// With the right ID resolved the same edit applies.
	e2 := newFilterEnv(t)
	e2.c.mu.Lock()
	e2.c.statusFieldID = ""
	e2.c.mu.Unlock()
	e2.c.fallback = &statusFieldClient{mockClient: e2.mc, fieldID: fixtureStatusFld}
	e2.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_own"))
	snap, _ := e2.c.store.Get("owner/repo", 1)
	if got := snap.State().Status; got != "Implement" {
		t.Fatalf("Status = %q, want Implement after lazy resolve", got)
	}
}

type statusFieldClient struct {
	*mockClient
	fieldID string
}

func (s *statusFieldClient) FetchStatusField(string) (*gh.StatusField, error) {
	return &gh.StatusField{FieldID: s.fieldID}, nil
}

func TestProjectsV2ItemStatusFieldLookupFailureDropsEvent(t *testing.T) {
	e := newFilterEnv(t)
	e.c.mu.Lock()
	e.c.statusFieldID = ""
	e.c.mu.Unlock()
	e.c.fallback = &failingStatusFieldClient{mockClient: e.mc}
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_own"))
	e.assertUntouched(t)
}

type failingStatusFieldClient struct{ *mockClient }

func (f *failingStatusFieldClient) FetchStatusField(string) (*gh.StatusField, error) {
	return nil, fmt.Errorf("boom")
}

// Non-vacuity: with the filters neutralised the same foreign and non-Status
// events do corrupt the cache, so the tests above pin the filters.
func TestProjectsV2ItemFilterNeutralisationCorrupts(t *testing.T) {
	t.Run("foreign created fetches and blanks Status", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.SetProjectFieldFilterDisabledForTest(true)
		e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "created_foreign"))
		if e.mc.fetchItemDetailsCount == 0 {
			t.Fatal("neutralised filter should have fetched item details for the foreign created event")
		}
		// R4 keeps the item identity even unfiltered; the Status blanking is the
		// remaining damage the project filter prevents.
		snap, _ := e.c.store.Get("owner/repo", 1)
		if got := snap.State().Status; got != "" {
			t.Fatalf("Status = %q, want blanked (unfiltered behaviour)", got)
		}
	})
	t.Run("non-Status edit changes Status", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.SetProjectFieldFilterDisabledForTest(true)
		e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_priority_own"))
		snap, _ := e.c.store.Get("owner/repo", 1)
		if got := snap.State().Status; got != "P1" {
			t.Fatalf("Status = %q, want P1 (unfiltered behaviour)", got)
		}
	})
	t.Run("foreign archived removes our item", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.SetProjectFieldFilterDisabledForTest(true)
		e.c.ApplyDelta("projects_v2_item", asOurItem(loadWebhookFixture(t, "archived_foreign")))
		if _, err := e.c.store.Get("owner/repo", 1); err == nil {
			// Get may fall back; check the index directly.
			if _, ok := e.c.GetItemID(itemKey("owner/repo", 1)); ok {
				t.Fatal("expected unfiltered foreign archived to remove our item")
			}
		}
	})
}

// FR-008: every relied-on field sits where the decoder reads it, in every
// own-project fixture.
func TestProjectsV2ItemPayloadContract(t *testing.T) {
	type spec struct {
		name  string
		paths [][]string
	}
	base := [][]string{{"action"}, {"projects_v2_item", "id"}, {"projects_v2_item", "project_node_id"}, {"projects_v2_item", "content_node_id"}, {"projects_v2_item", "content_type"}}
	edited := append(append([][]string{}, base...), []string{"changes", "field_value", "field_node_id"}, []string{"changes", "field_value", "field_type"}, []string{"changes", "field_value", "to"})
	specs := []spec{
		{"created_own", base}, {"created_foreign", base},
		{"archived_own", base}, {"archived_foreign", base},
		{"edited_status_own", edited}, {"edited_status_foreign", edited},
		{"edited_priority_own", edited}, {"edited_text_own", edited},
	}
	for _, sp := range specs {
		t.Run(sp.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(loadWebhookFixture(t, sp.name), &m); err != nil {
				t.Fatal(err)
			}
			for _, path := range sp.paths {
				var cur any = m
				for _, k := range path {
					obj, ok := cur.(map[string]any)
					if !ok {
						t.Fatalf("path %v: %v is not an object", path, k)
					}
					if cur, ok = obj[k]; !ok {
						t.Fatalf("fixture lacks %s", strings.Join(path, "."))
					}
				}
			}
			// The struct the handler decodes into must see the same values.
			var p projectsV2ItemPayload
			if err := json.Unmarshal(loadWebhookFixture(t, sp.name), &p); err != nil {
				t.Fatal(err)
			}
			if p.Action == "" || p.ProjectsV2Item.ID == "" || p.effectiveProjectNodeID() == "" {
				t.Fatalf("decoder missed relied-on fields: %+v", p)
			}
			if strings.HasPrefix(sp.name, "edited") && p.Changes.FieldValue.FieldNodeID == "" {
				t.Fatal("decoder missed changes.field_value.field_node_id")
			}
		})
	}
}
