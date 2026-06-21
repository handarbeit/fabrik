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

// minimalLinkedPRNode builds the shared fields for a linked-PR node in the
// FetchItemDetails GraphQL mock response, avoiding repetition across sub-tests.
func minimalLinkedPRNode(id string, number int, extra map[string]interface{}) map[string]interface{} {
	node := map[string]interface{}{
		"id":         id,
		"number":     number,
		"headRefOid": "sha" + id,
		"comments":   map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
		"reviewRequests": map[string]interface{}{"nodes": []interface{}{}},
		"latestReviews":  map[string]interface{}{"nodes": []interface{}{}},
		"reviewThreads":  map[string]interface{}{"nodes": []interface{}{}},
	}
	for k, v := range extra {
		node[k] = v
	}
	return node
}

// minimalFetchItemDetailsResponse builds a complete FetchItemDetails GraphQL
// response wrapping the given linked-PR nodes.
func minimalFetchItemDetailsResponse(linkedPRNodes []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"node": map[string]interface{}{
				"number": 5,
				"title":  "Test issue",
				"body":   "",
				"url":    "https://github.com/test/repo/issues/5",
				"author": map[string]interface{}{"login": "alice"},
				"labels": map[string]interface{}{
					"nodes":    []interface{}{},
					"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
				},
				"assignees": map[string]interface{}{"nodes": []interface{}{}},
				"comments": map[string]interface{}{
					"nodes":    []interface{}{},
					"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
				},
				"closedByPullRequestsReferences": map[string]interface{}{
					"nodes": linkedPRNodes,
				},
			},
		},
	}
}

func TestFetchItemDetailsQueueFields(t *testing.T) {
	t.Run("queue fields are populated when mergeQueueEntry is non-null", func(t *testing.T) {
		prNode := minimalLinkedPRNode("PR_xyz", 7, map[string]interface{}{
			"isMergeQueueEnabled": true,
			"isInMergeQueue":      true,
			"mergeQueueEntry": map[string]interface{}{
				"state":    "QUEUED",
				"position": 1,
				"enqueuer": map[string]interface{}{"login": "bob"},
			},
		})
		_, client := makeLookupServer(t, func() interface{} {
			return minimalFetchItemDetailsResponse([]interface{}{prNode})
		})

		item := &ProjectItem{ID: "ISSUE_id", Number: 5}
		if err := client.FetchItemDetails(item); err != nil {
			t.Fatalf("FetchItemDetails: %v", err)
		}

		if !item.LinkedPRIsMergeQueueEnabled {
			t.Error("LinkedPRIsMergeQueueEnabled = false, want true")
		}
		if !item.LinkedPRIsInMergeQueue {
			t.Error("LinkedPRIsInMergeQueue = false, want true")
		}
		if item.LinkedPRMergeQueueEntry == nil {
			t.Fatal("LinkedPRMergeQueueEntry = nil, want non-nil")
		}
		if item.LinkedPRMergeQueueEntry.State != "QUEUED" {
			t.Errorf("MergeQueueEntry.State = %q, want %q", item.LinkedPRMergeQueueEntry.State, "QUEUED")
		}
		if item.LinkedPRMergeQueueEntry.Position != 1 {
			t.Errorf("MergeQueueEntry.Position = %d, want 1", item.LinkedPRMergeQueueEntry.Position)
		}
		if item.LinkedPRMergeQueueEntry.EnqueuerLogin != "bob" {
			t.Errorf("MergeQueueEntry.EnqueuerLogin = %q, want %q", item.LinkedPRMergeQueueEntry.EnqueuerLogin, "bob")
		}
	})

	t.Run("queue fields are zero when mergeQueueEntry is null", func(t *testing.T) {
		prNode := minimalLinkedPRNode("PR_abc", 8, map[string]interface{}{
			"isMergeQueueEnabled": false,
			"isInMergeQueue":      false,
			"mergeQueueEntry":     nil,
		})
		_, client := makeLookupServer(t, func() interface{} {
			return minimalFetchItemDetailsResponse([]interface{}{prNode})
		})

		item := &ProjectItem{ID: "ISSUE_id2", Number: 6}
		if err := client.FetchItemDetails(item); err != nil {
			t.Fatalf("FetchItemDetails: %v", err)
		}

		if item.LinkedPRIsMergeQueueEnabled {
			t.Error("LinkedPRIsMergeQueueEnabled = true, want false")
		}
		if item.LinkedPRIsInMergeQueue {
			t.Error("LinkedPRIsInMergeQueue = true, want false")
		}
		if item.LinkedPRMergeQueueEntry != nil {
			t.Errorf("LinkedPRMergeQueueEntry = %+v, want nil", item.LinkedPRMergeQueueEntry)
		}
	})

	t.Run("queue fields are zero when no linked PR", func(t *testing.T) {
		_, client := makeLookupServer(t, func() interface{} {
			return minimalFetchItemDetailsResponse([]interface{}{})
		})

		item := &ProjectItem{ID: "ISSUE_id3", Number: 7}
		if err := client.FetchItemDetails(item); err != nil {
			t.Fatalf("FetchItemDetails: %v", err)
		}

		if item.LinkedPRIsMergeQueueEnabled {
			t.Error("LinkedPRIsMergeQueueEnabled = true, want false (no PR)")
		}
		if item.LinkedPRIsInMergeQueue {
			t.Error("LinkedPRIsInMergeQueue = true, want false (no PR)")
		}
		if item.LinkedPRMergeQueueEntry != nil {
			t.Errorf("LinkedPRMergeQueueEntry non-nil when no PR: %+v", item.LinkedPRMergeQueueEntry)
		}
	})
}
