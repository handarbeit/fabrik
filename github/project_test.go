package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchProjectBoard_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
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
										"comments": map[string]interface{}{
											"nodes": []interface{}{
												map[string]interface{}{
													"id": "C_1",
													"author": map[string]interface{}{
														"login": "alice",
													},
													"body":      "Started work",
													"createdAt": "2024-01-15T10:00:00Z",
												},
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
	board, err := c.FetchProjectBoard("owner", "repo", 1)
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
	if item.Body != "It is broken" {
		t.Errorf("Body = %q", item.Body)
	}
	if item.Status != "In Progress" {
		t.Errorf("Status = %q", item.Status)
	}
	if item.Author != "alice" {
		t.Errorf("Author = %q", item.Author)
	}
	if len(item.Labels) != 2 || item.Labels[0] != "bug" {
		t.Errorf("Labels = %v", item.Labels)
	}
	if len(item.Assignees) != 1 || item.Assignees[0] != "bob" {
		t.Errorf("Assignees = %v", item.Assignees)
	}
	if len(item.Comments) != 1 || item.Comments[0].Author != "alice" {
		t.Errorf("Comments = %v", item.Comments)
	}
	if item.Comments[0].Body != "Started work" {
		t.Errorf("Comment body = %q", item.Comments[0].Body)
	}
}

func TestFetchProjectBoard_SkipsNonIssues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
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
										"id":     "I_real",
										"number": 1,
										"title":  "Real issue",
										"body":   "",
										"url":    "https://example.com",
										"labels":    map[string]interface{}{"nodes": []interface{}{}},
										"assignees": map[string]interface{}{"nodes": []interface{}{}},
										"comments":  map[string]interface{}{"nodes": []interface{}{}},
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
	board, err := c.FetchProjectBoard("owner", "repo", 1)
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
				"repository": map[string]interface{}{
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
										"comments":  map[string]interface{}{"nodes": []interface{}{}},
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
	board, err := c.FetchProjectBoard("owner", "repo", 1)
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
				"repository": map[string]interface{}{
					"projectV2": map[string]interface{}{
						"id": "PVT_123",
						"items": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"id": "PVTI_001",
									"content": map[string]interface{}{
										"id":     "I_1",
										"number": 1,
										"title":  "Ghost author",
										"body":   "",
										"url":    "https://example.com",
										"author": nil,
										"labels":    map[string]interface{}{"nodes": []interface{}{}},
										"assignees": map[string]interface{}{"nodes": []interface{}{}},
										"comments": map[string]interface{}{
											"nodes": []interface{}{
												map[string]interface{}{
													"id":        "C_1",
													"author":    nil,
													"body":      "ghost comment",
													"createdAt": "",
												},
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
	board, err := c.FetchProjectBoard("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if board.Items[0].Author != "" {
		t.Errorf("Author = %q, want empty", board.Items[0].Author)
	}
	if board.Items[0].Comments[0].Author != "" {
		t.Errorf("Comment Author = %q, want empty", board.Items[0].Comments[0].Author)
	}
}

func TestFetchProjectBoard_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("bad-token", srv.URL)
	_, err := c.FetchProjectBoard("owner", "repo", 1)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

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
			"comments":  map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
		},
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
					"repository": map[string]interface{}{
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
					"repository": map[string]interface{}{
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
	board, err := c.FetchProjectBoard("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls, got %d", callCount)
	}
	if len(board.Items) != 3 {
		t.Fatalf("expected 3 items across 2 pages, got %d", len(board.Items))
	}
	if board.Items[0].Title != "Item One" || board.Items[1].Title != "Item Two" || board.Items[2].Title != "Item Three" {
		t.Errorf("unexpected items: %v", board.Items)
	}
}

func TestFetchProjectBoard_CommentOverflow(t *testing.T) {
	// Handler tracks which requests have been made.
	// Request 1: main items query (no cursor) — item with comments hasNextPage=true
	// Request 2: node comments query (startCursor="c_overflow") — returns extra comment
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		// Distinguish main query (has "owner") from node query (has "id").
		if _, isMain := vars["owner"]; isMain {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"pageInfo": map[string]interface{}{
									"hasNextPage": false,
									"endCursor":   "",
								},
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_1",
										"content": map[string]interface{}{
											"id":        "I_1",
											"number":    1,
											"title":     "Issue with many comments",
											"body":      "",
											"url":       "https://example.com",
											"labels":    map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
											"assignees": map[string]interface{}{"nodes": []interface{}{}},
											"comments": map[string]interface{}{
												"nodes": []interface{}{
													map[string]interface{}{
														"id":        "C_1",
														"author":    map[string]interface{}{"login": "alice"},
														"body":      "First comment",
														"createdAt": "2024-01-15T10:00:00Z",
													},
												},
												"pageInfo": map[string]interface{}{
													"hasNextPage": true,
													"endCursor":   "c_overflow",
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
		} else {
			// Node comments overflow query
			nodeID, _ := vars["id"].(string)
			if nodeID != "I_1" {
				t.Errorf("unexpected node ID: %q", nodeID)
			}
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"comments": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"id":        "C_2",
									"author":    map[string]interface{}{"login": "bob"},
									"body":      "Overflow comment",
									"createdAt": "2024-01-16T10:00:00Z",
								},
							},
							"pageInfo": map[string]interface{}{
								"hasNextPage": false,
								"endCursor":   "",
							},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (main + overflow), got %d", callCount)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	item := board.Items[0]
	if len(item.Comments) != 2 {
		t.Fatalf("expected 2 comments (1 main + 1 overflow), got %d", len(item.Comments))
	}
	if item.Comments[0].Body != "First comment" {
		t.Errorf("comment[0].Body = %q", item.Comments[0].Body)
	}
	if item.Comments[1].Body != "Overflow comment" {
		t.Errorf("comment[1].Body = %q", item.Comments[1].Body)
	}
}

func TestFetchProjectBoard_LabelOverflow(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		if _, isMain := vars["owner"]; isMain {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_1",
										"content": map[string]interface{}{
											"id":        "I_1",
											"number":    1,
											"title":     "Issue with many labels",
											"body":      "",
											"url":       "https://example.com",
											"labels": map[string]interface{}{
												"nodes":    []interface{}{map[string]interface{}{"name": "bug"}},
												"pageInfo": map[string]interface{}{"hasNextPage": true, "endCursor": "l_overflow"},
											},
											"assignees": map[string]interface{}{"nodes": []interface{}{}},
											"comments":  map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
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
			// Node labels overflow query
			nodeID, _ := vars["id"].(string)
			if nodeID != "I_1" {
				t.Errorf("unexpected node ID: %q", nodeID)
			}
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"labels": map[string]interface{}{
							"nodes":    []interface{}{map[string]interface{}{"name": "priority"}, map[string]interface{}{"name": "v2"}},
							"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (main + overflow), got %d", callCount)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	item := board.Items[0]
	if len(item.Labels) != 3 {
		t.Fatalf("expected 3 labels (1 main + 2 overflow), got %d: %v", len(item.Labels), item.Labels)
	}
	if item.Labels[0] != "bug" || item.Labels[1] != "priority" || item.Labels[2] != "v2" {
		t.Errorf("unexpected labels: %v", item.Labels)
	}
}

func TestFetchProjectBoard_LinkedPRCommentOverflow(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		if _, isMain := vars["owner"]; isMain {
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"projectV2": map[string]interface{}{
							"id": "PVT_123",
							"items": map[string]interface{}{
								"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								"nodes": []interface{}{
									map[string]interface{}{
										"id": "PVTI_1",
										"content": map[string]interface{}{
											"id":        "I_1",
											"number":    1,
											"title":     "Issue with linked PR",
											"body":      "",
											"url":       "https://example.com",
											"labels":    map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
											"assignees": map[string]interface{}{"nodes": []interface{}{}},
											"comments":  map[string]interface{}{"nodes": []interface{}{}, "pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""}},
											"closedByPullRequestsReferences": map[string]interface{}{
												"nodes": []interface{}{
													map[string]interface{}{
														"id":     "PR_5",
														"number": 5,
														"comments": map[string]interface{}{
															"nodes": []interface{}{
																map[string]interface{}{
																	"id":        "PC_1",
																	"author":    map[string]interface{}{"login": "alice"},
																	"body":      "PR comment 1",
																	"createdAt": "2024-01-15T10:00:00Z",
																},
															},
															"pageInfo": map[string]interface{}{"hasNextPage": true, "endCursor": "pc_overflow"},
														},
													},
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
		} else {
			// Node comments overflow query (for the linked PR)
			nodeID, _ := vars["id"].(string)
			if nodeID != "PR_5" {
				t.Errorf("unexpected node ID: %q", nodeID)
			}
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"comments": map[string]interface{}{
							"nodes": []interface{}{
								map[string]interface{}{
									"id":        "PC_2",
									"author":    map[string]interface{}{"login": "bob"},
									"body":      "PR overflow comment",
									"createdAt": "2024-01-16T10:00:00Z",
								},
							},
							"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("owner", "repo", 1)
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (main + PR overflow), got %d", callCount)
	}
	if len(board.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(board.Items))
	}
	item := board.Items[0]
	if len(item.Comments) != 2 {
		t.Fatalf("expected 2 PR comments (1 main + 1 overflow), got %d", len(item.Comments))
	}
	if item.Comments[0].Body != "PR comment 1" || item.Comments[0].FromPR != 5 {
		t.Errorf("comment[0] = %+v", item.Comments[0])
	}
	if item.Comments[1].Body != "PR overflow comment" || item.Comments[1].FromPR != 5 {
		t.Errorf("comment[1] = %+v", item.Comments[1])
	}
}
