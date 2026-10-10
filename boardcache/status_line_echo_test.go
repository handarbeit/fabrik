package boardcache

// Display-only status-line field (#2048, ADR 2048): the engine writes a text
// field on every project item it touches, and every write comes back as a
// projects_v2_item "edited" webhook. These tests pin that such an echo — and
// the updatedAt bump the write itself causes — is invisible to the cache: no
// Store change (so no wake), no local-delta stamp (so no invalidation), no
// drift count and no deep fetch.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// textEditPayloads are the plausible shapes of a text-field edit delivery. The
// "to" value of a text field is not the {id,name} object a single_select edit
// carries (the exact shape is unverified), so the handler must tolerate all of
// them without an unmarshal error.
func textEditPayloads(itemID string) map[string][]byte {
	mk := func(to string) []byte {
		return []byte(fmt.Sprintf(`{"action":"edited","projects_v2_item":{"id":%q},`+
			`"changes":{"field_value":{"field_type":"text","to":%s,"from":null}}}`, itemID, to))
	}
	return map[string][]byte{
		"string to":  mk(`"queued · batch of 3"`),
		"null to":    mk(`null`),
		"object to":  mk(`{"id":"x","name":"landing"}`),
		"absent to":  []byte(fmt.Sprintf(`{"action":"edited","projects_v2_item":{"id":%q},"changes":{"field_value":{"field_type":"text"}}}`, itemID)),
		"number to":  mk(`42`),
		"clear (to)": mk(`""`),
	}
}

// checkTextEditIsNoOp applies every text-edit shape to a seeded cache and
// returns a description of each way it was NOT a no-op.
func checkTextEditIsNoOp(t *testing.T, c *CacheImpl, changes *[]itemstate.Change, mu *sync.Mutex, logs *[]string) []string {
	t.Helper()
	var problems []string
	before := testGetState(t, c, "owner/repo", 1)

	var echoes int
	c.SetMatchEchoFn(func(kind, action, id string) { echoes++ })

	for name, payload := range textEditPayloads("PVTI_001") {
		c.ApplyDelta("projects_v2_item", payload)
		if s := testGetState(t, c, "owner/repo", 1); s.Status != before.Status {
			problems = append(problems, fmt.Sprintf("%s: Status changed %q -> %q", name, before.Status, s.Status))
		}
		_ = name
	}

	mu.Lock()
	if len(*changes) != 0 {
		problems = append(problems, fmt.Sprintf("Store emitted %d change(s) (each would wake the poll)", len(*changes)))
	}
	for _, l := range *logs {
		if strings.Contains(l, "unmarshal") {
			problems = append(problems, "unmarshal error logged: "+strings.TrimSpace(l))
		}
	}
	mu.Unlock()

	c.mu.RLock()
	if n := len(c.localDeltaAt); n != 0 {
		problems = append(problems, fmt.Sprintf("localDeltaAt stamped for %d item(s) (invalidates the item)", n))
	}
	c.mu.RUnlock()

	if want := len(textEditPayloads("")); echoes != want {
		problems = append(problems, fmt.Sprintf("echo match ran %d times, want %d (the echo must still be matched exactly once per delivery)", echoes, want))
	}
	return problems
}

func seededCacheWithObserver(t *testing.T) (*CacheImpl, *[]itemstate.Change, *sync.Mutex, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var changes []itemstate.Change
	var logs []string
	store := itemstate.NewStore(nil)
	c := NewCacheImpl(&mockClient{}, store, func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	})
	c.BootstrapFromProbe([]gh.BoardProbeItem{
		{ContentID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo", Status: "Research"},
	}, "PID")
	// Subscribe after bootstrap so only delta-driven changes are counted.
	store.Subscribe(itemstate.ObserverFunc(func(ch itemstate.Change, _ itemstate.Snapshot) {
		mu.Lock()
		changes = append(changes, ch)
		mu.Unlock()
	}))
	return c, &changes, &mu, &logs
}

func TestDeltaProjectsV2ItemTextEditIsNoOp(t *testing.T) {
	c, changes, mu, logs := seededCacheWithObserver(t)
	if problems := checkTextEditIsNoOp(t, c, changes, mu, logs); len(problems) != 0 {
		t.Fatalf("a display-field edit must be a no-op:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestDeltaProjectsV2ItemTextEditIsNoOp_NeutralisationFails shows the test above
// has teeth: with the suppression neutralised, the same check reports the text
// being written into Status (and the wake/invalidation that follows).
func TestDeltaProjectsV2ItemTextEditIsNoOp_NeutralisationFails(t *testing.T) {
	c, changes, mu, logs := seededCacheWithObserver(t)
	c.SetTextEditSuppressionDisabledForTest(true)
	problems := checkTextEditIsNoOp(t, c, changes, mu, logs)
	if len(problems) == 0 {
		t.Fatal("with the echo suppression neutralised the no-op check still passed — it does not pin the suppression")
	}
	var statusCorrupted bool
	for _, p := range problems {
		if strings.Contains(p, "Status changed") {
			statusCorrupted = true
		}
	}
	if !statusCorrupted {
		t.Errorf("expected the neutralised handler to write display text into Status; problems: %v", problems)
	}
}

// TestDeltaProjectsV2ItemSingleSelectStillApplies guards the other direction:
// the tolerant payload decode must not break a real Status edit.
func TestDeltaProjectsV2ItemSingleSelectStillApplies(t *testing.T) {
	c, changes, mu, _ := seededCacheWithObserver(t)
	c.ApplyDelta("projects_v2_item", []byte(`{"action":"edited","projects_v2_item":{"id":"PVTI_001"},`+
		`"changes":{"field_value":{"field_type":"single_select","to":{"id":"o1","name":"Plan"}}}}`))
	if s := testGetState(t, c, "owner/repo", 1); s.Status != "Plan" {
		t.Errorf("Status = %q, want Plan", s.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*changes) == 0 {
		t.Error("a Status edit must still reach the Store")
	}
}

// statusLineBoardServer serves a one-item project board whose item updatedAt was
// bumped by a display-field write (item updatedAt == the field value's own
// updatedAt, later than the issue's). Like GitHub it returns the statusLine
// selection only when the query asked for it (withStatusLine).
func statusLineBoardServer(t *testing.T, issueAt, itemAt time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		node := map[string]interface{}{
			"id":               "PVTI_001",
			"updatedAt":        itemAt.Format(time.RFC3339),
			"fieldValueByName": map[string]interface{}{"name": "Research"},
			"content": map[string]interface{}{
				"__typename": "Issue", "id": "I_001", "number": 1, "title": "t", "state": "OPEN",
				"updatedAt":  issueAt.Format(time.RFC3339),
				"repository": map[string]interface{}{"nameWithOwner": "owner/repo"},
				"labels":     map[string]interface{}{"nodes": []interface{}{}},
			},
		}
		if with, _ := body.Variables["withStatusLine"].(bool); with {
			node["statusLine"] = map[string]interface{}{"updatedAt": itemAt.Format(time.RFC3339)}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
			"organization": map[string]interface{}{"projectV2": map[string]interface{}{
				"id": "PID", "title": "Board",
				"items": map[string]interface{}{
					"totalCount": 1,
					"pageInfo":   map[string]interface{}{"hasNextPage": false, "endCursor": ""},
					"nodes":      []interface{}{node},
				},
			}},
		}})
	}))
}

func lightReconcileAgainst(t *testing.T, configure func(*gh.Client)) int {
	t.Helper()
	issueAt := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	itemAt := issueAt.Add(time.Hour) // the display write's bump
	srv := statusLineBoardServer(t, issueAt, itemAt)
	defer srv.Close()

	client := gh.NewClientWithBaseURL("token", srv.URL)
	configure(client)

	c := NewCacheImpl(&mockClient{}, itemstate.NewStore(nil), nopLog)
	// The cache holds the item as of before the display write.
	testBootstrapFromBoard(c, &gh.ProjectBoard{
		ProjectID: "PID",
		Items: []gh.ProjectItem{{
			ID: "I_001", ItemID: "PVTI_001", Number: 1, Repo: "owner/repo",
			Status: "Research", UpdatedAt: issueAt,
		}},
	})
	c.fallback = client

	drift, keys, _, err := c.LightReconcile("owner", "repo", 1, "organization")
	if err != nil {
		t.Fatalf("LightReconcile: %v (keys %v)", err, keys)
	}
	return drift
}

// TestLightReconcile_DisplayFieldBumpIsNotDrift: the project item's updatedAt
// moved only because Fabrik wrote its status line, so the reconcile drift
// check sees no change (which would otherwise cost a full Pause/Reconcile/
// Resume cycle and flip webhook health per write burst).
func TestLightReconcile_DisplayFieldBumpIsNotDrift(t *testing.T) {
	if got := lightReconcileAgainst(t, func(c *gh.Client) { c.SetStatusLineField("Fabrik") }); got != 0 {
		t.Fatalf("drift = %d, want 0 for a display-field-only updatedAt bump", got)
	}
}

// TestLightReconcile_DisplayFieldBumpIsNotDrift_NeutralisationFails: without the
// client-side discount the same bump is counted as drift — proof the test
// above pins the discount and not a coincidence.
func TestLightReconcile_DisplayFieldBumpIsNotDrift_NeutralisationFails(t *testing.T) {
	if got := lightReconcileAgainst(t, func(*gh.Client) {}); got != 1 {
		t.Fatalf("drift = %d, want 1 when the discount is neutralised (feature off)", got)
	}
}

// TestDeltaProjectsV2ItemDateEditIsNoOp pins that an edit of the "Last
// activity" date field (#2049) is as invisible to the cache as a text edit:
// the guard keys on field_type != single_select, not on text.
func TestDeltaProjectsV2ItemDateEditIsNoOp(t *testing.T) {
	c, changes, mu, logs := seededCacheWithObserver(t)
	before := testGetState(t, c, "owner/repo", 1)

	payloads := map[string]string{
		"string to": `"2026-10-10"`,
		"null to":   `null`,
		"object to": `{"date":"2026-10-10"}`,
	}
	for name, to := range payloads {
		p := []byte(fmt.Sprintf(`{"action":"edited","projects_v2_item":{"id":"PVTI_001"},`+
			`"changes":{"field_value":{"field_type":"date","to":%s,"from":null}}}`, to))
		c.ApplyDelta("projects_v2_item", p)
		if s := testGetState(t, c, "owner/repo", 1); s.Status != before.Status {
			t.Errorf("%s: Status changed %q -> %q", name, before.Status, s.Status)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*changes) != 0 {
		t.Errorf("Store emitted %d change(s) (each would wake the poll)", len(*changes))
	}
	for _, l := range *logs {
		if strings.Contains(l, "unmarshal") {
			t.Errorf("unmarshal error logged: %s", strings.TrimSpace(l))
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if n := len(c.localDeltaAt); n != 0 {
		t.Errorf("localDeltaAt stamped for %d item(s) (invalidates the item)", n)
	}
}
