package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func textFieldServer(t *testing.T, recording string, check func(query string, vars map[string]interface{})) *httptest.Server {
	t.Helper()
	raw := loadRecording(t, recording)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		vars, _ := body["variables"].(map[string]interface{})
		q, _ := body["query"].(string)
		check(q, vars)
		w.WriteHeader(200)
		w.Write(raw)
	}))
}

func TestFetchTextField_Success(t *testing.T) {
	srv := textFieldServer(t, "fetch_text_field", func(q string, vars map[string]interface{}) {
		if vars["projectId"] != "PVT_1" || vars["name"] != "Fabrik" {
			t.Errorf("vars = %v", vars)
		}
	})
	defer srv.Close()

	f, err := NewClientWithBaseURL("token", srv.URL).FetchTextField("PVT_1", "Fabrik")
	if err != nil {
		t.Fatalf("FetchTextField: %v", err)
	}
	if f == nil || f.ID == "" || f.Name != "Fabrik" {
		t.Fatalf("field = %+v", f)
	}
}

func TestFetchTextField_MissingOrNotText(t *testing.T) {
	for name, resp := range map[string]string{
		"missing field": `{"data":{"node":{"field":null}}}`,
		"no id (union)": `{"data":{"node":{"field":{}}}}`,
		"number field":  `{"data":{"node":{"field":{"id":"F","name":"Fabrik","dataType":"NUMBER"}}}}`,
		"non-project":   `{"data":{"node":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(resp))
			}))
			defer srv.Close()
			f, err := NewClientWithBaseURL("token", srv.URL).FetchTextField("PVT_1", "Fabrik")
			if err != nil {
				t.Fatalf("want nil error for an absent/non-text field, got %v", err)
			}
			if f != nil {
				t.Fatalf("want nil field, got %+v", f)
			}
		})
	}
}

func TestFetchTextField_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
	}))
	defer srv.Close()
	if _, err := NewClientWithBaseURL("token", srv.URL).FetchTextField("PVT_1", "Fabrik"); err == nil {
		t.Fatal("expected error for GraphQL error")
	}
}

func TestUpdateProjectItemTextField_Success(t *testing.T) {
	srv := textFieldServer(t, "update_project_item_text_field", func(q string, vars map[string]interface{}) {
		if !strings.Contains(q, "updateProjectV2ItemFieldValue") || !strings.Contains(q, "text: $text") {
			t.Errorf("unexpected query: %s", q)
		}
		if vars["projectId"] != "PVT_1" || vars["itemId"] != "PVTI_1" || vars["fieldId"] != "F_1" || vars["text"] != "landing" {
			t.Errorf("vars = %v", vars)
		}
	})
	defer srv.Close()
	if err := NewClientWithBaseURL("token", srv.URL).UpdateProjectItemTextField("PVT_1", "PVTI_1", "F_1", "landing"); err != nil {
		t.Fatalf("UpdateProjectItemTextField: %v", err)
	}
}

func TestUpdateProjectItemTextField_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"nope"}]}`))
	}))
	defer srv.Close()
	if err := NewClientWithBaseURL("token", srv.URL).UpdateProjectItemTextField("PVT_1", "PVTI_1", "F_1", "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestClearProjectItemField_Success(t *testing.T) {
	srv := textFieldServer(t, "clear_project_item_field", func(q string, vars map[string]interface{}) {
		if !strings.Contains(q, "clearProjectV2ItemFieldValue") {
			t.Errorf("unexpected query: %s", q)
		}
		if vars["projectId"] != "PVT_1" || vars["itemId"] != "PVTI_1" || vars["fieldId"] != "F_1" {
			t.Errorf("vars = %v", vars)
		}
	})
	defer srv.Close()
	if err := NewClientWithBaseURL("token", srv.URL).ClearProjectItemField("PVT_1", "PVTI_1", "F_1"); err != nil {
		t.Fatalf("ClearProjectItemField: %v", err)
	}
}

func TestClearProjectItemField_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"nope"}]}`))
	}))
	defer srv.Close()
	if err := NewClientWithBaseURL("token", srv.URL).ClearProjectItemField("PVT_1", "PVTI_1", "F_1"); err == nil {
		t.Fatal("expected error")
	}
}

func TestProjectItemUpdatedAt(t *testing.T) {
	field := &statusLineValue{UpdatedAt: "2026-10-10T10:00:00Z"}
	cases := []struct {
		name     string
		item     string
		line     *statusLineValue
		wantOK   bool
		wantTime string
	}{
		{"no display field behaves as a plain parse", "2026-10-10T10:00:00Z", nil, true, "2026-10-10T10:00:00Z"},
		{"bump equal to the display write is discounted", "2026-10-10T10:00:00Z", field, false, ""},
		{"bump within tolerance is discounted", "2026-10-10T10:00:01Z", field, false, ""},
		{"older than the display write is discounted", "2026-10-10T09:00:00Z", field, false, ""},
		{"later Status move still counts", "2026-10-10T10:05:00Z", field, true, "2026-10-10T10:05:00Z"},
		{"unparseable display time falls back to plain", "2026-10-10T10:00:00Z", &statusLineValue{UpdatedAt: "junk"}, true, "2026-10-10T10:00:00Z"},
		{"unparseable item time is dropped", "junk", field, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := projectItemUpdatedAt(tc.item, tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Format(time.RFC3339) != tc.wantTime {
				t.Errorf("time = %s, want %s", got.Format(time.RFC3339), tc.wantTime)
			}
		})
	}
}

// statusLineProbeServer serves one probe item whose project-item updatedAt was
// bumped by a display-field write, and records the request variables.
func statusLineProbeServer(t *testing.T, itemUpdatedAt string, withLine bool, gotVars *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		*gotVars, _ = body["variables"].(map[string]interface{})
		node := probeItemResponse("PVTI_1", "I_abc", 10, "Issue", "OPEN", "2026-10-10T09:00:00Z", itemUpdatedAt, "owner/repo", 0, "")
		if withLine {
			node["statusLine"] = map[string]interface{}{"updatedAt": "2026-10-10T10:00:00Z"}
		}
		json.NewEncoder(w).Encode(probeResponse("user", "PVT_1", []interface{}{node}, 1, false, ""))
	}))
}

func TestProbeProjectBoard_DisplayFieldBumpIsDiscounted(t *testing.T) {
	var vars map[string]interface{}
	srv := statusLineProbeServer(t, "2026-10-10T10:00:00Z", true, &vars)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	c.SetStatusLineField("Fabrik")
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if vars["statusLine"] != "Fabrik" || vars["withStatusLine"] != true {
		t.Errorf("vars = %v, want statusLine=Fabrik withStatusLine=true", vars)
	}
	want, _ := time.Parse(time.RFC3339, "2026-10-10T09:00:00Z")
	if !items[0].EffectiveUpdatedAt.Equal(want) {
		t.Errorf("EffectiveUpdatedAt = %v, want the content time %v (display bump discounted)", items[0].EffectiveUpdatedAt, want)
	}
}

func TestProbeProjectBoard_LaterItemUpdateStillCounts(t *testing.T) {
	var vars map[string]interface{}
	srv := statusLineProbeServer(t, "2026-10-10T10:30:00Z", true, &vars)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	c.SetStatusLineField("Fabrik")
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-10-10T10:30:00Z")
	if !items[0].EffectiveUpdatedAt.Equal(want) {
		t.Errorf("EffectiveUpdatedAt = %v, want %v", items[0].EffectiveUpdatedAt, want)
	}
}

func TestProbeProjectBoard_FeatureOffBehavesAsBefore(t *testing.T) {
	var vars map[string]interface{}
	srv := statusLineProbeServer(t, "2026-10-10T10:00:00Z", false, &vars)
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL) // no SetStatusLineField
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if vars["withStatusLine"] != false {
		t.Errorf("withStatusLine = %v, want false so the selection stays off the wire", vars["withStatusLine"])
	}
	want, _ := time.Parse(time.RFC3339, "2026-10-10T10:00:00Z")
	if !items[0].EffectiveUpdatedAt.Equal(want) {
		t.Errorf("EffectiveUpdatedAt = %v, want the item time %v", items[0].EffectiveUpdatedAt, want)
	}
}

func TestFetchProjectBoard_DisplayFieldBumpIsDiscounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := map[string]interface{}{
			"id":         "PVTI_1",
			"updatedAt":  "2026-10-10T10:00:00Z",
			"statusLine": map[string]interface{}{"updatedAt": "2026-10-10T10:00:00Z"},
			"content": map[string]interface{}{
				"__typename": "Issue", "id": "I_1", "number": 1, "title": "t", "state": "OPEN",
				"updatedAt":  "2026-10-10T09:00:00Z",
				"repository": map[string]interface{}{"nameWithOwner": "owner/repo"},
				"labels":     map[string]interface{}{"nodes": []interface{}{}},
			},
		}
		json.NewEncoder(w).Encode(probeResponse("user", "PVT_1", []interface{}{node}, 1, false, ""))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	c.SetStatusLineField("Fabrik")
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-10-10T09:00:00Z")
	if !board.Items[0].UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v (display bump discounted)", board.Items[0].UpdatedAt, want)
	}
}

func TestProjectItemUpdatedAt_MultipleDisplayFields(t *testing.T) {
	status := &statusLineValue{UpdatedAt: "2026-10-10T09:00:00Z"}
	activity := &statusLineValue{UpdatedAt: "2026-10-10T10:00:00Z"}
	run := &statusLineValue{UpdatedAt: "2026-10-10T10:00:01Z"}
	cases := []struct {
		name   string
		item   string
		vals   []*statusLineValue
		wantOK bool
	}{
		{"bump explained by the latest of three", "2026-10-10T10:00:01Z", []*statusLineValue{status, activity, run}, false},
		{"bump explained only by a newer field than status", "2026-10-10T10:00:00Z", []*statusLineValue{status, activity, nil}, false},
		{"nil values are ignored", "2026-10-10T10:00:00Z", []*statusLineValue{nil, nil, nil}, true},
		{"later real change still counts", "2026-10-10T10:30:00Z", []*statusLineValue{status, activity, run}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := projectItemUpdatedAt(tc.item, tc.vals...); ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// TestDisplayFieldBumpsAreDiscounted_NewFields pins that a project-item bump
// explained only by a Last activity or Last run write is not item activity on
// either query (#2049), and that a later change still counts.
func TestDisplayFieldBumpsAreDiscounted_NewFields(t *testing.T) {
	for _, tc := range []struct {
		alias string
		want  string
		bump  string
	}{
		{"lastActivity", "2026-10-10T09:00:00Z", "2026-10-10T10:00:00Z"},
		{"lastRun", "2026-10-10T09:00:00Z", "2026-10-10T10:00:00Z"},
		{"lastActivity", "2026-10-10T10:30:00Z", "2026-10-10T10:30:00Z"},
		{"lastRun", "2026-10-10T10:30:00Z", "2026-10-10T10:30:00Z"},
	} {
		t.Run(tc.alias+"/"+tc.want, func(t *testing.T) {
			var probeVars, boardVars map[string]interface{}
			probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				json.NewDecoder(r.Body).Decode(&body)
				probeVars, _ = body["variables"].(map[string]interface{})
				node := probeItemResponse("PVTI_1", "I_abc", 10, "Issue", "OPEN", "2026-10-10T09:00:00Z", tc.bump, "owner/repo", 0, "")
				node[tc.alias] = map[string]interface{}{"updatedAt": "2026-10-10T10:00:00Z"}
				json.NewEncoder(w).Encode(probeResponse("user", "PVT_1", []interface{}{node}, 1, false, ""))
			}))
			defer probeSrv.Close()
			c := NewClientWithBaseURL("token", probeSrv.URL)
			c.SetLastActivityField("Last activity")
			c.SetLastRunField("Last run")
			items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
			if err != nil {
				t.Fatalf("ProbeProjectBoard: %v", err)
			}
			want, _ := time.Parse(time.RFC3339, tc.want)
			if !items[0].EffectiveUpdatedAt.Equal(want) {
				t.Errorf("probe EffectiveUpdatedAt = %v, want %v", items[0].EffectiveUpdatedAt, want)
			}
			if probeVars["lastActivity"] != "Last activity" || probeVars["withLastActivity"] != true ||
				probeVars["lastRun"] != "Last run" || probeVars["withLastRun"] != true ||
				probeVars["withStatusLine"] != false {
				t.Errorf("probe vars = %v", probeVars)
			}

			boardSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]interface{}
				json.NewDecoder(r.Body).Decode(&body)
				boardVars, _ = body["variables"].(map[string]interface{})
				node := map[string]interface{}{
					"id":        "PVTI_1",
					"updatedAt": tc.bump,
					tc.alias:    map[string]interface{}{"updatedAt": "2026-10-10T10:00:00Z"},
					"content": map[string]interface{}{
						"__typename": "Issue", "id": "I_1", "number": 1, "title": "t", "state": "OPEN",
						"updatedAt":  "2026-10-10T09:00:00Z",
						"repository": map[string]interface{}{"nameWithOwner": "owner/repo"},
						"labels":     map[string]interface{}{"nodes": []interface{}{}},
					},
				}
				json.NewEncoder(w).Encode(probeResponse("user", "PVT_1", []interface{}{node}, 1, false, ""))
			}))
			defer boardSrv.Close()
			c = NewClientWithBaseURL("token", boardSrv.URL)
			c.SetLastActivityField("Last activity")
			c.SetLastRunField("Last run")
			board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
			if err != nil {
				t.Fatalf("FetchProjectBoard: %v", err)
			}
			if !board.Items[0].UpdatedAt.Equal(want) {
				t.Errorf("board UpdatedAt = %v, want %v", board.Items[0].UpdatedAt, want)
			}
			if boardVars["withLastActivity"] != true || boardVars["withLastRun"] != true {
				t.Errorf("board vars = %v", boardVars)
			}
		})
	}
}
