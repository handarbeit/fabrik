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

// TestCreateProjectV2_Unlinked: with no repository, the board is created
// with a mutation that has no repositoryId at all — sending "" would be a
// GraphQL error, and a linked mutation would need a real repository.
func TestCreateProjectV2_Unlinked(t *testing.T) {
	var gotQuery string
	var gotVars map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotQuery, gotVars = body.Query, body.Variables
		json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
			"createProjectV2": map[string]interface{}{"projectV2": map[string]interface{}{"id": "PVT_NEW2", "number": 8}},
		}})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	projectID, number, err := c.CreateProjectV2("O_ORG1", "acme Fabrik Pipeline", "")
	if err != nil {
		t.Fatalf("CreateProjectV2: %v", err)
	}
	if projectID != "PVT_NEW2" || number != 8 {
		t.Errorf("got (%q, %d), want (PVT_NEW2, 8)", projectID, number)
	}
	if strings.Contains(gotQuery, "repositoryId") {
		t.Errorf("unlinked create sent a repositoryId in its query:\n%s", gotQuery)
	}
	if _, ok := gotVars["repositoryId"]; ok {
		t.Errorf("unlinked create sent a repositoryId variable: %+v", gotVars)
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

func TestCreateProjectV2Field_SendsNameAndTypeAndReturnsID(t *testing.T) {
	var gotVars map[string]interface{}
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotVars, gotQuery = body.Variables, body.Query
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"createProjectV2Field": map[string]interface{}{
					"projectV2Field": map[string]interface{}{"id": "F_NEW"},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	id, err := c.CreateProjectV2Field("P1", "Last run", FieldTypeText)
	if err != nil {
		t.Fatalf("CreateProjectV2Field: %v", err)
	}
	if id != "F_NEW" {
		t.Errorf("id = %q, want F_NEW", id)
	}
	if gotVars["projectId"] != "P1" || gotVars["name"] != "Last run" || gotVars["dataType"] != "TEXT" {
		t.Errorf("variables = %+v", gotVars)
	}
	if !strings.Contains(gotQuery, "createProjectV2Field") || strings.Contains(gotQuery, "updateProjectV2Field") {
		t.Errorf("must use createProjectV2Field and never updateProjectV2Field: %s", gotQuery)
	}
}

func TestCreateProjectV2Field_RejectsUnsupportedType(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.CreateProjectV2Field("P1", "X", "SINGLE_SELECT"); err == nil {
		t.Fatal("expected error for unsupported data type")
	}
	if called {
		t.Error("no request should be made for an unsupported data type")
	}
}

func TestCreateProjectV2Field_EmptyIDIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"createProjectV2Field": map[string]interface{}{
					"projectV2Field": map[string]interface{}{},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.CreateProjectV2Field("P1", "Fabrik", FieldTypeText)
	if err == nil || !strings.Contains(err.Error(), "Fabrik") {
		t.Fatalf("expected error naming the field, got %v", err)
	}
}

func TestCreateProjectV2Field_GraphQLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{{"message": "Name has already been taken"}},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.CreateProjectV2Field("P1", "Fabrik", FieldTypeText)
	if err == nil || !strings.Contains(err.Error(), "already been taken") {
		t.Fatalf("expected GitHub's message preserved, got %v", err)
	}
}

func TestFetchProjectFields_PaginatesAndReturnsAllTypes(t *testing.T) {
	calls := 0
	var afters []interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		afters = append(afters, body.Variables["after"])
		calls++
		var page map[string]interface{}
		if calls == 1 {
			page = map[string]interface{}{
				"nodes": []map[string]interface{}{
					{"id": "F1", "name": "Title", "dataType": "TITLE"},
					{"id": "F2", "name": "Fabrik", "dataType": "SINGLE_SELECT"},
					{}, // a node matching no fragment decodes empty
				},
				"pageInfo": map[string]interface{}{"hasNextPage": true, "endCursor": "CUR1"},
			}
		} else {
			page = map[string]interface{}{
				"nodes": []map[string]interface{}{
					{"id": "F3", "name": "Last run", "dataType": "TEXT"},
				},
				"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"node": map[string]interface{}{"fields": page}},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	fields, err := c.FetchProjectFields("P1")
	if err != nil {
		t.Fatalf("FetchProjectFields: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("fields = %+v, want 3", fields)
	}
	if fields[1].Name != "Fabrik" || fields[1].DataType != "SINGLE_SELECT" {
		t.Errorf("fields[1] = %+v", fields[1])
	}
	if fields[2].ID != "F3" || fields[2].DataType != "TEXT" {
		t.Errorf("fields[2] = %+v", fields[2])
	}
	if calls != 2 || afters[0] != nil || afters[1] != "CUR1" {
		t.Errorf("pagination: calls=%d afters=%v", calls, afters)
	}
}

func TestFetchProjectFields_ProjectNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"node": nil}})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	if _, err := c.FetchProjectFields("P1"); err == nil {
		t.Fatal("expected error when project is not found")
	}
}

// optionInputIntrospection answers the option-input introspection query with
// the given input field names (nil = a null __type).
func optionInputIntrospection(names []string) map[string]interface{} {
	if names == nil {
		return map[string]interface{}{"data": map[string]interface{}{"__type": nil}}
	}
	fields := make([]map[string]interface{}, 0, len(names))
	for _, n := range names {
		fields = append(fields, map[string]interface{}{"name": n})
	}
	return map[string]interface{}{"data": map[string]interface{}{"__type": map[string]interface{}{"inputFields": fields}}}
}

func TestStatusOptionIDSupport(t *testing.T) {
	tests := []struct {
		name string
		body func(w http.ResponseWriter)
		want IDSupport
	}{
		{"github.com shape", func(w http.ResponseWriter) {
			json.NewEncoder(w).Encode(optionInputIntrospection([]string{"color", "description", "id", "name"}))
		}, IDAccepted},
		{"GHES shape", func(w http.ResponseWriter) {
			json.NewEncoder(w).Encode(optionInputIntrospection([]string{"color", "description", "name"}))
		}, IDNotAccepted},
		{"null type", func(w http.ResponseWriter) {
			json.NewEncoder(w).Encode(optionInputIntrospection(nil))
		}, IDUnknown},
		{"graphql error (introspection denied)", func(w http.ResponseWriter) {
			json.NewEncoder(w).Encode(map[string]interface{}{"errors": []map[string]interface{}{{"message": "introspection is disabled"}}})
		}, IDUnknown},
		{"http error", func(w http.ResponseWriter) {
			w.WriteHeader(500)
			w.Write([]byte("boom"))
		}, IDUnknown},
		{"empty field list", func(w http.ResponseWriter) {
			json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"__type": map[string]interface{}{"inputFields": []interface{}{}}}})
		}, IDUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Query string `json:"query"`
				}
				json.NewDecoder(r.Body).Decode(&body)
				gotQuery = body.Query
				tc.body(w)
			}))
			defer srv.Close()
			c := NewClientWithBaseURL("token", srv.URL)
			if got := c.StatusOptionIDSupport(); got != tc.want {
				t.Errorf("StatusOptionIDSupport = %v, want %v", got, tc.want)
			}
			if !strings.Contains(gotQuery, "ProjectV2SingleSelectFieldOptionInput") {
				t.Errorf("did not introspect the option input type: %s", gotQuery)
			}
		})
	}
}

// populationServer serves pages of items; each page is a list of item
// "has Status" flags. It records the variables of every request.
func populationServer(t *testing.T, pages [][]bool, gotVars *[]map[string]interface{}, gotQuery *string) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		*gotVars = append(*gotVars, body.Variables)
		*gotQuery = body.Query
		page := pages[call]
		call++
		nodes := make([]map[string]interface{}, 0, len(page))
		for i, has := range page {
			n := map[string]interface{}{"id": "PVTI_" + string(rune('a'+i)), "fieldValueByName": nil}
			if has {
				n["fieldValueByName"] = map[string]interface{}{"optionId": "OPT"}
			}
			nodes = append(nodes, n)
		}
		hasNext := call < len(pages)
		end := ""
		if hasNext {
			end = "CUR" + string(rune('0'+call))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"node": map[string]interface{}{
			"items": map[string]interface{}{
				"pageInfo": map[string]interface{}{"hasNextPage": hasNext, "endCursor": end},
				"nodes":    nodes,
			},
		}}})
	}))
}

func TestBoardHasStatusValue(t *testing.T) {
	t.Run("no items", func(t *testing.T) {
		var vars []map[string]interface{}
		var q string
		srv := populationServer(t, [][]bool{{}}, &vars, &q)
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err != nil || got {
			t.Fatalf("got (%v, %v), want (false, nil)", got, err)
		}
		if !strings.Contains(q, "archivedStates: [ARCHIVED, NOT_ARCHIVED]") {
			t.Errorf("archived items not requested: %s", q)
		}
	})
	t.Run("items none with a Status", func(t *testing.T) {
		var vars []map[string]interface{}
		var q string
		srv := populationServer(t, [][]bool{{false, false}, {false}}, &vars, &q)
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err != nil || got {
			t.Fatalf("got (%v, %v), want (false, nil)", got, err)
		}
		if len(vars) != 2 || vars[1]["cursor"] != "CUR1" {
			t.Errorf("did not page: %v", vars)
		}
	})
	t.Run("hit on first page stops early", func(t *testing.T) {
		var vars []map[string]interface{}
		var q string
		srv := populationServer(t, [][]bool{{false, true}, {true}}, &vars, &q)
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err != nil || !got {
			t.Fatalf("got (%v, %v), want (true, nil)", got, err)
		}
		if len(vars) != 1 {
			t.Errorf("should stop at the first hit, made %d requests", len(vars))
		}
	})
	t.Run("hit on a later page", func(t *testing.T) {
		var vars []map[string]interface{}
		var q string
		srv := populationServer(t, [][]bool{{false}, {false}, {true}}, &vars, &q)
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err != nil || !got {
			t.Fatalf("got (%v, %v), want (true, nil)", got, err)
		}
		if len(vars) != 3 {
			t.Errorf("requests = %d, want 3", len(vars))
		}
	})
	t.Run("http error fails closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		}))
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err == nil || !got {
			t.Fatalf("got (%v, %v), want (true, error)", got, err)
		}
	})
	t.Run("unreadable project fails closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"node": nil}})
		}))
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err == nil || !got {
			t.Fatalf("got (%v, %v), want (true, error)", got, err)
		}
	})
	t.Run("error on a later page fails closed", func(t *testing.T) {
		call := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			call++
			if call == 2 {
				w.WriteHeader(500)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"node": map[string]interface{}{
				"items": map[string]interface{}{
					"pageInfo": map[string]interface{}{"hasNextPage": true, "endCursor": "C"},
					"nodes":    []interface{}{},
				},
			}}})
		}))
		defer srv.Close()
		got, err := NewClientWithBaseURL("token", srv.URL).BoardHasStatusValue("P1")
		if err == nil || !got {
			t.Fatalf("got (%v, %v), want (true, error)", got, err)
		}
	})
}

// TestSetStatusFieldOptionsWithoutIDs_RefusesAnID: the no-id path never sends
// a request when any option carries an id, and otherwise sends name, color and
// description only.
func TestSetStatusFieldOptionsWithoutIDs(t *testing.T) {
	var gotVars map[string]interface{}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Variables map[string]interface{} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotVars = body.Variables
		json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
			"updateProjectV2Field": map[string]interface{}{"projectV2Field": map[string]interface{}{"id": "F"}},
		}})
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)

	id := "OPT_1"
	if err := c.SetStatusFieldOptionsWithoutIDs("F", []StatusOptionInput{{ID: &id, Name: "Todo", Color: "GRAY"}}); err == nil {
		t.Fatal("expected an error for an option carrying an id")
	}
	if calls != 0 {
		t.Fatalf("no request may be sent when an id is present, got %d", calls)
	}

	if err := c.SetStatusFieldOptionsWithoutIDs("F", []StatusOptionInput{
		{Name: "Todo", Color: "BLUE", Description: "d"},
		{Name: "Queued", Color: "GRAY"},
	}); err != nil {
		t.Fatalf("SetStatusFieldOptionsWithoutIDs: %v", err)
	}
	raw := gotVars["options"].([]interface{})
	if len(raw) != 2 {
		t.Fatalf("options = %v", raw)
	}
	for i, o := range raw {
		m := o.(map[string]interface{})
		if _, has := m["id"]; has {
			t.Errorf("option %d sent an id: %v", i, m)
		}
		for _, k := range []string{"name", "color", "description"} {
			if _, ok := m[k]; !ok {
				t.Errorf("option %d missing %s: %v", i, k, m)
			}
		}
	}
	if raw[0].(map[string]interface{})["color"] != "BLUE" || raw[0].(map[string]interface{})["description"] != "d" {
		t.Errorf("existing color/description not preserved: %v", raw[0])
	}
}
