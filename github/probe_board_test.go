package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// probeItemResponse builds a minimal JSON map representing one node in the
// ProbeProjectBoard response. updatedAt values are RFC3339 strings.
func probeItemResponse(itemID, contentID string, number int, typename, state, contentUpdatedAt, itemUpdatedAt, repo string, linkedPRNumber int, linkedPRUpdatedAt string) map[string]interface{} {
	content := map[string]interface{}{
		"__typename": typename,
		"id":         contentID,
		"number":     number,
		"state":      state,
		"updatedAt":  contentUpdatedAt,
		"repository": map[string]interface{}{"nameWithOwner": repo},
	}
	if linkedPRNumber > 0 {
		content["closedByPullRequestsReferences"] = map[string]interface{}{
			"nodes": []interface{}{
				map[string]interface{}{
					"number":    linkedPRNumber,
					"updatedAt": linkedPRUpdatedAt,
				},
			},
		}
	} else {
		content["closedByPullRequestsReferences"] = map[string]interface{}{
			"nodes": []interface{}{},
		}
	}
	node := map[string]interface{}{
		"id":        itemID,
		"updatedAt": itemUpdatedAt,
		"content":   content,
	}
	return node
}

func probeResponse(ownerType, projectID string, nodes []interface{}, totalCount int, hasNextPage bool, endCursor string) map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			ownerType: map[string]interface{}{
				"projectV2": map[string]interface{}{
					"id": projectID,
					"items": map[string]interface{}{
						"totalCount": totalCount,
						"pageInfo": map[string]interface{}{
							"hasNextPage": hasNextPage,
							"endCursor":   endCursor,
						},
						"nodes": nodes,
					},
				},
			},
		},
	}
}

func TestProbeProjectBoard_Success(t *testing.T) {
	t1 := "2024-01-01T10:00:00Z"
	t2 := "2024-01-01T11:00:00Z" // later than t1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := probeItemResponse("PVTI_1", "I_abc", 42, "Issue", "OPEN", t1, t2, "owner/repo", 0, "")
		resp := probeResponse("user", "PVT_123", []interface{}{node}, 1, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, projectID, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if projectID != "PVT_123" {
		t.Errorf("projectID = %q, want PVT_123", projectID)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	item := items[0]
	if item.ItemID != "PVTI_1" {
		t.Errorf("ItemID = %q, want PVTI_1", item.ItemID)
	}
	if item.ContentID != "I_abc" {
		t.Errorf("ContentID = %q, want I_abc", item.ContentID)
	}
	if item.Number != 42 {
		t.Errorf("Number = %d, want 42", item.Number)
	}
	if item.IsPR {
		t.Error("IsPR should be false for Issue")
	}
	if item.IsClosed {
		t.Error("IsClosed should be false for open issue")
	}
	if item.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want owner/repo", item.Repo)
	}
	// effectiveUpdatedAt = max(contentUpdatedAt, itemUpdatedAt) = t2 (later)
	want, _ := time.Parse(time.RFC3339, t2)
	if !item.EffectiveUpdatedAt.Equal(want) {
		t.Errorf("EffectiveUpdatedAt = %v, want %v", item.EffectiveUpdatedAt, want)
	}
}

func TestProbeProjectBoard_EffectiveUpdatedAtMax(t *testing.T) {
	t1 := "2024-01-01T10:00:00Z" // content updatedAt
	t2 := "2024-01-01T09:00:00Z" // item updatedAt (earlier)
	t3 := "2024-01-01T12:00:00Z" // linkedPR updatedAt (latest — should win)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := probeItemResponse("PVTI_1", "I_abc", 10, "Issue", "OPEN", t1, t2, "owner/repo", 99, t3)
		resp := probeResponse("user", "PVT_123", []interface{}{node}, 1, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	want, _ := time.Parse(time.RFC3339, t3)
	if !items[0].EffectiveUpdatedAt.Equal(want) {
		t.Errorf("EffectiveUpdatedAt = %v, want linkedPR time %v", items[0].EffectiveUpdatedAt, want)
	}
	if items[0].LinkedPRNumber != 99 {
		t.Errorf("LinkedPRNumber = %d, want 99", items[0].LinkedPRNumber)
	}
}

func TestProbeProjectBoard_IsClosed(t *testing.T) {
	ts := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := probeItemResponse("PVTI_1", "I_abc", 7, "Issue", "CLOSED", ts, ts, "owner/repo", 0, "")
		resp := probeResponse("user", "PVT_123", []interface{}{node}, 1, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if !items[0].IsClosed {
		t.Error("IsClosed should be true for CLOSED issue")
	}
	if items[0].IsPR {
		t.Error("IsPR should be false for Issue typename")
	}
}

func TestProbeProjectBoard_PullRequest(t *testing.T) {
	ts := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node := probeItemResponse("PVTI_2", "PR_abc", 55, "PullRequest", "", ts, ts, "owner/repo", 0, "")
		resp := probeResponse("user", "PVT_123", []interface{}{node}, 1, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if !items[0].IsPR {
		t.Error("IsPR should be true for PullRequest typename")
	}
	if items[0].IsClosed {
		t.Error("IsClosed should be false for PullRequest (not set from state)")
	}
}

func TestProbeProjectBoard_RetriesOnEmptyResponse(t *testing.T) {
	callCount := 0
	t1 := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			// First two attempts: degraded (0 items, totalCount=0).
			resp := probeResponse("user", "PVT_123", []interface{}{}, 0, false, "")
			json.NewEncoder(w).Encode(resp)
			return
		}
		// Third attempt: indexer is back.
		node := probeItemResponse("PVTI_1", "I_abc", 42, "Issue", "OPEN", t1, t1, "owner/repo", 0, "")
		resp := probeResponse("user", "PVT_123", []interface{}{node}, 1, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if callCount != 3 {
		t.Errorf("expected 3 API calls (2 empty + 1 success), got %d", callCount)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item after retry, got %d", len(items))
	}
}

func TestProbeProjectBoard_AcceptsGenuinelyEmpty(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		resp := probeResponse("user", "PVT_123", []interface{}{}, 0, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, projectID, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if projectID != "PVT_123" {
		t.Errorf("projectID = %q, want PVT_123", projectID)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items for genuinely empty board, got %d", len(items))
	}
	if callCount != projectBoardFetchAttempts {
		t.Errorf("expected %d attempts for all-empty board, got %d", projectBoardFetchAttempts, callCount)
	}
}

func TestProbeProjectBoard_OrgUserFallback(t *testing.T) {
	callCount := 0
	t1 := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var reqBody struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&reqBody)

		// Determine if this is an org or user query.
		// When the query uses "organization", return an error response.
		// When the query uses "user", return success.
		if callCount == 1 {
			// First attempt: org — return error (missing data key).
			json.NewEncoder(w).Encode(map[string]interface{}{
				"errors": []interface{}{
					map[string]interface{}{"message": "Could not resolve to an Organization"},
				},
			})
			return
		}
		// Second attempt: user — success.
		node := probeItemResponse("PVTI_1", "I_abc", 5, "Issue", "OPEN", t1, t1, "owner/repo", 0, "")
		resp := probeResponse("user", "PVT_123", []interface{}{node}, 1, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	// ownerType="" triggers org-then-user fallback.
	items, projectID, err := c.ProbeProjectBoard("owner", "repo", 1, "")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if projectID != "PVT_123" {
		t.Errorf("projectID = %q, want PVT_123", projectID)
	}
	if len(items) != 1 {
		t.Errorf("expected 1 item after org->user fallback, got %d", len(items))
	}
}

func TestProbeProjectBoard_Pagination(t *testing.T) {
	callCount := 0
	t1 := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var reqBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&reqBody)
		vars, _ := reqBody["variables"].(map[string]interface{})
		cursor, _ := vars["cursor"].(string)

		if cursor == "" {
			// First page: 1 item, hasNextPage=true.
			node := probeItemResponse("PVTI_1", "I_001", 1, "Issue", "OPEN", t1, t1, "owner/repo", 0, "")
			resp := probeResponse("user", "PVT_123", []interface{}{node}, 2, true, "cursor_abc")
			json.NewEncoder(w).Encode(resp)
		} else {
			// Second page: 1 item, hasNextPage=false.
			node := probeItemResponse("PVTI_2", "I_002", 2, "Issue", "OPEN", t1, t1, "owner/repo", 0, "")
			resp := probeResponse("user", "PVT_123", []interface{}{node}, 2, false, "")
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d, want 2 (paginated)", len(items))
	}
	if items[0].Number != 1 || items[1].Number != 2 {
		t.Errorf("items numbers = %d, %d; want 1, 2", items[0].Number, items[1].Number)
	}
	// probeProjectBoardOnce loops via cursor — each page is a separate HTTP request.
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls for 2-page board, got %d", callCount)
	}
}

func TestProbeProjectBoard_SkipsDraftItems(t *testing.T) {
	t1 := "2024-01-01T10:00:00Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Draft item has empty content.id — should be skipped.
		draft := map[string]interface{}{
			"id":        "PVTI_draft",
			"updatedAt": t1,
			"content": map[string]interface{}{
				"__typename": "DraftIssue",
				"id":         "", // empty — marks as draft
			},
		}
		real := probeItemResponse("PVTI_real", "I_real", 7, "Issue", "OPEN", t1, t1, "owner/repo", 0, "")
		resp := probeResponse("user", "PVT_123", []interface{}{draft, real}, 2, false, "")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	items, _, err := c.ProbeProjectBoard("owner", "repo", 1, "user")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1 (draft should be skipped)", len(items))
	}
	if items[0].ItemID != "PVTI_real" {
		t.Errorf("ItemID = %q, want PVTI_real", items[0].ItemID)
	}
}
