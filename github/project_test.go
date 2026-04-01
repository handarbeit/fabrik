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
