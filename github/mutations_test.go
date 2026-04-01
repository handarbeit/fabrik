package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddComment_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/42/comments") {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["body"] != "Hello world" {
			t.Errorf("body = %v", body["body"])
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddComment("owner", "repo", 42, "Hello world"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
}

func TestAddComment_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddComment("owner", "repo", 42, "test"); err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestAddLabelToIssue_Success(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if strings.HasSuffix(r.URL.Path, "/labels") && !strings.Contains(r.URL.Path, "/issues/") {
			// ensureLabel call — create label
			w.WriteHeader(201)
			return
		}
		// Add label to issue
		if !strings.Contains(r.URL.Path, "/issues/10/labels") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		labels := body["labels"].([]interface{})
		if labels[0] != "fabrik:locked:user" {
			t.Errorf("label = %v", labels[0])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddLabelToIssue("owner", "repo", 10, "fabrik:locked:user"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 API calls (ensureLabel + addLabel), got %d", callCount)
	}
}

func TestAddLabelToIssue_EnsureLabelAlreadyExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/labels") && !strings.Contains(r.URL.Path, "/issues/") {
			// Label already exists — 422
			w.WriteHeader(422)
			w.Write([]byte(`{"message":"Validation Failed"}`))
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	// Should succeed even though ensureLabel gets 422
	if err := c.AddLabelToIssue("owner", "repo", 1, "test-label"); err != nil {
		t.Fatalf("AddLabelToIssue: %v", err)
	}
}

func TestUpdateProjectItemStatus_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		vars := body["variables"].(map[string]interface{})
		if vars["projectId"] != "PVT_1" {
			t.Errorf("projectId = %v", vars["projectId"])
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"PVTI_1"}}}}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.UpdateProjectItemStatus("PVT_1", "PVTI_1", "FIELD_1", "OPT_1"); err != nil {
		t.Fatalf("UpdateProjectItemStatus: %v", err)
	}
}

func TestUpdateProjectItemStatus_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"errors":[{"message":"Project not found"}]}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.UpdateProjectItemStatus("PVT_1", "PVTI_1", "FIELD_1", "OPT_1"); err == nil {
		t.Fatal("expected error for GraphQL error")
	}
}

func TestFetchStatusField_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"field": map[string]interface{}{
						"id": "FIELD_STATUS",
						"options": []interface{}{
							map[string]interface{}{"id": "OPT_1", "name": "Todo"},
							map[string]interface{}{"id": "OPT_2", "name": "In Progress"},
							map[string]interface{}{"id": "OPT_3", "name": "Done"},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	sf, err := c.FetchStatusField("PVT_123")
	if err != nil {
		t.Fatalf("FetchStatusField: %v", err)
	}
	if sf.FieldID != "FIELD_STATUS" {
		t.Errorf("FieldID = %q", sf.FieldID)
	}
	if len(sf.Options) != 3 {
		t.Errorf("options count = %d", len(sf.Options))
	}
	if sf.Options["Todo"] != "OPT_1" {
		t.Errorf("Options[Todo] = %q", sf.Options["Todo"])
	}
	if sf.Options["Done"] != "OPT_3" {
		t.Errorf("Options[Done] = %q", sf.Options["Done"])
	}
}

func TestFetchStatusField_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`server error`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchStatusField("PVT_123")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestFetchStatusField_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"field": nil,
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.FetchStatusField("PVT_456")
	if err == nil {
		t.Fatal("expected error when Status field is absent")
	}
	if !strings.Contains(err.Error(), "no Status field") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestAddLabelToIssue_AddFails(t *testing.T) {
	callNum := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum++
		if callNum == 1 {
			// ensureLabel succeeds
			w.WriteHeader(201)
			return
		}
		// addLabel fails
		w.WriteHeader(500)
		w.Write([]byte(`server error`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	err := c.AddLabelToIssue("owner", "repo", 1, "test")
	if err == nil {
		t.Fatal("expected error when add label fails")
	}
}

func TestEnsureLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "test-label" {
			t.Errorf("name = %v", body["name"])
		}
		if body["color"] != "6f42c1" {
			t.Errorf("color = %v", body["color"])
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.ensureLabel("owner", "repo", "test-label"); err != nil {
		t.Fatalf("ensureLabel: %v", err)
	}
}
