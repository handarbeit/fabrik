package github

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

