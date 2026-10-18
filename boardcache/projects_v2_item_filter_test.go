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

// Identifiers carried by the captured testdata/webhooks deliveries (#2383).
// The bed board is project #2; board1 is project #1, whose Labels edits were
// captured too.
const (
	fixtureOwnProject    = "PVT_kwDOENB0Ac4BYraz"
	fixtureStatusFld     = "PVTSSF_lADOENB0Ac4BYrazzhTven4"
	fixtureOtherFld      = "PVTSSF_lADOENB0Ac4BYrazzhmLQ-A"
	fixtureOwnItemID     = "PVTI_lADOENB0Ac4BYrazzhBoniI"
	fixtureBoard1Project = "PVT_kwDOENB0Ac4BW0n_"
	fixtureBoard1ItemID  = "PVTI_lADOENB0Ac4BW0n_zhBog4Q"
	fixtureLabelsFld     = "PVTF_lADOENB0Ac4BW0n_zhSF0w0"
	// board1's Status field ID is not in any capture (no Status edit was
	// recorded on board #1); the env needs one so the Labels field is compared
	// against a known Status field. It only configures the cache.
	board1StatusFld = "PVTSSF_board1_status_config_only"
)

func loadWebhookFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", "projects_v2_item_"+name+".json"))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

// deriveFixture builds a shape no delivery was captured for by mutating a
// capture. Derived shapes live in test code, never on disk, so the fixture
// directory holds only verbatim (scrubbed) captures.
func deriveFixture(t *testing.T, name string, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(loadWebhookFixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func itemBlock(m map[string]any) map[string]any { return m["projects_v2_item"].(map[string]any) }

type filterEnv struct {
	c       *CacheImpl
	mc      *mockClient
	logs    *[]string
	changes *[]itemstate.Change
	mu      *sync.Mutex
	itemID  string
}

// newFilterEnv is the bed board (#2) with the captured draft item's ID seeded as
// issue #1 in status Validate.
func newFilterEnv(t *testing.T) *filterEnv {
	return newEnvFor(t, fixtureOwnProject, fixtureOwnItemID, fixtureStatusFld)
}

// newBoard1Env is board #1, so its captured Labels edits reach the field filter.
func newBoard1Env(t *testing.T) *filterEnv {
	return newEnvFor(t, fixtureBoard1Project, fixtureBoard1ItemID, board1StatusFld)
}

func newEnvFor(t *testing.T, project, itemID, statusField string) *filterEnv {
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
		{ContentID: "I_issue1", ItemID: itemID, Number: 1, Repo: "owner/repo", Status: "Validate"},
	}, project)
	c.SetStatusFieldID(statusField)
	store.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		changes = append(changes, ch)
		mu.Unlock()
	}))
	return &filterEnv{c: c, mc: mc, logs: &logs, changes: &changes, mu: &mu, itemID: itemID}
}

func (e *filterEnv) countLogs(substrs ...string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
outer:
	for _, l := range *e.logs {
		for _, s := range substrs {
			if !strings.Contains(l, s) {
				continue outer
			}
		}
		n++
	}
	return n
}

func (e *filterEnv) ignoredLogs() int { return e.countLogs("projects_v2_item", "ignored") }

func (e *filterEnv) status(t *testing.T) string {
	t.Helper()
	snap, err := e.c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	return snap.State().Status
}

func (e *filterEnv) assertUntouched(t *testing.T) {
	t.Helper()
	snap, err := e.c.store.Get("owner/repo", 1)
	if err != nil {
		t.Fatalf("item missing after ignored event: %v", err)
	}
	s := snap.State()
	if s.ItemID != e.itemID || s.Status != "Validate" {
		t.Fatalf("cached item changed: ItemID=%q Status=%q", s.ItemID, s.Status)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(*e.changes) != 0 {
		t.Fatalf("ignored event produced %d store change(s)", len(*e.changes))
	}
}

// foreignVariants returns real deliveries that belong to another project from
// the bed's point of view, each also in a variant naming OUR item ID — the case
// that would resolve through itemIDToKey and consume an echo entry if the
// filter ran late.
func foreignVariants(t *testing.T) map[string][]byte {
	t.Helper()
	toForeign := func(m map[string]any) { itemBlock(m)["project_node_id"] = fixtureBoard1Project }
	ours := func(m map[string]any) { itemBlock(m)["node_id"] = fixtureOwnItemID }
	out := map[string][]byte{
		"labels_board1_a": loadWebhookFixture(t, "edited_labels_board1_a"),
		"labels_board1_b": loadWebhookFixture(t, "edited_labels_board1_b"),
		"labels_ourID":    deriveFixture(t, "edited_labels_board1_a", ours),
		"status_foreign":  deriveFixture(t, "edited_status_change", toForeign),
		"status_foreign_ourID": deriveFixture(t, "edited_status_change", func(m map[string]any) {
			toForeign(m)
			ours(m)
		}),
		"archived_foreign": deriveFixture(t, "archived", toForeign),
		"created_foreign": deriveFixture(t, "created_draft", func(m map[string]any) {
			toForeign(m)
			itemBlock(m)["content_type"] = "Issue"
			itemBlock(m)["content_node_id"] = "I_foreign"
		}),
	}
	return out
}

// FR-004/FR-006: events of another project change nothing, make no fetch, take
// no echo entry and log exactly one `ignored` line each.
func TestProjectsV2ItemForeignProjectIsIgnored(t *testing.T) {
	for name, payload := range foreignVariants(t) {
		t.Run(name, func(t *testing.T) {
			e := newFilterEnv(t)
			var echoes int
			e.c.SetMatchEchoFn(func(kind, action, id string) { echoes++ })
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
			if n := e.countLogs("foreign project"); n != 1 {
				t.Errorf("foreign-project log lines = %d, want 1", n)
			}
		})
	}
}

// FR-004/FR-006: a non-Status field edit on the event's own board changes
// nothing — a single-select field other than Status on the bed board, and the
// Labels field (the engine's own label writes) on board #1.
func TestProjectsV2ItemNonStatusEditIsIgnored(t *testing.T) {
	cases := []struct {
		name    string
		env     func(*testing.T) *filterEnv
		fixture string
		field   string
	}{
		{"other single-select on bed", newFilterEnv, "edited_other_single_select", fixtureOtherFld},
		{"labels a on board1", newBoard1Env, "edited_labels_board1_a", fixtureLabelsFld},
		{"labels b on board1", newBoard1Env, "edited_labels_board1_b", fixtureLabelsFld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.env(t)
			var echoes int
			e.c.SetMatchEchoFn(func(kind, action, id string) { echoes++ })
			e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, tc.fixture))
			e.assertUntouched(t)
			if echoes != 0 {
				t.Errorf("non-Status edit consumed an echo entry")
			}
			if n := e.ignoredLogs(); n != 1 {
				t.Errorf("ignored-event log lines = %d, want 1", n)
			}
			if n := e.countLogs("field "+tc.field+" is not the Status field", "ignored"); n != 1 {
				t.Errorf("non-Status-field log lines = %d, want 1", n)
			}
		})
	}
}

// FR-003/FR-005: deliveries missing an identifying ID are not applied.
func TestProjectsV2ItemMissingIDsAreNotApplied(t *testing.T) {
	cases := map[string][]byte{
		"edited no node_id": deriveFixture(t, "edited_status_set", func(m map[string]any) {
			delete(itemBlock(m), "node_id")
		}),
		"edited no project_node_id": deriveFixture(t, "edited_status_set", func(m map[string]any) {
			delete(itemBlock(m), "project_node_id")
		}),
		"edited no field_node_id": deriveFixture(t, "edited_status_set", func(m map[string]any) {
			delete(m["changes"].(map[string]any)["field_value"].(map[string]any), "field_node_id")
		}),
		"created no project_node_id": deriveFixture(t, "created_draft", func(m map[string]any) {
			delete(itemBlock(m), "project_node_id")
		}),
		"archived no node_id": deriveFixture(t, "archived", func(m map[string]any) {
			delete(itemBlock(m), "node_id")
		}),
		// The numeric id is never an identifier, even with node_id absent.
		"numeric id is not a fallback": deriveFixture(t, "archived", func(m map[string]any) {
			delete(itemBlock(m), "node_id")
			itemBlock(m)["id"] = 275291682
		}),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			e := newFilterEnv(t)
			e.c.ApplyDelta("projects_v2_item", payload)
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

// FR-005: no real delivery carries a top-level project_node_id, so one alone
// does not identify the project.
func TestProjectsV2ItemTopLevelProjectNodeIDIsIgnored(t *testing.T) {
	e := newFilterEnv(t)
	b := deriveFixture(t, "edited_status_set", func(m map[string]any) {
		delete(itemBlock(m), "project_node_id")
		m["project_node_id"] = fixtureOwnProject
	})
	e.c.ApplyDelta("projects_v2_item", b)
	e.assertUntouched(t)
	if n := e.countLogs("no project_node_id"); n != 1 {
		t.Errorf("no-project_node_id log lines = %d, want 1", n)
	}
}

// Unknown own project ID: nothing is trusted.
func TestProjectsV2ItemUnknownOwnProjectIsIgnored(t *testing.T) {
	e := newFilterEnv(t)
	e.c.mu.Lock()
	e.c.projectID = ""
	e.c.mu.Unlock()
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_set"))
	e.assertUntouched(t)
}

// Unknown Status field ID is resolved lazily from the fallback client; a failed
// resolution drops the event.
func TestProjectsV2ItemStatusFieldResolvedLazily(t *testing.T) {
	e := newFilterEnv(t)
	e.c.mu.Lock()
	e.c.statusFieldID = ""
	e.c.mu.Unlock()
	// mockClient reports field "SF1", which is not the capture's field.
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_set"))
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
	e2.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_set"))
	if got := e2.status(t); got != "Backlog" {
		t.Fatalf("Status = %q, want Backlog after lazy resolve", got)
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
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_set"))
	e.assertUntouched(t)
}

type failingStatusFieldClient struct{ *mockClient }

func (f *failingStatusFieldClient) FetchStatusField(string) (*gh.StatusField, error) {
	return nil, fmt.Errorf("boom")
}

// Non-vacuity: with the filters neutralised the same foreign and non-Status
// events do corrupt the cache, so the tests above pin the filters.
func TestProjectsV2ItemFilterNeutralisationCorrupts(t *testing.T) {
	variants := foreignVariants(t)
	t.Run("foreign created fetches and blanks Status", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.SetProjectFieldFilterDisabledForTest(true)
		e.c.ApplyDelta("projects_v2_item", variants["created_foreign"])
		if e.mc.fetchItemDetailsCount == 0 {
			t.Fatal("neutralised filter should have fetched item details for the foreign created event")
		}
		// The item identity is kept even unfiltered. Since #2348 (R3) the created
		// event is a partial payload that never writes an empty Status over a known
		// one, so the Status blanking this used to show is prevented there as well.
		if got := e.status(t); got != "Validate" {
			t.Fatalf("Status = %q, want kept: a partial payload carries no Status", got)
		}
	})
	t.Run("non-Status edit changes Status", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.SetProjectFieldFilterDisabledForTest(true)
		e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_other_single_select"))
		if got := e.status(t); got != "A" {
			t.Fatalf("Status = %q, want A (unfiltered behaviour)", got)
		}
	})
	t.Run("foreign archived removes our item", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.SetProjectFieldFilterDisabledForTest(true)
		e.c.ApplyDelta("projects_v2_item", variants["archived_foreign"])
		if _, ok := e.c.GetItemID(itemKey("owner/repo", 1)); ok {
			t.Fatal("expected unfiltered foreign archived to remove our item")
		}
	})
}

// Contract: every capture has the real layout the decoder relies on — a string
// node_id and project_node_id nested under projects_v2_item, a NUMERIC id, and
// no top-level project_node_id.
func TestProjectsV2ItemPayloadContract(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "webhooks", "projects_v2_item_*.json"))
	if err != nil || len(files) != 10 {
		t.Fatalf("captures = %d (err %v), want 10", len(files), err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "projects_v2_item_"), ".json")
		t.Run(name, func(t *testing.T) {
			raw := loadWebhookFixture(t, name)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if _, ok := m["project_node_id"]; ok {
				t.Error("captured delivery has a top-level project_node_id")
			}
			it := itemBlock(m)
			if _, ok := it["id"].(float64); !ok {
				t.Errorf("projects_v2_item.id = %T, want a JSON number", it["id"])
			}
			for _, k := range []string{"node_id", "project_node_id", "content_node_id", "content_type"} {
				if s, ok := it[k].(string); !ok || s == "" {
					t.Errorf("projects_v2_item.%s = %v, want a non-empty string", k, it[k])
				}
			}
			var p projectsV2ItemPayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			if p.Action == "" || p.ProjectsV2Item.NodeID == "" || p.ProjectsV2Item.ProjectNodeID == "" {
				t.Fatalf("decoder missed relied-on fields: %+v", p)
			}
			if !strings.HasPrefix(string(p.ProjectsV2Item.NodeID), "PVTI_") {
				t.Errorf("NodeID = %q, want a PVTI_ node ID", p.ProjectsV2Item.NodeID)
			}
			if strings.HasPrefix(name, "edited") && p.Changes.FieldValue.FieldNodeID == "" {
				t.Fatal("decoder missed changes.field_value.field_node_id")
			}
		})
	}
}
