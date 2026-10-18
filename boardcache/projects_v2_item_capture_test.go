package boardcache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests below feed verbatim captured projects_v2_item deliveries (#2383)
// through the real ApplyDelta path.

// SC-002 / FR-009: each own-board Status edit changes the cached Status with no
// probe or reconcile involved. This is the regression test: it fails when the
// item ID is read from the numeric projects_v2_item.id as text, because the
// whole delivery then fails to decode (see
// TestCapturedDeliveryFailsToDecodeWithStringID).
func TestCapturedStatusEditAppliesToStore(t *testing.T) {
	e := newFilterEnv(t)
	var echoIDs []string
	e.c.SetMatchEchoFn(func(kind, action, id string) {
		if kind == "projects_v2_item" && action == "edited" {
			echoIDs = append(echoIDs, id)
		}
	})

	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_set"))
	if got := e.status(t); got != "Backlog" {
		t.Fatalf("after first edit Status = %q, want Backlog", got)
	}
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_change"))
	if got := e.status(t); got != "Specify" {
		t.Fatalf("after second edit Status = %q, want Specify", got)
	}

	if n := e.ignoredLogs(); n != 0 {
		t.Errorf("applied events logged %d ignored lines", n)
	}
	if n := e.countLogs("unmarshal"); n != 0 {
		t.Errorf("applied events logged %d unmarshal errors", n)
	}
	if len(echoIDs) != 2 || echoIDs[0] != fixtureOwnItemID || echoIDs[1] != fixtureOwnItemID {
		t.Errorf("echo registry keys = %v, want the PVTI_ node ID twice", echoIDs)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(*e.changes) != 2 {
		t.Fatalf("store changes = %d, want 2", len(*e.changes))
	}
	for _, ch := range *e.changes {
		if ch.Sender != "octocat" {
			t.Errorf("Change.Sender = %q, want the scrubbed sender login", ch.Sender)
		}
	}
}

// FR-007: a Status clear (an edit with no from/to) is a named rule: the cached
// Status stays, the clear is logged by name (not as a filter `ignored` drop)
// and the echo entry is still consumed.
func TestCapturedStatusClearLeavesStatusToReconcile(t *testing.T) {
	e := newFilterEnv(t)
	var echoes int
	e.c.SetMatchEchoFn(func(kind, action, id string) { echoes++ })
	e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "edited_status_clear"))
	e.assertUntouched(t)
	if n := e.countLogs("Status clear received for "+fixtureOwnItemID, "left to reconcile"); n != 1 {
		t.Errorf("Status-clear log lines = %d, want 1", n)
	}
	if n := e.ignoredLogs(); n != 0 {
		t.Errorf("Status clear counted as ignored by the filters (%d)", n)
	}
	if echoes != 1 {
		t.Errorf("echo match calls = %d, want 1", echoes)
	}
}

// A Status edit whose "to" is present but not the {id,name} object is also left
// to reconcile and named as such.
func TestStatusEditWithUnreadableNewValueLeavesStatusToReconcile(t *testing.T) {
	e := newFilterEnv(t)
	b := deriveFixture(t, "edited_status_set", func(m map[string]any) {
		m["changes"].(map[string]any)["field_value"].(map[string]any)["to"] = "Backlog"
	})
	e.c.ApplyDelta("projects_v2_item", b)
	e.assertUntouched(t)
	if n := e.countLogs("unreadable new value"); n != 1 {
		t.Errorf("unreadable-value log lines = %d, want 1", n)
	}
}

// User story 3: lifecycle deliveries.
func TestCapturedLifecycleDeliveries(t *testing.T) {
	for _, name := range []string{"archived", "deleted"} {
		t.Run(name+" removes the item", func(t *testing.T) {
			e := newFilterEnv(t)
			e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, name))
			if _, ok := e.c.GetItemID(itemKey("owner/repo", 1)); ok {
				t.Fatal("item still cached after " + name)
			}
			if n := e.ignoredLogs(); n != 0 {
				t.Errorf("logged %d ignored lines", n)
			}
		})
	}
	t.Run("restored changes nothing", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "restored"))
		e.assertUntouched(t)
	})
	t.Run("created draft is ignored without a fetch", func(t *testing.T) {
		e := newFilterEnv(t)
		e.c.ApplyDelta("projects_v2_item", loadWebhookFixture(t, "created_draft"))
		e.assertUntouched(t)
		if e.mc.fetchItemDetailsCount != 0 {
			t.Errorf("FetchItemDetails calls = %d, want 0", e.mc.fetchItemDetailsCount)
		}
	})
}

// FR-003 / story 4: a mistyped field the delta does not use cannot drop the
// event; a mistyped field it does use is logged and ignored.
func TestMistypedFieldsDoNotDropTheEvent(t *testing.T) {
	t.Run("unused fields", func(t *testing.T) {
		e := newFilterEnv(t)
		b := deriveFixture(t, "edited_status_set", func(m map[string]any) {
			itemBlock(m)["id"] = "275291682" // a string, not a number
			itemBlock(m)["updated_at"] = 1789746633
			itemBlock(m)["created_at"] = false
			m["sender"].(map[string]any)["login"] = 82237
			m["changes"].(map[string]any)["field_value"].(map[string]any)["field_name"] = []any{"Status"}
			m["organization"] = "example-org"
		})
		e.c.ApplyDelta("projects_v2_item", b)
		if got := e.status(t); got != "Backlog" {
			t.Fatalf("Status = %q, want Backlog", got)
		}
	})
	for _, field := range []string{"node_id", "project_node_id"} {
		t.Run("relied-on "+field, func(t *testing.T) {
			e := newFilterEnv(t)
			b := deriveFixture(t, "edited_status_set", func(m map[string]any) {
				itemBlock(m)[field] = 42
			})
			e.c.ApplyDelta("projects_v2_item", b)
			e.assertUntouched(t)
			if n := e.ignoredLogs(); n != 1 {
				t.Errorf("ignored-event log lines = %d, want 1", n)
			}
		})
	}
}

// FR-009 non-vacuity: decoding a capture with the pre-fix declaration (the
// numeric id read as a string) fails the whole event. This also keeps proving
// the captures really carry a numeric id.
func TestCapturedDeliveryFailsToDecodeWithStringID(t *testing.T) {
	var old struct {
		ProjectsV2Item struct {
			ID            string `json:"id"`
			ProjectNodeID string `json:"project_node_id"`
		} `json:"projects_v2_item"`
	}
	err := json.Unmarshal(loadWebhookFixture(t, "edited_status_set"), &old)
	if err == nil || !strings.Contains(err.Error(), "cannot unmarshal number into Go struct field") {
		t.Fatalf("old string-typed id decode error = %v, want a number-into-string error", err)
	}
	// And the real decoder accepts the same bytes.
	var p projectsV2ItemPayload
	if err := json.Unmarshal(loadWebhookFixture(t, "edited_status_set"), &p); err != nil {
		t.Fatal(err)
	}
}

// FR-008 / SC-005: the fixtures are scrubbed captures with no placeholder
// notes and no personal or organisational identity left.
func TestWebhookFixturesAreScrubbedCaptures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "webhooks", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures (err %v)", err)
	}
	banned := []string{"verveguy", "82237", "handarbeit", "avatars.githubusercontent.com", "api.github.com", "installation", "verbatim capture", "_note"}
	for _, f := range files {
		if strings.HasSuffix(f, "README.md") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b := string(raw)
		for _, s := range banned {
			if strings.Contains(strings.ToLower(b), strings.ToLower(s)) {
				t.Errorf("%s contains %q", filepath.Base(f), s)
			}
		}
	}
}
