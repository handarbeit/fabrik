package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchProjectBoard_Success serves github/testdata/recordings/fetch_project_board.json
// — a real response recorded from the live handarbeit/fabrik board (#1453 R2/R5) — instead
// of a hand-authored fixture literal.
//
// AC5 finding: the hand-authored fixture this test used to serve included
// "body", "url", "author", and "assignees" keys on the issue content that
// the real fetchProjectBoardQueryTemplate query (github/project.go) never
// requests, and that itemNode (also project.go) has no struct fields to
// receive — GitHub would never actually return those keys for this query.
// The assertions below already documented this ("shallow query does not
// populate body/author/assignees") but the fixture silently included fields
// the real response can't contain; the recording only has the fields the
// query actually asks for.
func TestFetchProjectBoard_Success(t *testing.T) {
	raw := loadRecording(t, "fetch_project_board")

	var recorded struct {
		Data struct {
			Organization struct {
				ProjectV2 struct {
					Items struct {
						Nodes []json.RawMessage `json:"nodes"`
					} `json:"items"`
				} `json:"projectV2"`
			} `json:"organization"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("parsing recorded response: %v", err)
	}
	realNodeCount := len(recorded.Data.Organization.ProjectV2.Items.Nodes)
	if realNodeCount == 0 {
		t.Fatal("recorded fixture has no items to test against")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(raw)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("handarbeit", "fabrik", 1, "")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if board.ProjectID != "PVT_kwDOENB0Ac4BW0n_" {
		t.Errorf("ProjectID = %q", board.ProjectID)
	}
	if board.Title != "Fabrik" {
		t.Errorf("Title = %q, want %q", board.Title, "Fabrik")
	}
	if board.OwnerType != "organization" {
		t.Errorf("OwnerType = %q, want %q", board.OwnerType, "organization")
	}
	if len(board.Items) != realNodeCount {
		t.Fatalf("items count = %d, want %d (recorded node count)", len(board.Items), realNodeCount)
	}

	// Spot-check the first recorded item: real content from the live board,
	// not invented values.
	item := board.Items[0]
	if item.ID != "I_kwDOR2bN_c8AAAABL_NaWQ" {
		t.Errorf("ID = %q", item.ID)
	}
	if item.ItemID != "PVTI_lADOENB0Ac4BW0n_zg1zXpw" {
		t.Errorf("ItemID = %q", item.ItemID)
	}
	if item.Number != 1457 {
		t.Errorf("Number = %d", item.Number)
	}
	if item.Status != "Plan" {
		t.Errorf("Status = %q", item.Status)
	}
	if len(item.Labels) == 0 {
		t.Error("Labels = empty, want at least one recorded label")
	}
	// Body, Author, Assignees are not populated from the shallow board query —
	// they are fetched in FetchItemDetails (deep fetch), and (per the AC5
	// finding above) the real query response has no such keys at all.
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

// TestFetchProjectBoard_RetriesOnEmptyResponse verifies that fetchProjectBoard
// retries when GitHub returns 0 items + totalCount=0 (the indexer-degraded
// response shape observed during GitHub Projects v2 outages). The retry is
// meant to mask transient indexer hiccups: hitting the same query twice in
// a row returned 100 items or 0 items at random during the April 2026 outage.
func TestFetchProjectBoard_RetriesOnEmptyResponse(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			// First two attempts: degraded response (empty items + totalCount=0).
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"user": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"totalCount": 0,
								"pageInfo":   map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								"nodes":      []interface{}{},
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}
		// Third attempt: indexer is back, returns the real data.
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"totalCount": 1,
							"pageInfo":   map[string]interface{}{"hasNextPage": false, "endCursor": ""},
							"nodes":      []interface{}{makeItem("I_1", "PVTI_1", "Recovered")},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if callCount != 3 {
		t.Errorf("expected 3 API calls (2 empty + 1 success), got %d", callCount)
	}
	if len(board.Items) != 1 || board.Items[0].Title != "Recovered" {
		t.Errorf("expected 1 item titled 'Recovered', got: %v", board.Items)
	}
}

// TestFetchProjectBoard_AcceptsGenuinelyEmpty verifies that a project that
// reports zero items consistently across all retry attempts is accepted as
// genuinely empty (not treated as a permanent error).
func TestFetchProjectBoard_AcceptsGenuinelyEmpty(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"user": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"totalCount": 0,
							"pageInfo":   map[string]interface{}{"hasNextPage": false, "endCursor": ""},
							"nodes":      []interface{}{},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if callCount != projectBoardFetchAttempts {
		t.Errorf("expected %d API calls (all retries exhausted), got %d", projectBoardFetchAttempts, callCount)
	}
	if len(board.Items) != 0 {
		t.Errorf("expected empty board, got %d items", len(board.Items))
	}
}

// TestFetchProjectBoard_LinkedPRNumberShallow_Populated verifies that
// LinkedPRNumberShallow is set from the first closedByPullRequestsReferences node.
func TestFetchProjectBoard_LinkedPRNumberShallow_Populated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := boardResponseWith([]interface{}{
			map[string]interface{}{
				"id":               "PVTI_001",
				"fieldValueByName": nil,
				"content": map[string]interface{}{
					"id":     "I_1",
					"number": 1,
					"title":  "Issue with linked PR",
					"labels": map[string]interface{}{"nodes": []interface{}{}},
					"closedByPullRequestsReferences": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{
								"updatedAt": "2026-01-01T00:00:00Z",
								"number":    502,
							},
						},
					},
				},
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
	if board.Items[0].LinkedPRNumberShallow != 502 {
		t.Errorf("LinkedPRNumberShallow = %d, want 502", board.Items[0].LinkedPRNumberShallow)
	}
}

// TestFetchProjectBoard_LinkedPRNumberShallow_Empty verifies that
// LinkedPRNumberShallow stays 0 when closedByPullRequestsReferences is absent or empty.
func TestFetchProjectBoard_LinkedPRNumberShallow_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := boardResponseWith([]interface{}{
			map[string]interface{}{
				"id":               "PVTI_001",
				"fieldValueByName": nil,
				"content": map[string]interface{}{
					"id":     "I_1",
					"number": 1,
					"title":  "Issue without linked PR",
					"labels": map[string]interface{}{"nodes": []interface{}{}},
					"closedByPullRequestsReferences": map[string]interface{}{
						"nodes": []interface{}{},
					},
				},
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
	if board.Items[0].LinkedPRNumberShallow != 0 {
		t.Errorf("LinkedPRNumberShallow = %d, want 0 when no linked PRs", board.Items[0].LinkedPRNumberShallow)
	}
}
