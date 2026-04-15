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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"id": 12345}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, err := c.AddComment("owner", "repo", 42, "Hello world")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if id != 12345 {
		t.Errorf("expected id=12345, got %d", id)
	}
}

func TestAddComment_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.AddComment("owner", "repo", 42, "test"); err == nil {
		t.Fatal("expected error for 403 response")
	}
}

func TestAddComment_MissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{}`)) // valid JSON but no "id" field
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.AddComment("owner", "repo", 42, "test"); err == nil {
		t.Fatal("expected error when response omits id")
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
		if body["color"] != "0075ca" {
			t.Errorf("color = %v", body["color"])
		}
		if body["description"] != "my description" {
			t.Errorf("description = %v", body["description"])
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.ensureLabel("owner", "repo", "test-label", "my description", "0075ca"); err != nil {
		t.Fatalf("ensureLabel: %v", err)
	}
}

// TestSeedLabels_CreateMissing: seedOneLabel creates a label that does not exist.
func TestSeedLabels_CreateMissing(t *testing.T) {
	var gotMethod []string
	var postBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = append(gotMethod, r.Method)
		switch r.Method {
		case http.MethodGet:
			// Label does not exist
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"Not Found"}`))
		case http.MethodPost:
			json.NewDecoder(r.Body).Decode(&postBody)
			w.WriteHeader(201)
		default:
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	d := labelDef{"fabrik:yolo", "Auto-advance all stages and auto-merge the PR at Validate", "0075ca"}
	if err := c.seedOneLabel("owner", "repo", d); err != nil {
		t.Fatalf("seedOneLabel: %v", err)
	}
	if len(gotMethod) != 2 || gotMethod[0] != "GET" || gotMethod[1] != "POST" {
		t.Errorf("expected [GET POST], got %v", gotMethod)
	}
	if postBody["name"] != "fabrik:yolo" {
		t.Errorf("POST name = %v", postBody["name"])
	}
	if postBody["color"] != "0075ca" {
		t.Errorf("POST color = %v", postBody["color"])
	}
	if postBody["description"] != "Auto-advance all stages and auto-merge the PR at Validate" {
		t.Errorf("POST description = %v", postBody["description"])
	}
}

// TestSeedLabels_BackfillEmpty: seedOneLabel PATCHes an existing label that has
// an empty description. The PATCH body must NOT contain a color field.
func TestSeedLabels_BackfillEmpty(t *testing.T) {
	var gotMethod []string
	var patchBody map[string]interface{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = append(gotMethod, r.Method)
		switch r.Method {
		case http.MethodGet:
			// Label exists with empty description
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]string{"name": "fabrik:paused", "description": ""})
		case http.MethodPatch:
			json.NewDecoder(r.Body).Decode(&patchBody)
			w.WriteHeader(200)
		default:
			t.Errorf("unexpected method: %s", r.Method)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	d := labelDef{"fabrik:paused", "Stage failed or needs intervention; remove to resume", "e4e669"}
	if err := c.seedOneLabel("owner", "repo", d); err != nil {
		t.Fatalf("seedOneLabel: %v", err)
	}
	if len(gotMethod) != 2 || gotMethod[0] != "GET" || gotMethod[1] != "PATCH" {
		t.Errorf("expected [GET PATCH], got %v", gotMethod)
	}
	// PATCH body must include description and must NOT include color.
	if patchBody["description"] != "Stage failed or needs intervention; remove to resume" {
		t.Errorf("PATCH description = %v", patchBody["description"])
	}
	if _, hasColor := patchBody["color"]; hasColor {
		t.Error("PATCH body must not include color field")
	}
}

// TestSeedLabels_SkipNonEmpty: seedOneLabel does nothing when the label already
// has a non-empty description (preserves user customisation).
func TestSeedLabels_SkipNonEmpty(t *testing.T) {
	var gotMethod []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = append(gotMethod, r.Method)
		switch r.Method {
		case http.MethodGet:
			// Label exists with a custom description
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(map[string]string{"name": "fabrik:paused", "description": "user custom description"})
		default:
			t.Errorf("unexpected method: %s — should not PATCH when description is non-empty", r.Method)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	d := labelDef{"fabrik:paused", "Stage failed or needs intervention; remove to resume", "e4e669"}
	if err := c.seedOneLabel("owner", "repo", d); err != nil {
		t.Fatalf("seedOneLabel: %v", err)
	}
	if len(gotMethod) != 1 || gotMethod[0] != "GET" {
		t.Errorf("expected [GET] only, got %v", gotMethod)
	}
}

func TestUpdateComment_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/owner/repo/issues/comments/99") {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["body"] != "updated body" {
			t.Errorf("body = %v", body["body"])
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.UpdateComment("owner", "repo", 99, "updated body"); err != nil {
		t.Fatalf("UpdateComment: %v", err)
	}
}

func TestUpdateComment_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"not found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.UpdateComment("owner", "repo", 99, "body"); err == nil {
		t.Fatal("expected error for 404 response")
	}
}
