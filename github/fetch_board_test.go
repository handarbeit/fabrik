// Copyright (c) 2026 Fabrik Contributors. All rights reserved.

package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchProjectBoard_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"id": "PVTI_001",
									"fieldValueByName": map[string]interface{}{
										"name": "In Progress",
									},
									"content": map[string]interface{}{
										"id":     "I_abc",
										"number": 42,
										"title":  "Fix the bug",
										"body":   "It is broken",
										"url":    "https://github.com/owner/repo/issues/42",
										"author": map[string]interface{}{
											"login": "alice",
										},
										"labels": map[string]interface{}{
											"nodes": []interface{}{
												map[string]interface{}{"name": "bug"},
												map[string]interface{}{"name": "priority"},
											},
										},
										"assignees": map[string]interface{}{
											"nodes": []interface{}{
												map[string]interface{}{"login": "bob"},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if board.ProjectID != "PVT_123" {
		t.Errorf("ProjectID = %q", board.ProjectID)
	}
	if len(board.Items) != 1 {
		t.Fatalf("items count = %d, want 1", len(board.Items))
	}

	item := board.Items[0]
	if item.ID != "I_abc" {
		t.Errorf("ID = %q", item.ID)
	}
	if item.ItemID != "PVTI_001" {
		t.Errorf("ItemID = %q", item.ItemID)
	}
	if item.Number != 42 {
		t.Errorf("Number = %d", item.Number)
	}
	if item.Title != "Fix the bug" {
		t.Errorf("Title = %q", item.Title)
	}
	if item.Status != "In Progress" {
		t.Errorf("Status = %q", item.Status)
	}
	// Shallow query retains labels(first:5) for cleanupClosedIssueLocks.
	// Response includes 2 labels so both should be present.
	if len(item.Labels) != 2 || item.Labels[0] != "bug" {
		t.Errorf("Labels = %v", item.Labels)
	}
	// Body, Author, Assignees are not populated from the shallow board query —
	// they are fetched in FetchItemDetails (deep fetch).
	if item.Body != "" {
		t.Errorf("Body = %q, want empty (shallow query does not populate body)", item.Body)
	}
	if item.Author != "" {
		t.Errorf("Author = %q, want empty (shallow query does not populate author)", item.Author)
	}
	if len(item.Assignees) != 0 {
		t.Errorf("Assignees = %v, want empty (shallow query does not populate assignees)", item.Assignees)
	}
	// Shallow query does not populate comments.
	if len(item.Comments) != 0 {
		t.Errorf("expected no comments from shallow fetch, got %d", len(item.Comments))
	}
}

func TestFetchProjectBoard_SkipsNonIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"nodes": []interface{}{
								// Draft issue (no content.id)
								map[string]interface{}{
									"id":               "PVTI_draft",
									"fieldValueByName": nil,
									"content":          map[string]interface{}{},
								},
								// Real issue
								map[string]interface{}{
									"id": "PVTI_real",
									"content": map[string]interface{}{
										"id":        "I_real",
										"number":    1,
										"title":     "Real issue",
										"body":      "",
										"url":       "https://example.com",
										"labels":    map[string]interface{}{"nodes": []interface{}{}},
										"assignees": map[string]interface{}{"nodes": []interface{}{}},
									},
								},
							},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Errorf("expected 1 item (skipping draft), got %d", len(board.Items))
	}
}

func TestFetchProjectBoard_NoStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"id":               "PVTI_001",
									"fieldValueByName": nil,
									"content": map[string]interface{}{
										"id":        "I_1",
										"number":    1,
										"title":     "No status",
										"body":      "",
										"url":       "https://example.com",
										"labels":    map[string]interface{}{"nodes": []interface{}{}},
										"assignees": map[string]interface{}{"nodes": []interface{}{}},
									},
								},
							},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if board.Items[0].Status != "" {
		t.Errorf("Status = %q, want empty", board.Items[0].Status)
	}
}

func TestFetchProjectBoard_NilAuthor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"id": "PVTI_001",
									"content": map[string]interface{}{
										"id":        "I_1",
										"number":    1,
										"title":     "Ghost author",
										"body":      "",
										"url":       "https://example.com",
										"author":    nil,
										"labels":    map[string]interface{}{"nodes": []interface{}{}},
										"assignees": map[string]interface{}{"nodes": []interface{}{}},
									},
								},
							},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if board.Items[0].Author != "" {
		t.Errorf("Author = %q, want empty", board.Items[0].Author)
	}
}

func TestFetchProjectBoard_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("bad-token", srv.URL)
	_, err := c.FetchProjectBoard("owner", "repo", 1, "")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestFetchProjectBoard_ItemsPagination(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		cursor, _ := vars["cursor"].(string)
		if cursor == "" {
			// Page 1: two items, hasNextPage=true
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"user": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"pageInfo": map[string]interface{}{
									"hasNextPage": true,
									"endCursor":   "cursor_page2",
								},
								"nodes": []interface{}{
									makeItem("I_1", "PVTI_1", "Item One"),
									makeItem("I_2", "PVTI_2", "Item Two"),
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		} else if cursor == "cursor_page2" {
			// Page 2: one item, hasNextPage=false
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"user": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"pageInfo": map[string]interface{}{
									"hasNextPage": false,
									"endCursor":   "",
								},
								"nodes": []interface{}{
									makeItem("I_3", "PVTI_3", "Item Three"),
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			t.Errorf("unexpected cursor: %q", cursor)
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if callCount != 3 {
		t.Errorf("expected 3 API calls (org fail + 2 pages), got %d", callCount)
	}
	if len(board.Items) != 3 {
		t.Fatalf("expected 3 items across 2 pages, got %d", len(board.Items))
	}
	if board.Items[0].Title != "Item One" || board.Items[1].Title != "Item Two" || board.Items[2].Title != "Item Three" {
		t.Errorf("unexpected items: %v", board.Items)
	}
}

// TestFetchProjectBoard_NoLabelOverflow verifies that the shallow board query
// does NOT paginate labels even when hasNextPage=true. The shallow query fetches
// only labels(first:5) and intentionally skips pagination to minimize GraphQL
// cost. Full labels are fetched in FetchItemDetails (deep fetch).
func TestFetchProjectBoard_NoLabelOverflow(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		if _, isMain := vars["owner"]; isMain {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"user": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_1",
										"content": map[string]interface{}{
											"id":     "I_1",
											"number": 1,
											"title":  "Issue with many labels",
											"labels": map[string]interface{}{
												"nodes": []interface{}{map[string]interface{}{"name": "bug"}},
												// hasNextPage=true but shallow board must NOT follow it
											},
										},
									},
								},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			t.Errorf("unexpected second API call in shallow board fetch (labels should not be paginated)")
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()

	// Use ownerType="user" to get a single API call (skip org-then-user fallback).
	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	// Exactly one call: the main board query. No label pagination.
	if callCount != 1 {
		t.Errorf("expected 1 API call (main only), got %d — shallow board must not paginate labels", callCount)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	item := board.Items[0]
	// Only the labels returned in the initial response (no overflow)
	if len(item.Labels) != 1 || item.Labels[0] != "bug" {
		t.Errorf("Labels = %v, want [bug]", item.Labels)
	}
}

func makeMinimalIssueContent(id string, number int, state string) map[string]interface{} {
	return map[string]interface{}{
		"id":        id,
		"number":    number,
		"title":     "Test issue",
		"body":      "",
		"url":       "https://example.com",
		"state":     state,
		"labels":    map[string]interface{}{"nodes": []interface{}{}},
		"assignees": map[string]interface{}{"nodes": []interface{}{}},
	}
}

func makeMinimalPRContent(id string, number int) map[string]interface{} {
	return map[string]interface{}{
		"__typename": "PullRequest",
		"id":         id,
		"number":     number,
		"title":      "Test PR",
		"body":       "",
		"url":        "https://example.com",
		"labels":     map[string]interface{}{"nodes": []interface{}{}},
		"assignees":  map[string]interface{}{"nodes": []interface{}{}},
	}
}

func boardResponseWith(nodes []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"user": map[string]interface{}{
				"projectV2": map[string]interface{}{
					"id": "PVT_test",
					"items": map[string]interface{}{
						"pageInfo": map[string]interface{}{"hasNextPage": false},
						"nodes":    nodes,
					},
				},
			},
		},
	}
}

// TestFetchProjectBoard_IsClosed_ClosedIssue verifies that a closed Issue sets IsClosed=true.
func TestFetchProjectBoard_IsClosed_ClosedIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := boardResponseWith([]interface{}{
			map[string]interface{}{
				"id":               "PVTI_closed",
				"fieldValueByName": nil,
				"content":          makeMinimalIssueContent("I_closed", 99, "CLOSED"),
			},
		})
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	if !board.Items[0].IsClosed {
		t.Error("IsClosed should be true for CLOSED issue")
	}
}

// TestFetchProjectBoard_IsClosed_OpenIssue verifies that an open Issue sets IsClosed=false.
func TestFetchProjectBoard_IsClosed_OpenIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := boardResponseWith([]interface{}{
			map[string]interface{}{
				"id":               "PVTI_open",
				"fieldValueByName": nil,
				"content":          makeMinimalIssueContent("I_open", 7, "OPEN"),
			},
		})
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	if board.Items[0].IsClosed {
		t.Error("IsClosed should be false for OPEN issue")
	}
}

// TestFetchProjectBoard_IsClosed_PRItem verifies that a PR item always has IsClosed=false.
func TestFetchProjectBoard_IsClosed_PRItem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := boardResponseWith([]interface{}{
			map[string]interface{}{
				"id":               "PVTI_pr",
				"fieldValueByName": nil,
				"content":          makeMinimalPRContent("PR_1", 55),
			},
		})
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	item := board.Items[0]
	if !item.IsPR {
		t.Error("IsPR should be true for PullRequest item")
	}
	if item.IsClosed {
		t.Error("IsClosed should be false for PR item (PR state is not fetched here)")
	}
}
