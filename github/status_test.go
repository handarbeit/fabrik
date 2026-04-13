package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAddBoardColumn_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}

		// Verify that options include both existing and new.
		options, ok := body.Variables["options"].([]interface{})
		if !ok {
			t.Fatalf("options variable missing or wrong type")
		}
		if len(options) != 3 {
			t.Fatalf("expected 3 options (2 existing + 1 new), got %d", len(options))
		}

		// Verify ordering: existing in original order + new appended.
		names := make([]string, len(options))
		for i, opt := range options {
			m, ok := opt.(map[string]interface{})
			if !ok {
				t.Fatalf("option[%d] is not a map", i)
			}
			name, ok := m["name"].(string)
			if !ok {
				t.Fatalf("option[%d] has no string 'name'", i)
			}
			names[i] = name
		}
		expected := []string{"Plan", "Research", "Review"}
		for i, name := range names {
			if name != expected[i] {
				t.Errorf("option[%d] = %q, want %q", i, name, expected[i])
			}
		}

		// Return mutation response with updated options including the new one.
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"updateProjectV2FieldDefinition": map[string]interface{}{
					"field": map[string]interface{}{
						"options": []map[string]interface{}{
							{"id": "opt-1", "name": "Plan"},
							{"id": "opt-2", "name": "Research"},
							{"id": "opt-new", "name": "Review"},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	existing := []string{"Plan", "Research"}

	optionID, err := c.AddBoardColumn("proj-1", "field-1", existing, "Review")
	if err != nil {
		t.Fatalf("AddBoardColumn: %v", err)
	}
	if optionID != "opt-new" {
		t.Errorf("optionID = %q, want %q", optionID, "opt-new")
	}
}

func TestAddBoardColumn_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]interface{}{
				{"message": "insufficient permissions"},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.AddBoardColumn("proj-1", "field-1", []string{}, "Review")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
}

func TestAddBoardColumn_OptionNotInResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"updateProjectV2FieldDefinition": map[string]interface{}{
					"field": map[string]interface{}{
						"options": []map[string]interface{}{
							{"id": "opt-1", "name": "Research"},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	_, err := c.AddBoardColumn("proj-1", "field-1", []string{"Research"}, "Review")
	if err == nil {
		t.Fatal("expected error when new option not in response")
	}
}
