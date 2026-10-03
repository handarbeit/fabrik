package github

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// milestoneBoardResponse builds a one-page board response whose items each
// carry the given raw `milestone` JSON fragment (including the key, or empty
// to omit the key entirely).
func milestoneBoardResponse(items ...string) string {
	return `{"data":{"organization":{"projectV2":{"id":"PVT_1","title":"Fabrik","items":{` +
		`"totalCount":` + itoa(len(items)) + `,"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[` +
		strings.Join(items, ",") + `]}}}}}`
}

func itoa(n int) string { return strconv.Itoa(n) }

func milestoneItem(number int, typename, milestoneField string) string {
	n := strconv.Itoa(number)
	field := ""
	if milestoneField != "" {
		field = `,` + milestoneField
	}
	return `{"id":"PVTI_` + n + `","updatedAt":"2026-10-01T00:00:00Z","fieldValueByName":{"name":"Plan"},` +
		`"content":{"__typename":"` + typename + `","id":"I_` + n + `","number":` + n + `,"title":"t","state":"OPEN",` +
		`"updatedAt":"2026-10-01T00:00:00Z","repository":{"nameWithOwner":"o/r"},"labels":{"nodes":[]}` + field + `}}`
}

// TestFetchProjectBoard_Milestone covers #1967 R10's three states: a milestoned
// issue, an issue with an explicit null milestone (known-none), and a response
// that lacks the key entirely (never captured → unknown, never "none").
func TestFetchProjectBoard_Milestone(t *testing.T) {
	body := milestoneBoardResponse(
		milestoneItem(1, "Issue", `"milestone":{"title":"v1.2","number":7}`),
		milestoneItem(2, "Issue", `"milestone":null`),
		milestoneItem(3, "Issue", ``),
	)
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotQuery = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(board.Items))
	}

	if !strings.Contains(gotQuery, "milestone") {
		t.Errorf("board query does not request milestone: %s", gotQuery)
	}

	set := board.Items[0]
	if !set.MilestoneKnown || set.Milestone == nil || set.Milestone.Title != "v1.2" || set.Milestone.Number != 7 {
		t.Errorf("milestoned item = known=%v ms=%+v, want known v1.2 #7", set.MilestoneKnown, set.Milestone)
	}
	none := board.Items[1]
	if !none.MilestoneKnown || none.Milestone != nil {
		t.Errorf("null-milestone item = known=%v ms=%+v, want known-none", none.MilestoneKnown, none.Milestone)
	}
	absent := board.Items[2]
	if absent.MilestoneKnown || absent.Milestone != nil {
		t.Errorf("absent-key item = known=%v ms=%+v, want unknown", absent.MilestoneKnown, absent.Milestone)
	}
}

// TestFetchProjectBoard_MilestonePRNodeUnknown pins that PR content nodes (the
// query does not select milestone on PullRequest) stay unknown.
func TestFetchProjectBoard_MilestonePRNodeUnknown(t *testing.T) {
	body := milestoneBoardResponse(milestoneItem(1, "PullRequest", ``))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClientWithBaseURL("token", srv.URL)
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	if len(board.Items) != 1 || board.Items[0].MilestoneKnown {
		t.Fatalf("PR node milestone known = %v, want unknown", board.Items[0].MilestoneKnown)
	}
}

func TestParseMilestone(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		known bool
		title string
	}{
		{"", false, ""},
		{"null", true, ""},
		{`{"title":"a","number":3}`, true, "a"},
		{`"garbage"`, false, ""},
	} {
		m, known := parseMilestone([]byte(tc.raw))
		if known != tc.known {
			t.Errorf("parseMilestone(%q) known=%v, want %v", tc.raw, known, tc.known)
		}
		if (m != nil) != (tc.title != "") || (m != nil && m.Title != tc.title) {
			t.Errorf("parseMilestone(%q) = %+v, want title %q", tc.raw, m, tc.title)
		}
	}
}
