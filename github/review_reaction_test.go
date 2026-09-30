package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAddReviewReaction_RequestShape pins the exact GraphQL variables sent for
// a PullRequestReview reaction (#1953 R8): the review's node ID as subjectId
// and the *uppercase* GraphQL enum, not REST's lowercase name.
//
// The server replies with the real recorded addReaction response
// (testdata/recordings/add_review_reaction.json, captured against a sandbox PR
// review by scripts/wire-contract/record-fixtures.sh). The operation itself is
// validated against the vendored schema by
// TestWireContract_AllQueriesValidateAgainstSchema.
func TestAddReviewReaction_RequestShape(t *testing.T) {
	tests := []struct{ in, want string }{
		{"eyes", "EYES"},
		{"rocket", "ROCKET"},
		{"+1", "THUMBS_UP"},
	}
	recorded := loadRecording(t, "add_review_reaction")
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			var got struct {
				Query     string `json:"query"`
				Variables struct {
					SubjectID string `json:"subjectId"`
					Content   string `json:"content"`
				} `json:"variables"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/graphql" {
					t.Errorf("path = %s, want /graphql", r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decoding request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write(recorded)
			}))
			defer srv.Close()

			c := NewClientWithBaseURL("token", srv.URL)
			if err := c.AddReviewReaction("PRR_kwDOabc", tc.in); err != nil {
				t.Fatalf("AddReviewReaction: %v", err)
			}
			if got.Variables.SubjectID != "PRR_kwDOabc" {
				t.Errorf("subjectId = %q, want PRR_kwDOabc", got.Variables.SubjectID)
			}
			if got.Variables.Content != tc.want {
				t.Errorf("content = %q, want %q", got.Variables.Content, tc.want)
			}
			if !strings.Contains(got.Query, "addReaction") {
				t.Errorf("query does not call addReaction: %s", got.Query)
			}
		})
	}
}

func TestAddReviewReaction_RejectsBadInputWithoutRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddReviewReaction("PRR_x", "bogus"); err == nil {
		t.Error("unknown content: expected error")
	}
	if err := c.AddReviewReaction("", "eyes"); err == nil {
		t.Error("empty node ID: expected error")
	}
}

func TestAddReviewReaction_GraphQLErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errors":[{"message":"Resource not accessible by integration"}]}`))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	if err := c.AddReviewReaction("PRR_x", "eyes"); err == nil {
		t.Error("expected GraphQL error to propagate")
	}
}

func TestFetchPRReviews_CarriesNodeID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]map[string]interface{}{
			{"id": 7, "node_id": "PRR_kwDO7", "user": map[string]string{"login": "alice"}, "state": "COMMENTED", "body": "x"},
		})
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	reviews, err := c.FetchPRReviews("owner", "repo", 1)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("FetchPRReviews: %v, %+v", err, reviews)
	}
	if reviews[0].NodeID != "PRR_kwDO7" || reviews[0].DatabaseID != 7 {
		t.Errorf("review = %+v, want NodeID PRR_kwDO7 / DatabaseID 7", reviews[0])
	}
}

func TestFetchItemDetails_LatestReviewsCarryNodeID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
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
								"latestReviews": map[string]interface{}{"nodes": []interface{}{
									map[string]interface{}{
										"id":          "PRR_kwDOabc",
										"databaseId":  5356295491,
										"author":      map[string]interface{}{"__typename": "Bot", "login": "handarbeit-pruefer"},
										"state":       "COMMENTED",
										"body":        "finding",
										"submittedAt": "2026-09-29T17:48:12Z",
									},
								}},
								"reviewThreads": map[string]interface{}{"nodes": []interface{}{}},
							},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	item := &ProjectItem{ID: "I_1", Number: 1}
	if err := c.FetchItemDetails(item); err != nil {
		t.Fatalf("FetchItemDetails: %v", err)
	}
	if len(item.LinkedPRReviews) != 1 || item.LinkedPRReviews[0].NodeID != "PRR_kwDOabc" {
		t.Fatalf("LinkedPRReviews = %+v, want one review with NodeID PRR_kwDOabc", item.LinkedPRReviews)
	}
}
