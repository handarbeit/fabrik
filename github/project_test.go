package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseTime(t *testing.T) {
	ts, err := parseTime("2024-01-15T10:30:00Z")
	if err != nil {
		t.Fatalf("parseTime: %v", err)
	}
	if ts.Year() != 2024 || ts.Month() != 1 || ts.Day() != 15 {
		t.Errorf("parsed time = %v", ts)
	}

	_, err = parseTime("not-a-time")
	if err == nil {
		t.Error("expected error for invalid time string")
	}

	_, err = parseTime("")
	if err == nil {
		t.Error("expected error for empty time string")
	}
}

// readVars parses the GraphQL request body and returns the variables map.
func readVars(r *http.Request) map[string]interface{} {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(fmt.Sprintf("readVars: ReadAll: %v", err))
	}
	var req struct {
		Variables map[string]interface{} `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		panic(fmt.Sprintf("readVars: Unmarshal: %v", err))
	}
	return req.Variables
}

func makeItem(id, itemID, title string) map[string]interface{} {
	return map[string]interface{}{
		"id": itemID,
		"content": map[string]interface{}{
			"id":        id,
			"number":    1,
			"title":     title,
			"body":      "",
			"url":       "https://example.com",
			"labels":    map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
			"assignees": map[string]interface{}{"nodes": []interface{}{}},
		},
	}
}

// makeLookupServer creates an httptest server that serves a single GraphQL response
// at POST /graphql. The handler calls respFn to build the JSON response body.
func makeLookupServer(t *testing.T, respFn func() interface{}) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := respFn()
		if err := json.NewEncoder(w).Encode(body); err != nil {
			panic(fmt.Sprintf("makeLookupServer: encode: %v", err))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, NewClientWithBaseURL("token", srv.URL)
}

func TestLookupIssueProjectItem(t *testing.T) {
	t.Run("happy path returns itemID and status", func(t *testing.T) {
		_, client := makeLookupServer(t, func() interface{} {
			return map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"issue": map[string]interface{}{
							"projectItems": map[string]interface{}{
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_abc",
										"project": map[string]interface{}{
											"id": "PVT_target",
										},
										"fieldValueByName": map[string]interface{}{
											"name": "Research",
										},
									},
								},
							},
						},
					},
				},
			}
		})

		itemID, status, err := client.LookupIssueProjectItem("PVT_target", "owner/repo", 42)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if itemID != "PVTI_abc" {
			t.Errorf("itemID = %q, want %q", itemID, "PVTI_abc")
		}
		if status != "Research" {
			t.Errorf("status = %q, want %q", status, "Research")
		}
	})

	t.Run("multi-project filtering returns only the matching project item", func(t *testing.T) {
		_, client := makeLookupServer(t, func() interface{} {
			return map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"issue": map[string]interface{}{
							"projectItems": map[string]interface{}{
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_wrong",
										"project": map[string]interface{}{
											"id": "PVT_other",
										},
										"fieldValueByName": map[string]interface{}{
											"name": "Done",
										},
									},
									map[string]interface{}{
										"id": "PVTI_correct",
										"project": map[string]interface{}{
											"id": "PVT_target",
										},
										"fieldValueByName": map[string]interface{}{
											"name": "Plan",
										},
									},
								},
							},
						},
					},
				},
			}
		})

		itemID, status, err := client.LookupIssueProjectItem("PVT_target", "owner/repo", 7)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if itemID != "PVTI_correct" {
			t.Errorf("itemID = %q, want %q", itemID, "PVTI_correct")
		}
		if status != "Plan" {
			t.Errorf("status = %q, want %q", status, "Plan")
		}
	})

	t.Run("issue not on project returns empty strings and no error", func(t *testing.T) {
		_, client := makeLookupServer(t, func() interface{} {
			return map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"issue": map[string]interface{}{
							"projectItems": map[string]interface{}{
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_other",
										"project": map[string]interface{}{
											"id": "PVT_other",
										},
										"fieldValueByName": nil,
									},
								},
							},
						},
					},
				},
			}
		})

		itemID, status, err := client.LookupIssueProjectItem("PVT_target", "owner/repo", 1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if itemID != "" {
			t.Errorf("itemID = %q, want empty", itemID)
		}
		if status != "" {
			t.Errorf("status = %q, want empty", status)
		}
	})

	t.Run("GraphQL error returns non-nil error", func(t *testing.T) {
		_, client := makeLookupServer(t, func() interface{} {
			return map[string]interface{}{
				"errors": []interface{}{
					map[string]interface{}{"message": "some graphql error"},
				},
			}
		})

		_, _, err := client.LookupIssueProjectItem("PVT_target", "owner/repo", 1)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("no status field value returns empty status", func(t *testing.T) {
		_, client := makeLookupServer(t, func() interface{} {
			return map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"issue": map[string]interface{}{
							"projectItems": map[string]interface{}{
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_nostatus",
										"project": map[string]interface{}{
											"id": "PVT_target",
										},
										"fieldValueByName": nil,
									},
								},
							},
						},
					},
				},
			}
		})

		itemID, status, err := client.LookupIssueProjectItem("PVT_target", "owner/repo", 5)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if itemID != "PVTI_nostatus" {
			t.Errorf("itemID = %q, want %q", itemID, "PVTI_nostatus")
		}
		if status != "" {
			t.Errorf("status = %q, want empty", status)
		}
	})

	t.Run("invalid repo format returns error", func(t *testing.T) {
		// No server needed — should fail before making any HTTP call.
		client := NewClientWithBaseURL("token", "http://localhost:0")
		_, _, err := client.LookupIssueProjectItem("PVT_target", "noslash", 1)
		if err == nil {
			t.Fatal("expected error for invalid repo, got nil")
		}
	})
}
