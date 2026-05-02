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

// TestFetchItemDetails_PopulatesFullMetadata verifies that FetchItemDetails
// populates body, url, author, labels, assignees, and blockedBy from the
// deep-fetch response in addition to comments.
func TestFetchItemDetails_PopulatesFullMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"body": "This is the issue body.",
					"url":  "https://github.com/owner/repo/issues/42",
					"author": map[string]interface{}{
						"login": "authoruser",
					},
					"labels": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{"name": "bug"},
							map[string]interface{}{"name": "stage:Research:complete"},
						},
						"pageInfo": map[string]interface{}{
							"hasNextPage": false,
							"endCursor":   "",
						},
					},
					"assignees": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{"login": "assignee1"},
						},
					},
					"blockedBy": map[string]interface{}{
						"pageInfo": map[string]interface{}{"hasNextPage": false},
						"nodes": []interface{}{
							map[string]interface{}{
								"number": 10,
								"state":  "OPEN",
								"repository": map[string]interface{}{
									"nameWithOwner": "owner/repo",
								},
							},
						},
					},
					"comments": map[string]interface{}{
						"nodes":    []interface{}{},
						"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_42", Number: 42}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	if item.Body != "This is the issue body." {
		t.Errorf("Body = %q, want %q", item.Body, "This is the issue body.")
	}
	if item.URL != "https://github.com/owner/repo/issues/42" {
		t.Errorf("URL = %q, want URL", item.URL)
	}
	if item.Author != "authoruser" {
		t.Errorf("Author = %q, want %q", item.Author, "authoruser")
	}
	if len(item.Labels) != 2 || item.Labels[0] != "bug" || item.Labels[1] != "stage:Research:complete" {
		t.Errorf("Labels = %v, want [bug stage:Research:complete]", item.Labels)
	}
	if len(item.Assignees) != 1 || item.Assignees[0] != "assignee1" {
		t.Errorf("Assignees = %v, want [assignee1]", item.Assignees)
	}
	if len(item.BlockedBy) != 1 {
		t.Fatalf("BlockedBy = %v, want 1 entry", item.BlockedBy)
	}
	if item.BlockedBy[0].Number != 10 || item.BlockedBy[0].State != "OPEN" || item.BlockedBy[0].Repo != "owner/repo" {
		t.Errorf("BlockedBy[0] = %+v", item.BlockedBy[0])
	}
}

// TestFetchItemDetails_ResetsLabelsFromShallow verifies that FetchItemDetails
// resets item.Labels (clearing any shallow-fetch labels) before populating from
// the deep-fetch response. This prevents label duplication.
func TestFetchItemDetails_ResetsLabelsFromShallow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"labels": map[string]interface{}{
						"nodes": []interface{}{
							map[string]interface{}{"name": "deep-label"},
						},
						"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
					},
					"assignees": map[string]interface{}{"nodes": []interface{}{}},
					"comments": map[string]interface{}{
						"nodes":    []interface{}{},
						"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	// Pre-populate with shallow labels (simulating what fetchProjectBoard sets)
	item := &ProjectItem{ID: "I_1", Number: 1, Labels: []string{"shallow-label"}}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	// Should only have the deep-fetched label, not the shallow one
	if len(item.Labels) != 1 || item.Labels[0] != "deep-label" {
		t.Errorf("Labels = %v, want [deep-label] (shallow labels should be replaced)", item.Labels)
	}
}

// TestFetchItemDetails_ReviewThreadLocationFields verifies that location fields
// (path, line, originalLine, diffHunk) on PR review thread comment nodes are
// correctly decoded and surfaced in LinkedPRReviewThreadComments.
func TestFetchItemDetails_ReviewThreadLocationFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
								"id":     "PR_10",
								"number": 10,
								"comments": map[string]interface{}{
									"nodes":    []interface{}{},
									"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								},
								"reviewRequests": map[string]interface{}{"nodes": []interface{}{}},
								"latestReviews":  map[string]interface{}{"nodes": []interface{}{}},
								"reviewThreads": map[string]interface{}{
									"nodes": []interface{}{
										map[string]interface{}{
											"id":           "RT_abc123",
											"isResolved":   false,
											"path":         "engine/claude.go",
											"line":         243,
											"originalLine": 240,
											"diffSide":     "RIGHT",
											"comments": map[string]interface{}{
												"nodes": []interface{}{
													map[string]interface{}{
														"id":             "PRC_1",
														"databaseId":     1001,
														"author":         map[string]interface{}{"login": "copilot"},
														"body":           "Fix error handling here",
														"createdAt":      "2026-01-15T10:30:00Z",
														"diffHunk":       "@@ -241,7 +241,7 @@\n-\tfoo()\n+\tbar()",
														"path":           "engine/claude.go",
														"line":           243,
														"originalLine":   240,
														"reactionGroups": []interface{}{},
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
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	if len(item.LinkedPRReviewThreadComments) != 1 {
		t.Fatalf("expected 1 review thread comment, got %d", len(item.LinkedPRReviewThreadComments))
	}
	c2 := item.LinkedPRReviewThreadComments[0]
	if c2.ReviewThreadID != "RT_abc123" {
		t.Errorf("ReviewThreadID = %q, want %q", c2.ReviewThreadID, "RT_abc123")
	}
	if c2.Path != "engine/claude.go" {
		t.Errorf("Path = %q, want %q", c2.Path, "engine/claude.go")
	}
	if c2.Line != 243 {
		t.Errorf("Line = %d, want 243", c2.Line)
	}
	if c2.OriginalLine != 240 {
		t.Errorf("OriginalLine = %d, want 240", c2.OriginalLine)
	}
	if c2.DiffHunk != "@@ -241,7 +241,7 @@\n-\tfoo()\n+\tbar()" {
		t.Errorf("DiffHunk = %q, want diff hunk", c2.DiffHunk)
	}
	if c2.Author != "copilot" {
		t.Errorf("Author = %q, want %q", c2.Author, "copilot")
	}
	if c2.Body != "Fix error handling here" {
		t.Errorf("Body = %q, want %q", c2.Body, "Fix error handling here")
	}
	if c2.FromPR != 10 {
		t.Errorf("FromPR = %d, want 10", c2.FromPR)
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

// TestFetchItemDetails_LinkedPRNumber_AndBotDetection verifies that:
// - item.LinkedPRNumber is populated from the first linked PR's number field
// - ReviewRequest.IsBot is set from __typename == "Bot" (primary signal)
// - ReviewRequest.IsBot falls back to isBotLogin() when __typename is "User"
func TestFetchItemDetails_LinkedPRNumber_AndBotDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
								"id":     "PR_77",
								"number": 77,
								"comments": map[string]interface{}{
									"nodes":    []interface{}{},
									"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
								},
								"reviewRequests": map[string]interface{}{
									"nodes": []interface{}{
										// Bot via __typename
										map[string]interface{}{
											"requestedReviewer": map[string]interface{}{
												"__typename": "Bot",
												"login":      "copilot-pull-request-reviewer",
											},
										},
										// Human via __typename
										map[string]interface{}{
											"requestedReviewer": map[string]interface{}{
												"__typename": "User",
												"login":      "alice",
											},
										},
										// Bot via login pattern fallback (__typename mismatch)
										map[string]interface{}{
											"requestedReviewer": map[string]interface{}{
												"__typename": "User",
												"login":      "dependabot",
											},
										},
									},
								},
								"latestReviews": map[string]interface{}{"nodes": []interface{}{}},
								"reviewThreads": map[string]interface{}{"nodes": []interface{}{}},
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
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}

	if item.LinkedPRNumber != 77 {
		t.Errorf("LinkedPRNumber = %d, want 77", item.LinkedPRNumber)
	}
	if len(item.LinkedPRReviewRequests) != 3 {
		t.Fatalf("LinkedPRReviewRequests = %d, want 3", len(item.LinkedPRReviewRequests))
	}

	// copilot-pull-request-reviewer: __typename == "Bot" → IsBot
	if item.LinkedPRReviewRequests[0].Login != "copilot-pull-request-reviewer" || !item.LinkedPRReviewRequests[0].IsBot {
		t.Errorf("rr[0] = %+v, want {Login:copilot-pull-request-reviewer IsBot:true}", item.LinkedPRReviewRequests[0])
	}
	// alice: __typename == "User" → not a bot
	if item.LinkedPRReviewRequests[1].Login != "alice" || item.LinkedPRReviewRequests[1].IsBot {
		t.Errorf("rr[1] = %+v, want {Login:alice IsBot:false}", item.LinkedPRReviewRequests[1])
	}
	// dependabot: login-pattern fallback → IsBot even though __typename == "User"
	if item.LinkedPRReviewRequests[2].Login != "dependabot" || !item.LinkedPRReviewRequests[2].IsBot {
		t.Errorf("rr[2] = %+v, want {Login:dependabot IsBot:true}", item.LinkedPRReviewRequests[2])
	}
}

// TestFetchItemDetails_LinkedPRNumber_ResetOnRepeat verifies that LinkedPRNumber
// is reset to 0 on repeated FetchItemDetails calls when no linked PRs are present.
func TestFetchItemDetails_LinkedPRNumber_ResetOnRepeat(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var linkedPRs interface{}
		if callCount == 1 {
			// First call: linked PR present
			linkedPRs = map[string]interface{}{
				"nodes": []interface{}{
					map[string]interface{}{
						"id":     "PR_55",
						"number": 55,
						"comments": map[string]interface{}{
							"nodes":    []interface{}{},
							"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
						},
						"reviewRequests": map[string]interface{}{"nodes": []interface{}{}},
						"latestReviews":  map[string]interface{}{"nodes": []interface{}{}},
						"reviewThreads":  map[string]interface{}{"nodes": []interface{}{}},
					},
				},
			}
		} else {
			// Second call: no linked PRs
			linkedPRs = nil
		}
		resp := map[string]interface{}{
			"data": map[string]interface{}{
				"node": map[string]interface{}{
					"comments": map[string]interface{}{
						"nodes":    []interface{}{},
						"pageInfo": map[string]interface{}{"hasNextPage": false, "endCursor": ""},
					},
					"closedByPullRequestsReferences": linkedPRs,
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_1", Number: 1}

	// First call: LinkedPRNumber should be set
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("first FetchItemDetails: %v", err)
	}
	if item.LinkedPRNumber != 55 {
		t.Errorf("after first call: LinkedPRNumber = %d, want 55", item.LinkedPRNumber)
	}

	// Second call: LinkedPRNumber should be reset to 0
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("second FetchItemDetails: %v", err)
	}
	if item.LinkedPRNumber != 0 {
		t.Errorf("after second call: LinkedPRNumber = %d, want 0 (should be reset)", item.LinkedPRNumber)
	}
}
