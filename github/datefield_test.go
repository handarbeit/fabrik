package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchDateField_Success(t *testing.T) {
	srv := textFieldServer(t, "fetch_date_field", func(q string, vars map[string]interface{}) {
		if vars["projectId"] != "PVT_1" || vars["name"] != "Last activity" {
			t.Errorf("vars = %v", vars)
		}
	})
	defer srv.Close()

	f, err := NewClientWithBaseURL("token", srv.URL).FetchDateField("PVT_1", "Last activity")
	if err != nil {
		t.Fatalf("FetchDateField: %v", err)
	}
	if f == nil || f.ID == "" || f.Name != "Last activity" {
		t.Fatalf("field = %+v", f)
	}
}

func TestFetchDateField_TypeMismatch(t *testing.T) {
	for name, resp := range map[string]string{
		"no id (union)": `{"data":{"node":{"field":{}}}}`,
		"text field":    `{"data":{"node":{"field":{"id":"F","name":"Last activity","dataType":"TEXT"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(resp))
			}))
			defer srv.Close()
			f, err := NewClientWithBaseURL("token", srv.URL).FetchDateField("PVT_1", "Last activity")
			var mm *FieldTypeMismatchError
			if !errors.As(err, &mm) || f != nil || mm.Want != "DATE" {
				t.Fatalf("want *FieldTypeMismatchError for DATE, got %+v, %v", f, err)
			}
		})
	}
}

// SC-001 (#2218), date twin: see TestFetchTextField_AbsentFieldGraphQLError.
func TestFetchDateField_AbsentFieldGraphQLError(t *testing.T) {
	srv := textFieldServer(t, "fetch_date_field_absent", func(q string, vars map[string]interface{}) {})
	defer srv.Close()
	f, err := NewClientWithBaseURL("token", srv.URL).FetchDateField("PVT_1", "Last activity")
	if err != nil || f != nil {
		t.Fatalf("an absent field must resolve to (nil, nil), got %+v, %v", f, err)
	}
}

func TestFetchDateField_Missing(t *testing.T) {
	for name, resp := range map[string]string{
		"missing field": `{"data":{"node":{"field":null}}}`,
		"non-project":   `{"data":{"node":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(resp))
			}))
			defer srv.Close()
			f, err := NewClientWithBaseURL("token", srv.URL).FetchDateField("PVT_1", "Last activity")
			if err != nil {
				t.Fatalf("want nil error for an absent/non-date field, got %v", err)
			}
			if f != nil {
				t.Fatalf("want nil field, got %+v", f)
			}
		})
	}
}

func TestFetchTextField_RejectsDateField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"node":{"field":{"id":"F","name":"Last run","dataType":"DATE"}}}}`))
	}))
	defer srv.Close()
	f, err := NewClientWithBaseURL("token", srv.URL).FetchTextField("PVT_1", "Last run")
	var mm *FieldTypeMismatchError
	if !errors.As(err, &mm) || f != nil || mm.Found != "DATE" {
		t.Fatalf("a date field must read as a type mismatch for a text lookup, got %+v, %v", f, err)
	}
}

func TestFetchDateField_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
	}))
	defer srv.Close()
	if _, err := NewClientWithBaseURL("token", srv.URL).FetchDateField("PVT_1", "Last activity"); err == nil {
		t.Fatal("expected error for GraphQL error")
	}
}

func TestUpdateProjectItemDateField_Success(t *testing.T) {
	srv := textFieldServer(t, "update_project_item_date_field", func(q string, vars map[string]interface{}) {
		if !strings.Contains(q, "updateProjectV2ItemFieldValue") || !strings.Contains(q, "date: $date") {
			t.Errorf("unexpected query: %s", q)
		}
		if vars["projectId"] != "PVT_1" || vars["itemId"] != "PVTI_1" || vars["fieldId"] != "F_1" || vars["date"] != "2026-10-10" {
			t.Errorf("vars = %v", vars)
		}
	})
	defer srv.Close()
	if err := NewClientWithBaseURL("token", srv.URL).UpdateProjectItemDateField("PVT_1", "PVTI_1", "F_1", "2026-10-10"); err != nil {
		t.Fatalf("UpdateProjectItemDateField: %v", err)
	}
}

func TestUpdateProjectItemDateField_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errors":[{"message":"nope"}]}`))
	}))
	defer srv.Close()
	if err := NewClientWithBaseURL("token", srv.URL).UpdateProjectItemDateField("PVT_1", "PVTI_1", "F_1", "2026-10-10"); err == nil {
		t.Fatal("expected error")
	}
}
