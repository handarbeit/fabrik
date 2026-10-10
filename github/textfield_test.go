package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
