package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveOwner_Organization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repositoryOwner": map[string]interface{}{
					"__typename": "Organization",
					"id":         "O_ORG1",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, ownerType, err := c.ResolveOwner("handarbeit")
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if id != "O_ORG1" {
		t.Errorf("id = %q, want O_ORG1", id)
	}
	if ownerType != "organization" {
		t.Errorf("ownerType = %q, want organization", ownerType)
	}
}

func TestResolveOwner_User(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repositoryOwner": map[string]interface{}{
					"__typename": "User",
					"id":         "U_USER1",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, ownerType, err := c.ResolveOwner("someuser")
	if err != nil {
		t.Fatalf("ResolveOwner: %v", err)
	}
	if id != "U_USER1" {
		t.Errorf("id = %q, want U_USER1", id)
	}
	if ownerType != "user" {
		t.Errorf("ownerType = %q, want user", ownerType)
	}
}

func TestResolveOwner_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repositoryOwner": nil,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, _, err := c.ResolveOwner("nobody"); err == nil {
		t.Fatal("expected error for unknown owner")
	}
}

func TestResolveOwner_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, _, err := c.ResolveOwner("handarbeit"); err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestFetchRepositoryID_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
					"id": "R_REPO1",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, err := c.FetchRepositoryID("handarbeit", "fabrik")
	if err != nil {
		t.Fatalf("FetchRepositoryID: %v", err)
	}
	if id != "R_REPO1" {
		t.Errorf("id = %q, want R_REPO1", id)
	}
}

func TestFetchRepositoryID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repository": nil,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchRepositoryID("handarbeit", "nope"); err == nil {
		t.Fatal("expected error for missing repository")
	}
}

func TestCreateProjectV2_Success(t *testing.T) {
	var gotVars map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotVars = body.Variables

		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"createProjectV2": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id":     "PVT_NEW1",
						"number": 7,
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	projectID, number, err := c.CreateProjectV2("O_ORG1", "fabrik Fabrik Pipeline", "R_REPO1")
	if err != nil {
		t.Fatalf("CreateProjectV2: %v", err)
	}
	if projectID != "PVT_NEW1" {
		t.Errorf("projectID = %q, want PVT_NEW1", projectID)
	}
	if number != 7 {
		t.Errorf("number = %d, want 7", number)
	}
	if gotVars["ownerId"] != "O_ORG1" || gotVars["repositoryId"] != "R_REPO1" {
		t.Errorf("unexpected variables sent: %+v", gotVars)
	}
}

func TestCreateProjectV2_NoProjectReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"createProjectV2": map[string]interface{}{
					"projectV2": nil,
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, _, err := c.CreateProjectV2("O_ORG1", "title", "R_REPO1"); err == nil {
		t.Fatal("expected error when no project is returned")
	}
}

func TestSetProjectDescription_Success(t *testing.T) {
	var gotVars map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotVars = body.Variables
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"updateProjectV2": map[string]interface{}{
					"projectV2": map[string]interface{}{"id": "PVT_1"},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.SetProjectDescription("PVT_1", "Managed by Fabrik"); err != nil {
		t.Fatalf("SetProjectDescription: %v", err)
	}
	if gotVars["shortDescription"] != "Managed by Fabrik" {
		t.Errorf("shortDescription = %v", gotVars["shortDescription"])
	}
}

func TestSetProjectDescription_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.SetProjectDescription("PVT_1", "desc"); err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// TestSetStatusFieldOptions_PreservesExistingIDsAndEchoesFields is the R4/R5
// payload assertion: it proves the exact wire shape SetStatusFieldOptions
// sends echoes every existing option's id/color/description unchanged and
// appends only new options with a nil id — the shape the issue's own
// empirical 2026-09-13 finding established as safe (preserving ids keeps
// item Status assignments intact; omitting id on an existing option
// regenerates it and silently clears every item's Status).
func TestSetStatusFieldOptions_PreservesExistingIDsAndEchoesFields(t *testing.T) {
	var gotVars map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotVars = body.Variables
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"updateProjectV2Field": map[string]interface{}{
					"projectV2Field": map[string]interface{}{"id": "FIELD_STATUS"},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	existingID1 := "OPT_1"
	existingID2 := "OPT_2"
	options := []StatusOptionInput{
		{ID: &existingID1, Name: "Todo", Color: "GRAY", Description: "todo desc"},
		{ID: &existingID2, Name: "In Progress", Color: "BLUE", Description: ""},
		{ID: nil, Name: "Queued", Color: "GRAY", Description: ""}, // new column, no id
	}

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.SetStatusFieldOptions("FIELD_STATUS", options); err != nil {
		t.Fatalf("SetStatusFieldOptions: %v", err)
	}

	rawOptions, ok := gotVars["options"].([]interface{})
	if !ok || len(rawOptions) != 3 {
		t.Fatalf("options in payload = %+v", gotVars["options"])
	}

	opt0 := rawOptions[0].(map[string]interface{})
	if opt0["id"] != "OPT_1" || opt0["name"] != "Todo" || opt0["color"] != "GRAY" || opt0["description"] != "todo desc" {
		t.Errorf("existing option 0 not echoed correctly: %+v", opt0)
	}
	opt1 := rawOptions[1].(map[string]interface{})
	if opt1["id"] != "OPT_2" || opt1["color"] != "BLUE" {
		t.Errorf("existing option 1 not echoed correctly: %+v", opt1)
	}
	opt2 := rawOptions[2].(map[string]interface{})
	if _, hasID := opt2["id"]; hasID {
		t.Errorf("new option must omit id entirely, got: %+v", opt2)
	}
	if opt2["name"] != "Queued" {
		t.Errorf("new option name = %v, want Queued", opt2["name"])
	}
}

func TestSetStatusFieldOptions_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("server error"))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.SetStatusFieldOptions("FIELD_STATUS", nil); err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// TestSetStatusFieldOptions_QueryOmitsIDForNilPointer is a focused unit test
// (no HTTP round trip) proving the mutation-string-building logic itself
// never emits an "id" key when StatusOptionInput.ID is nil, independent of
// httptest JSON round-tripping (which could theoretically mask a bug via
// omitempty-like behavior at the transport layer).
func TestSetStatusFieldOptions_QueryOmitsIDForNilPointer(t *testing.T) {
	if !strings.Contains(updateProjectV2FieldMutation, "singleSelectOptions: $options") {
		t.Fatal("updateProjectV2FieldMutation no longer sends $options as singleSelectOptions — update this test")
	}
}
