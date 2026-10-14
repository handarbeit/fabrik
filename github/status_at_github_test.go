package github

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func statusAtItem(number int, fieldValue string) string {
	n := itoa(number)
	return `{"id":"PVTI_` + n + `","updatedAt":"2026-10-01T00:00:00Z","fieldValueByName":` + fieldValue + `,` +
		`"content":{"__typename":"Issue","id":"I_` + n + `","number":` + n + `,"title":"t","state":"OPEN",` +
		`"updatedAt":"2026-10-01T00:00:00Z","repository":{"nameWithOwner":"o/r"},"labels":{"nodes":[]}}}`
}

func serveBody(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClientWithBaseURL("token", srv.URL)
}

// TestStatusAtGitHub_ShallowAndProbe covers #2246: the Status value's own
// updatedAt is parsed on both the shallow board and probe queries; a missing,
// null or unparseable time is unknown (zero), never the epoch.
func TestStatusAtGitHub_ShallowAndProbe(t *testing.T) {
	items := []string{
		statusAtItem(1, `{"name":"Queued","updatedAt":"2026-10-14T12:00:05Z"}`),
		statusAtItem(2, `{"name":"Queued"}`),
		statusAtItem(3, `{"name":"Queued","updatedAt":"not-a-time"}`),
		statusAtItem(4, `null`),
	}
	body := milestoneBoardResponse(items...)
	want := map[int]time.Time{
		1: time.Date(2026, 10, 14, 12, 0, 5, 0, time.UTC),
	}

	c := serveBody(t, body)
	board, err := c.FetchProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatalf("FetchProjectBoard: %v", err)
	}
	for _, it := range board.Items {
		if got := it.StatusAtGitHub; !got.Equal(want[it.Number]) {
			t.Errorf("shallow #%d StatusAtGitHub = %v, want %v", it.Number, got, want[it.Number])
		}
	}

	c = serveBody(t, body)
	probe, _, err := c.ProbeProjectBoard("o", "r", 1, "organization")
	if err != nil {
		t.Fatalf("ProbeProjectBoard: %v", err)
	}
	if len(probe) != len(items) {
		t.Fatalf("probe items = %d, want %d", len(probe), len(items))
	}
	for _, it := range probe {
		if got := it.StatusAtGitHub; !got.Equal(want[it.Number]) {
			t.Errorf("probe #%d StatusAtGitHub = %v, want %v", it.Number, got, want[it.Number])
		}
	}
}

// TestStatusQueriesSelectStatusValueUpdatedAt pins that both queries select the
// Status value's own updatedAt inside the single-select fragment.
func TestStatusQueriesSelectStatusValueUpdatedAt(t *testing.T) {
	for name, q := range map[string]string{"shallow": fetchProjectBoardQueryTemplate, "probe": probeProjectBoardQueryTemplate} {
		if !strings.Contains(q, "ProjectV2ItemFieldSingleSelectValue {\n              name\n              updatedAt") {
			t.Errorf("%s query does not select the Status value's updatedAt", name)
		}
	}
}
