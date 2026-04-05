package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchItemDetails_Comments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"comments": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{
								"id":        "C_1",
								"author":    map[string]interface{}{"login": "alice"},
								"body":      "First comment",
								"createdAt": "2024-01-15T10:00:00Z",
							},
							map[string]interface{}{
								"id":        "C_2",
								"author":    map[string]interface{}{"login": "bob"},
								"body":      "Second comment",
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
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	if len(item.Comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(item.Comments))
	}
	if item.Comments[0].Body != "First comment" || item.Comments[0].Author != "alice" {
		t.Errorf("comment[0] = %+v", item.Comments[0])
	}
	if item.Comments[1].Body != "Second comment" || item.Comments[1].Author != "bob" {
		t.Errorf("comment[1] = %+v", item.Comments[1])
	}
}

func TestFetchItemDetails_CommentOverflow(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		if _, hasCursor := vars["cursor"]; !hasCursor || vars["cursor"] == nil {
			// First call: FetchItemDetails main query
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
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
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			// Overflow query
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
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (main + overflow), got %d", callCount)
	}
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

func TestFetchItemDetails_LinkedPRCommentOverflow(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := readVars(r)
		callCount++

		if _, hasCursor := vars["cursor"]; !hasCursor || vars["cursor"] == nil {
			// First call: FetchItemDetails main query
			resp := map[string]interface{}{
				"data": map[string]interface{}{
					"node": map[string]interface{}{
						"comments": map[string]interface{}{
							"nodes":    []interface{}{},
							"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
						},
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
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			// PR comment overflow query
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
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (main + PR overflow), got %d", callCount)
	}
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

func TestFetchItemDetails_NilAuthor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"comments": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{
								"id":        "C_1",
								"author":    nil,
								"body":      "ghost comment",
								"createdAt": "",
							},
						},
						"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if len(item.Comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(item.Comments))
	}
	if item.Comments[0].Author != "" {
		t.Errorf("Comment Author = %q, want empty", item.Comments[0].Author)
	}
}
